package team

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

// AttemptEntryKind is the file type an attempt manifest records. Unlike the
// effect snapshotter's WorkspaceFileState it distinguishes a regular file
// from a symlink, so a file-to-symlink change is always visible in a delta.
type AttemptEntryKind string

const (
	AttemptEntryFile    AttemptEntryKind = "file"
	AttemptEntrySymlink AttemptEntryKind = "symlink"
	// AttemptEntryOther marks a FIFO, socket, device, or other special file.
	// It can be observed but never copied or applied.
	AttemptEntryOther AttemptEntryKind = "other"
)

// AttemptManifestEntry is one path's observed state in an attempt world or
// the canonical project. SHA256 hashes the file bytes, or the link text for
// a symlink.
type AttemptManifestEntry struct {
	Path       string           `json:"path"`
	Kind       AttemptEntryKind `json:"kind"`
	SHA256     string           `json:"sha256,omitempty"`
	Size       int64            `json:"size,omitempty"`
	Mode       uint32           `json:"mode,omitempty"`
	LinkTarget string           `json:"link_target,omitempty"`
}

// sameState reports whether two observations are the same file state. A nil
// entry means the path does not exist.
func (e *AttemptManifestEntry) sameState(other *AttemptManifestEntry) bool {
	if e == nil || other == nil {
		return e == nil && other == nil
	}
	return e.Kind == other.Kind && e.SHA256 == other.SHA256 && e.Mode == other.Mode && e.LinkTarget == other.LinkTarget
}

func (e *AttemptManifestEntry) clone() *AttemptManifestEntry {
	if e == nil {
		return nil
	}
	clone := *e
	return &clone
}

// attemptManifest maps a root-relative path to its observed state.
type attemptManifest map[string]AttemptManifestEntry

func (m attemptManifest) digest() string {
	paths := make([]string, 0, len(m))
	for rel := range m {
		paths = append(paths, rel)
	}
	sort.Strings(paths)
	entries := make([]AttemptManifestEntry, 0, len(paths))
	for _, rel := range paths {
		entries = append(entries, m[rel])
	}
	encoded, _ := json.Marshal(entries)
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}

// attemptPathHasGitSegment reports whether a root-relative path passes
// through a .git entry. Such paths are Git's own state and are never part of
// an attempt world's delta.
func attemptPathHasGitSegment(rel string) bool {
	for _, segment := range strings.Split(rel, "/") {
		if segment == ".git" {
			return true
		}
	}
	return false
}

// observeAttemptEntry reads one path's state through an os.Root, without
// following a final symlink. It returns nil for a path that does not exist.
func observeAttemptEntry(root *os.Root, rel string) (*AttemptManifestEntry, error) {
	name := filepath.FromSlash(rel)
	info, err := root.Lstat(name)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("stat %s: %w", rel, err)
	}
	entry := &AttemptManifestEntry{Path: rel, Mode: uint32(info.Mode().Perm())}
	switch {
	case info.Mode().IsRegular():
		file, err := root.Open(name)
		if err != nil {
			return nil, fmt.Errorf("open %s: %w", rel, err)
		}
		defer func() { _ = file.Close() }()
		hash := sha256.New()
		size, err := io.Copy(hash, file)
		if err != nil {
			return nil, fmt.Errorf("hash %s: %w", rel, err)
		}
		entry.Kind, entry.Size, entry.SHA256 = AttemptEntryFile, size, hex.EncodeToString(hash.Sum(nil))
	case info.Mode()&fs.ModeSymlink != 0:
		target, err := root.Readlink(name)
		if err != nil {
			return nil, fmt.Errorf("read link %s: %w", rel, err)
		}
		sum := sha256.Sum256([]byte(target))
		entry.Kind, entry.LinkTarget, entry.SHA256, entry.Mode = AttemptEntrySymlink, target, hex.EncodeToString(sum[:]), 0
	case info.IsDir():
		return nil, fmt.Errorf("%s is a directory", rel)
	default:
		entry.Kind = AttemptEntryOther
	}
	return entry, nil
}

// scanAttemptRoot observes every file and symlink under root without
// following symlinks and without entering .git.
func scanAttemptRoot(ctx context.Context, rootPath string) (attemptManifest, error) {
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		return nil, fmt.Errorf("open attempt root: %w", err)
	}
	defer func() { _ = root.Close() }()
	manifest := attemptManifest{}
	err = fs.WalkDir(root.FS(), ".", func(rel string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		if entry.Name() == ".git" {
			if entry.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if entry.IsDir() {
			return nil
		}
		observed, err := observeAttemptEntry(root, rel)
		if err != nil {
			return err
		}
		if observed != nil {
			manifest[rel] = *observed
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("scan attempt root: %w", err)
	}
	return manifest, nil
}

// symlinkTargetStaysInRoot reports whether a relative link at rel resolves,
// lexically, to a path inside the root that contains it.
func symlinkTargetStaysInRoot(rel, target string) bool {
	if target == "" || path.IsAbs(target) || filepath.IsAbs(target) {
		return false
	}
	resolved := path.Clean(path.Join(path.Dir(rel), filepath.ToSlash(target)))
	return resolved != ".." && !strings.HasPrefix(resolved, "../")
}
