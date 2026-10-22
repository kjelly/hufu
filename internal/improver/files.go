// Package improver materializes controlled experiments and owns their durable
// orchestration. Runtime task semantics remain in package team.
package improver

import (
	"bytes"
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/kjelly/hufu/internal/improve/paired"
	"github.com/kjelly/hufu/internal/utils"
	"gopkg.in/yaml.v3"
)

type File struct {
	Path   string `json:"path"`
	Digest string `json:"digest"`
	Size   int64  `json:"size_bytes"`
}

// Tree owns captured bytes, independent of the source directory. Bytes are
// private so callers cannot accidentally change an already computed inventory.
type Tree struct {
	files map[string][]byte
}

func (t Tree) Inventory() []File {
	files := make([]File, 0, len(t.files))
	for _, name := range slices.Sorted(maps.Keys(t.files)) {
		raw := t.files[name]
		files = append(files, File{name, paired.BytesDigest(raw), int64(len(raw))})
	}
	return files
}

func (t Tree) Read(name string) ([]byte, bool) {
	value, ok := t.files[name]
	return bytes.Clone(value), ok
}

func CaptureTree(dir string) (Tree, error) {
	root, err := openSourceRoot(dir)
	if err != nil {
		return Tree{}, err
	}
	defer func() { _ = root.Close() }()
	first, err := readTree(root)
	if err != nil {
		return Tree{}, err
	}
	second, err := readTree(root)
	if err != nil {
		return Tree{}, err
	}
	if !slices.Equal(first.Inventory(), second.Inventory()) {
		return Tree{}, errors.New("source_drift: tree changed during capture")
	}
	return first, nil
}

func openSourceRoot(dir string) (*os.Root, error) {
	absolute, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	// Reject symlinks in every existing component, not just the final file.
	resolved, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return nil, err
	}
	if resolved != absolute {
		return nil, errors.New("source root contains a symlink")
	}
	return os.OpenRoot(absolute)
}

func excludedInput(name string) bool {
	base := path.Base(name)
	return slices.Contains([]string{".git", "workspace", ".ssh", ".aws", ".codex", ".env", "credentials", "credentials.json", "context.sqlite", "runtime.sqlite"}, base) ||
		strings.HasPrefix(base, ".env.")
}

func readTree(root *os.Root) (Tree, error) {
	tree := Tree{files: map[string][]byte{}}
	var total int64
	err := fs.WalkDir(root.FS(), ".", func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if name == "." {
			return nil
		}
		if excludedInput(name) {
			if entry.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("source %q: symlink not allowed", name)
		}
		if entry.IsDir() {
			return nil
		}
		if len(tree.files) >= paired.MaxFiles {
			return errors.New("source: file count limit exceeded")
		}
		raw, err := readRegular(root, name, paired.MaxDataBytes)
		if err != nil {
			return err
		}
		total += int64(len(raw))
		if total > paired.MaxTotalBytes {
			return errors.New("source: total byte limit exceeded")
		}
		tree.files[name] = raw
		return nil
	})
	return tree, err
}

func readRegular(root *os.Root, name string, limit int64) ([]byte, error) {
	if !paired.SafeRelativePath(name) {
		return nil, fmt.Errorf("source %q: invalid relative path", name)
	}
	for current := name; current != "."; current = path.Dir(current) {
		info, err := root.Lstat(filepath.FromSlash(current))
		if err != nil {
			return nil, err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("source %q: symlink not allowed", current)
		}
	}
	info, err := root.Lstat(filepath.FromSlash(name))
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > limit {
		return nil, fmt.Errorf("source %q: expected regular file of at most %d bytes", name, limit)
	}
	file, err := root.Open(filepath.FromSlash(name))
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	opened, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !os.SameFile(info, opened) {
		return nil, errors.New("source_drift: file identity changed")
	}
	raw, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, err
	}
	after, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) > limit || after.Size() != info.Size() || !after.ModTime().Equal(info.ModTime()) {
		return nil, errors.New("source_drift: file changed during read")
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	again, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, err
	}
	current, err := root.Lstat(filepath.FromSlash(name))
	if err != nil || !os.SameFile(info, current) || !bytes.Equal(raw, again) {
		return nil, errors.New("source_drift: file changed between reads")
	}
	if err := checkSanitized(name, raw); err != nil {
		return nil, fmt.Errorf("source %q: provide sanitized content before freezing", name)
	}
	return raw, nil
}

func checkSanitized(name string, raw []byte) error {
	var canonical []byte
	switch path.Ext(name) {
	case ".yaml", ".yml":
		var value any
		if err := yaml.Unmarshal(raw, &value); err != nil {
			return err
		}
		var err error
		canonical, err = paired.CanonicalJSON(value)
		if err != nil {
			return err
		}
	case ".json":
		var err error
		canonical, err = paired.CanonicalJSON(jsontext.Value(raw))
		if err != nil {
			return err
		}
	default:
		if utils.RedactSecrets(string(raw)) != string(raw) {
			return errors.New("secret detected")
		}
		return nil
	}
	redacted, err := utils.RedactJSON(canonical)
	if err != nil {
		return err
	}
	// RedactJSON uses the legacy encoder. Re-encode syntax before comparing,
	// without decoding numbers through a float64.
	var value jsontext.Value
	if err := json.Unmarshal(redacted, &value); err != nil {
		return err
	}
	redacted, err = paired.CanonicalJSON(value)
	if err != nil {
		return err
	}
	if !bytes.Equal(canonical, redacted) {
		return errors.New("secret detected")
	}
	return nil
}

// WriteNew writes captured files to a newly created directory. It cannot
// overwrite a checkout, and does not copy source permissions or executability.
func (t Tree) WriteNew(dir string) error {
	if err := os.Mkdir(dir, 0o700); err != nil {
		return err
	}
	for _, name := range slices.Sorted(maps.Keys(t.files)) {
		if !paired.SafeRelativePath(name) {
			return errors.New("invalid captured path")
		}
		target := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			return err
		}
		if err := os.WriteFile(target, t.files[name], 0o600); err != nil {
			return err
		}
	}
	return nil
}
