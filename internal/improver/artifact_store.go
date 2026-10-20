package improver

import (
	"bytes"
	"crypto/rand"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/kjelly/hufu/internal/improve/paired"
)

// ObjectStore holds immutable, already sanitized experiment inputs and
// canonical evidence. It never rewrites bytes after their digest is assigned.
// Close must follow controller/executor drain; the caller owns that lifetime.
type ObjectStore struct {
	root     *os.Root
	readOnly bool
}

// OpenObjects confines every operation to the existing workspace. Read-only
// opening neither creates directories nor changes permissions.
func OpenObjects(workspace, experiment string, readOnly bool) (*ObjectStore, error) {
	if !paired.ValidID(experiment) {
		return nil, errors.New("objects: invalid experiment")
	}
	root, err := openSourceRoot(workspace)
	if err != nil {
		return nil, err
	}
	for _, component := range []string{"improvement", "paired", experiment, "objects"} {
		next, openErr := openObjectDirectory(root, component, readOnly)
		closeErr := root.Close()
		if openErr != nil || closeErr != nil {
			if next != nil {
				_ = next.Close()
			}
			return nil, errors.Join(openErr, closeErr)
		}
		root = next
	}
	return &ObjectStore{root: root, readOnly: readOnly}, nil
}

func openObjectDirectory(parent *os.Root, name string, readOnly bool) (*os.Root, error) {
	if !readOnly {
		if err := parent.Mkdir(name, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
			return nil, err
		}
		if err := syncObjectDirectory(parent); err != nil {
			return nil, err
		}
	}
	info, err := parent.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode().Perm() != 0o700 {
		return nil, errors.New("objects: expected private directory without symlinks")
	}
	child, err := parent.OpenRoot(name)
	if err != nil {
		return nil, err
	}
	current, err := child.Stat(".")
	if err != nil || !os.SameFile(info, current) {
		_ = child.Close()
		return nil, errors.New("objects: directory identity changed")
	}
	return child, nil
}

func syncObjectDirectory(root *os.Root) error {
	dir, err := root.Open(".")
	if err != nil {
		return err
	}
	return errors.Join(dir.Sync(), dir.Close())
}

func (s *ObjectStore) Close() error { return s.root.Close() }

func (s *ObjectStore) Put(kind string, raw []byte) (paired.ArtifactRef, error) {
	ref := paired.ArtifactRef{Kind: kind, Digest: paired.BytesDigest(raw), SizeBytes: int64(len(raw))}
	if err := ref.Validate(kind); err != nil {
		return paired.ArtifactRef{}, err
	}
	if s.readOnly {
		return paired.ArtifactRef{}, errors.New("objects: read only")
	}
	// Check canonical bytes, never redact and silently change their identity.
	name := kind
	if len(bytes.TrimSpace(raw)) > 0 && (bytes.TrimSpace(raw)[0] == '{' || bytes.TrimSpace(raw)[0] == '[') {
		name += ".json"
	}
	if err := checkSanitized(name, raw); err != nil {
		return paired.ArtifactRef{}, errors.New("objects: content requires sanitization")
	}
	temp := ".pending-" + rand.Text()
	file, err := s.root.OpenFile(temp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return paired.ArtifactRef{}, err
	}
	defer func() { _ = s.root.Remove(temp) }()
	_, writeErr := file.Write(raw)
	syncErr := file.Sync()
	closeErr := file.Close()
	if err := errors.Join(writeErr, syncErr, closeErr); err != nil {
		return paired.ArtifactRef{}, err
	}
	// Link is an exclusive atomic publication, including across processes.
	if err := s.root.Link(temp, ref.Digest); err != nil && !errors.Is(err, fs.ErrExist) {
		return paired.ArtifactRef{}, err
	}
	if err := syncObjectDirectory(s.root); err != nil {
		return paired.ArtifactRef{}, err
	}
	if _, err := s.Read(ref); err != nil {
		return paired.ArtifactRef{}, err
	}
	return ref, nil
}

func (s *ObjectStore) PutJSON(kind string, value any) (paired.ArtifactRef, error) {
	raw, err := paired.CanonicalJSON(value)
	if err != nil {
		return paired.ArtifactRef{}, err
	}
	return s.Put(kind, raw)
}

func (s *ObjectStore) Read(ref paired.ArtifactRef) ([]byte, error) {
	if err := ref.Validate(ref.Kind); err != nil {
		return nil, err
	}
	info, err := s.root.Lstat(ref.Digest)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 || info.Size() != ref.SizeBytes {
		return nil, errors.New("objects: invalid mode or size")
	}
	raw, err := readRegular(s.root, filepath.ToSlash(ref.Digest), paired.MaxTotalBytes)
	if err != nil {
		return nil, err
	}
	if paired.BytesDigest(raw) != ref.Digest {
		return nil, fmt.Errorf("objects: digest mismatch for %s", ref.Kind)
	}
	return raw, nil
}
