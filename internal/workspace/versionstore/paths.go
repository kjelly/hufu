package versionstore

import (
	"fmt"
	"os"
	"path/filepath"
)

// Persistent layout under <Project.StateDir>/workspace-versions/.
const (
	storageDirName = "workspace-versions"
	dbFileName     = "workspace_versions.db"
	lockFileName   = "lock"
	ownerFileName  = "lock.owner"
	objectsDirName = "objects"
	tmpDirName     = "tmp"
	hashAlgorithm  = "sha256"
)

// StorageRoot returns the version store directory for a project state dir.
func StorageRoot(stateDir string) string {
	return filepath.Join(stateDir, storageDirName)
}

type layout struct {
	root string
}

func (l layout) dbPath() string { return filepath.Join(l.root, dbFileName) }
func (l layout) tmpDir() string { return filepath.Join(l.root, tmpDirName) }
func (l layout) objectsDir() string {
	return filepath.Join(l.root, objectsDirName)
}

// ensure creates the owner-only directory skeleton.
func (l layout) ensure() error {
	for _, dir := range []string{l.root, l.objectsDir(), l.tmpDir()} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("create version store directory %s: %w", dir, err)
		}
		if err := os.Chmod(dir, 0o700); err != nil {
			return fmt.Errorf("secure version store directory %s: %w", dir, err)
		}
	}
	return nil
}

// objectPath is objects/<kind>/sha256/<first 2>/<remaining 62>.
func (l layout) objectPath(kind ObjectKind, hash string) (string, error) {
	if !kind.valid() {
		return "", fmt.Errorf("%w: unknown object kind %q", ErrInvalidTree, kind)
	}
	if !validHash(hash) {
		return "", fmt.Errorf("%w: malformed object hash %q", ErrInvalidTree, hash)
	}
	return filepath.Join(l.objectsDir(), string(kind), hashAlgorithm, hash[:2], hash[2:]), nil
}
