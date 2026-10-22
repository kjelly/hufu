package improver

import (
	"bytes"
	"context"
	"crypto/rand"
	json "encoding/json/v2"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"

	"github.com/kjelly/hufu/internal/fsutil"
	"github.com/kjelly/hufu/internal/improve/paired"
)

func publishBundle(ctx context.Context, destination string, objects Objects, content bundleContent) (retErr error) {
	if destination == "" {
		return errors.New("explicit new review directory is required")
	}
	target, err := filepath.Abs(destination)
	if err != nil {
		return err
	}
	parent, err := openSourceRoot(filepath.Dir(target))
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, parent.Close()) }()
	parentInfo, err := parent.Stat(".")
	if err != nil {
		return err
	}
	if _, err := parent.Lstat(filepath.Base(target)); !errors.Is(err, os.ErrNotExist) {
		return errors.Join(err, errors.New("review destination must not exist"))
	}
	name := ".hufu-review-" + rand.Text()
	if err := parent.Mkdir(name, 0o700); err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, parent.RemoveAll(name)) }()
	root, err := parent.OpenRoot(name)
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, root.Close()) }()
	if err := writeBundleContent(ctx, root, objects, content); err != nil {
		return err
	}
	if err := syncBundleTree(ctx, root); err != nil {
		return err
	}
	// Verify the exact staged bytes before the single no-replace publication.
	if _, err := VerifyReviewBundle(ctx, filepath.Join(filepath.Dir(target), name)); err != nil {
		return err
	}
	current, err := os.Stat(filepath.Dir(target))
	if err != nil || !os.SameFile(parentInfo, current) {
		return errors.Join(err, errors.New("review parent directory identity changed"))
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := fsutil.PublishDirectoryNoReplace(filepath.Join(filepath.Dir(target), name), target); err != nil {
		return err
	}
	return syncObjectDirectory(parent)
}

func writeBundleContent(ctx context.Context, root *os.Root, objects Objects, content bundleContent) error {
	if err := root.Mkdir("objects", 0o700); err != nil {
		return err
	}
	written := map[string]bool{}
	for _, ref := range content.manifest.Objects {
		if err := ctx.Err(); err != nil {
			return err
		}
		if written[ref.Digest] {
			continue
		}
		raw, err := objects.Read(ref)
		if err != nil {
			return err
		}
		if paired.BytesDigest(raw) != ref.Digest || int64(len(raw)) != ref.SizeBytes {
			return errors.New("bundle source changed during export")
		}
		if err := writeBundleFile(root, "objects/"+ref.Digest, raw); err != nil {
			return err
		}
		written[ref.Digest] = true
	}
	for _, item := range []struct {
		name string
		tree Tree
	}{{"baseline", content.prepared.Variants["baseline"].Tree}, {"candidate", content.prepared.Variants[content.manifest.SelectedVariant].Tree}} {
		if err := root.Mkdir(item.name, 0o700); err != nil {
			return err
		}
		for _, file := range item.tree.Inventory() {
			name := filepath.Join(item.name, filepath.FromSlash(file.Path))
			if err := root.MkdirAll(filepath.Dir(name), 0o700); err != nil {
				return err
			}
			if err := writeBundleFile(root, name, item.tree.files[file.Path]); err != nil {
				return err
			}
		}
	}
	manifest, err := paired.CanonicalJSON(content.manifest)
	if err != nil {
		return err
	}
	for _, file := range []struct {
		name string
		raw  []byte
	}{{"manifest.json", manifest}, {"candidate.patch", content.patch}, {"report.md", content.markdown}} {
		if err := writeBundleFile(root, file.name, file.raw); err != nil {
			return err
		}
	}
	return nil
}

func writeBundleFile(root *os.Root, name string, raw []byte) error {
	file, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_, writeErr := file.Write(raw)
	return errors.Join(writeErr, file.Sync(), file.Close())
}

func syncBundleTree(ctx context.Context, root *os.Root) error {
	var directories []string
	if err := fs.WalkDir(root.FS(), ".", func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if entry.IsDir() {
			directories = append(directories, name)
		}
		return nil
	}); err != nil {
		return err
	}
	// Children must be durable before the directory entries pointing to them.
	slices.Reverse(directories)
	for _, name := range directories {
		dir, err := root.Open(name)
		if err != nil {
			return err
		}
		if err := errors.Join(dir.Sync(), dir.Close()); err != nil {
			return err
		}
	}
	return nil
}

// VerifyReviewBundle reconstructs the report from the bundled journal and
// canonical child evidence. It needs no original workspace or provider.
func VerifyReviewBundle(ctx context.Context, directory string) (result ReviewBundle, retErr error) {
	root, err := openSourceRoot(directory)
	if err != nil {
		return result, err
	}
	defer func() { retErr = errors.Join(retErr, root.Close()) }()
	info, err := root.Stat(".")
	if err != nil || info.Mode().Perm() != 0o700 {
		return result, errors.Join(err, errors.New("review bundle must be private"))
	}
	raw, err := readRegular(root, "manifest.json", paired.MaxTotalBytes)
	if err != nil {
		return result, err
	}
	if err := json.Unmarshal(raw, &result, json.RejectUnknownMembers(true)); err != nil {
		return result, err
	}
	if result.Kind != "paired_review_bundle" || result.SchemaVersion != bundleSchemaVersion || result.AdoptionAuthorized {
		return result, errors.New("invalid review bundle identity or adoption authority")
	}
	objectRoot, err := openObjectDirectory(root, "objects", true)
	if err != nil {
		return result, err
	}
	objects := &ObjectStore{root: objectRoot, readOnly: true}
	defer func() { retErr = errors.Join(retErr, objects.Close()) }()
	expected, err := deriveBundle(ctx, objects, result.Scope, result.ParentJournal)
	if err != nil {
		return result, err
	}
	canonical, err := paired.CanonicalJSON(expected.manifest)
	if err != nil {
		return result, err
	}
	declared, err := paired.CanonicalJSON(result)
	if err != nil || !bytes.Equal(canonical, declared) {
		return result, errors.Join(err, errors.New("bundle manifest differs from canonical evidence"))
	}
	if err := verifyBundleFiles(ctx, root, expected); err != nil {
		return result, err
	}
	return result, nil
}

func verifyBundleFiles(ctx context.Context, root *os.Root, content bundleContent) error {
	expected := map[string]BundleFile{
		"candidate.patch": content.manifest.Patch,
		"report.md":       content.manifest.Markdown,
	}
	for _, ref := range content.manifest.Objects {
		path := "objects/" + ref.Digest
		expected[path] = BundleFile{path, ref.Digest, ref.SizeBytes}
	}
	for prefix, inventory := range map[string][]File{
		"baseline/": content.manifest.BaselineInventory, "candidate/": content.manifest.CandidateInventory,
	} {
		for _, file := range inventory {
			name := prefix + file.Path
			expected[name] = BundleFile{name, file.Digest, file.Size}
		}
	}
	// Reject unexpected files, including ignored .git/config or credentials.
	// CaptureTree intentionally ignores those in source trees, so cannot serve
	// as the complete review-bundle inventory check.
	seen := map[string]bool{}
	err := fs.WalkDir(root.FS(), ".", func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		if name == "manifest.json" {
			return nil
		}
		file, exists := expected[name]
		if !exists {
			return errors.New("review bundle contains an unexpected file")
		}
		raw, err := readRegular(root, name, 2*paired.MaxTotalBytes)
		if err != nil {
			return err
		}
		if int64(len(raw)) != file.SizeBytes || paired.BytesDigest(raw) != file.SHA256 {
			return errors.New("review bundle file digest differs")
		}
		seen[name] = true
		return nil
	})
	if err == nil && len(seen) != len(expected) {
		err = errors.New("review bundle is missing files")
	}
	return err
}
