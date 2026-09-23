package versionstore

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
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

// hashRootFile hashes a regular file inside root without following a final
// symlink.
func hashRootFile(root *os.Root, rel string) (string, int64, error) {
	file, err := openRegularNoFollow(root, filepath.FromSlash(rel))
	if err != nil {
		return "", 0, err
	}
	defer func() { _ = file.Close() }()
	hasher := sha256.New()
	n, err := io.Copy(hasher, file)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(hasher.Sum(nil)), n, nil
}

// diskMatches reports whether rel already holds exactly want, which makes a
// repeated materialization after a partial failure a no-op for that path.
func (m *materializer) diskMatches(ctx context.Context, rel string, want flatLeaf) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	info, err := m.root.Lstat(filepath.FromSlash(rel))
	if err != nil {
		// Missing, or unreachable because a parent is not a plain directory;
		// either way the path must be written, and ensureParents reports any
		// collision precisely.
		return false, nil
	}
	switch want.Kind {
	case EntryBlob:
		if !info.Mode().IsRegular() || info.Size() != want.Size || uint32(info.Mode().Perm()) != want.Mode {
			return false, nil
		}
		hash, _, hashErr := hashRootFile(m.root, rel)
		if hashErr != nil {
			return false, fmt.Errorf("materialize: hash %q: %w", rel, hashErr)
		}
		return hash == want.Hash, nil
	case EntrySymlink:
		if info.Mode()&os.ModeSymlink == 0 {
			return false, nil
		}
		return m.symlinkMatches(rel, want)
	}
	return false, nil
}

func (m *materializer) symlinkMatches(rel string, want flatLeaf) (bool, error) {
	target, err := m.store.readLink(want.Hash)
	if err != nil {
		return false, err
	}
	actual, err := m.root.Readlink(filepath.FromSlash(rel))
	if err != nil {
		return false, nil
	}
	return actual == target, nil
}

// ensureParents creates missing directories. An existing symlink or file in
// a parent position is never followed or replaced.
func (m *materializer) ensureParents(rel string) error {
	segments := strings.Split(rel, "/")
	for index := 1; index < len(segments); index++ {
		dir := strings.Join(segments[:index], "/")
		info, err := m.root.Lstat(filepath.FromSlash(dir))
		switch {
		case err == nil && info.IsDir():
			continue
		case err == nil:
			return fmt.Errorf("%w: %q is a %s where %q needs a directory", ErrMaterializationCollision, dir, info.Mode().Type(), rel)
		case errors.Is(err, os.ErrNotExist):
			if mkErr := m.root.Mkdir(filepath.FromSlash(dir), 0o755); mkErr != nil && !errors.Is(mkErr, os.ErrExist) {
				return fmt.Errorf("materialize: create %q: %w", dir, mkErr)
			}
		default:
			return fmt.Errorf("materialize: stat %q: %w", dir, err)
		}
	}
	return nil
}

// checkReplaceable refuses to overwrite anything the current snapshot does
// not manage.
func (m *materializer) checkReplaceable(rel string) error {
	info, err := m.root.Lstat(filepath.FromSlash(rel))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("materialize: stat %q: %w", rel, err)
	}
	if info.IsDir() {
		return fmt.Errorf("%w: %q is a directory with unmanaged content", ErrMaterializationCollision, rel)
	}
	if _, managed := m.plan.current[rel]; !managed {
		return fmt.Errorf("%w: %q exists but is not managed by the current snapshot", ErrMaterializationCollision, rel)
	}
	return nil
}

func materializeTempName(rel string) (string, error) {
	suffix := make([]byte, 8)
	if _, err := rand.Read(suffix); err != nil {
		return "", fmt.Errorf("materialize: temp name: %w", err)
	}
	return path.Join(path.Dir(rel), materializeTempPrefix+hex.EncodeToString(suffix)), nil
}

// writeLeaf publishes one target leaf with temp file + rename.
func (m *materializer) writeLeaf(ctx context.Context, rel string, want flatLeaf) error {
	if err := m.ensureParents(rel); err != nil {
		return err
	}
	if err := m.checkReplaceable(rel); err != nil {
		return err
	}
	tmpRel, err := materializeTempName(rel)
	if err != nil {
		return err
	}
	tmpOS, relOS := filepath.FromSlash(tmpRel), filepath.FromSlash(rel)
	switch want.Kind {
	case EntryBlob:
		err = m.writeBlobTemp(ctx, tmpOS, want)
	case EntrySymlink:
		var target string
		if target, err = m.store.readLink(want.Hash); err == nil {
			err = m.root.Symlink(target, tmpOS)
		}
	default:
		err = fmt.Errorf("%w: cannot materialize kind %s", ErrInvalidTree, want.Kind)
	}
	if err != nil {
		_ = m.root.Remove(tmpOS)
		return fmt.Errorf("materialize %q: %w", rel, err)
	}
	if err = m.root.Rename(tmpOS, relOS); err != nil {
		_ = m.root.Remove(tmpOS)
		return fmt.Errorf("materialize %q: rename: %w", rel, err)
	}
	return m.syncDir(path.Dir(rel))
}

func (m *materializer) writeBlobTemp(ctx context.Context, tmpOS string, want flatLeaf) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	src, err := m.store.openObject(ObjectBlob, want.Hash)
	if err != nil {
		return err
	}
	defer func() { _ = src.Close() }()
	dst, err := m.root.OpenFile(tmpOS, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	hasher := sha256.New()
	n, copyErr := io.Copy(io.MultiWriter(dst, hasher), src)
	if copyErr == nil && (hex.EncodeToString(hasher.Sum(nil)) != want.Hash || n != want.Size) {
		copyErr = fmt.Errorf("%w: blob %s does not match its name", ErrCASCorrupt, want.Hash)
	}
	if copyErr == nil {
		copyErr = dst.Chmod(fs.FileMode(want.Mode))
	}
	if copyErr == nil {
		copyErr = dst.Sync()
	}
	return errors.Join(copyErr, dst.Close())
}

func (m *materializer) syncDir(dir string) error {
	handle, err := m.root.Open(filepath.FromSlash(dir))
	if err != nil {
		return fmt.Errorf("materialize: open directory %q: %w", dir, err)
	}
	defer func() { _ = handle.Close() }()
	if err = handle.Sync(); err != nil {
		return fmt.Errorf("materialize: sync directory %q: %w", dir, err)
	}
	return nil
}

// deleteManaged removes one path the current snapshot manages. A path that
// has become a directory holds unmanaged content and is never removed.
func (m *materializer) deleteManaged(rel string) error {
	relOS := filepath.FromSlash(rel)
	info, err := m.root.Lstat(relOS)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("materialize: stat %q: %w", rel, err)
	}
	if info.IsDir() {
		return fmt.Errorf("%w: managed path %q is now a directory", ErrMaterializationCollision, rel)
	}
	if err = m.root.Remove(relOS); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("materialize: delete %q: %w", rel, err)
	}
	m.deleted++
	for _, dir := range parentDirs(rel) {
		m.removedParents[dir] = true
	}
	return m.syncDir(path.Dir(rel))
}

// removeEmptiedDirs removes, deepest first, directories that became empty
// because managed files were deleted from them (D6). A directory that still
// has any entry is kept.
func (m *materializer) removeEmptiedDirs() error {
	dirs := sortedBoolKeys(m.removedParents)
	sort.SliceStable(dirs, func(i, j int) bool { return strings.Count(dirs[i], "/") > strings.Count(dirs[j], "/") })
	for _, dir := range dirs {
		dirOS := filepath.FromSlash(dir)
		entries, err := fs.ReadDir(m.root.FS(), dir)
		if err != nil || len(entries) > 0 {
			continue
		}
		if err = m.root.Remove(dirOS); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("materialize: remove empty directory %q: %w", dir, err)
		}
	}
	m.removedParents = map[string]bool{}
	return nil
}

// verify is the post-materialization check (§21.5): every target path
// exists with the right kind and mode; written paths are re-hashed.
func (m *materializer) verify(ctx context.Context) error {
	written := make(map[string]bool, len(m.plan.writes))
	for _, rel := range m.plan.writes {
		written[rel] = true
	}
	for _, rel := range sortedKeys(m.plan.target) {
		if err := ctx.Err(); err != nil {
			return err
		}
		want := m.plan.target[rel]
		info, err := m.root.Lstat(filepath.FromSlash(rel))
		if err != nil {
			return fmt.Errorf("%w: %q: %v", ErrMaterializationVerifyFailed, rel, err)
		}
		switch want.Kind {
		case EntryBlob:
			if !info.Mode().IsRegular() || info.Size() != want.Size || uint32(info.Mode().Perm()) != want.Mode {
				return fmt.Errorf("%w: %q has unexpected type, size, or mode", ErrMaterializationVerifyFailed, rel)
			}
			if written[rel] {
				hash, _, hashErr := hashRootFile(m.root, rel)
				if hashErr != nil || hash != want.Hash {
					return fmt.Errorf("%w: %q content does not match", ErrMaterializationVerifyFailed, rel)
				}
			}
		case EntrySymlink:
			if info.Mode()&os.ModeSymlink == 0 {
				return fmt.Errorf("%w: %q is not a symlink", ErrMaterializationVerifyFailed, rel)
			}
			ok, linkErr := m.symlinkMatches(rel, want)
			if linkErr != nil || !ok {
				return fmt.Errorf("%w: %q symlink target does not match", ErrMaterializationVerifyFailed, rel)
			}
		}
	}
	return nil
}
