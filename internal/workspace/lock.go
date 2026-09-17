package workspace

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

var ErrBusy = errors.New("workspace is busy")

type WorkspaceLocks struct {
	locks []*workspaceFileLock
}

func AcquireWorkspaceLocks(stateRoot string, workspaceIDs []string) (*WorkspaceLocks, error) {
	ids := slices.Clone(workspaceIDs)
	slices.Sort(ids)
	ids = slices.Compact(ids)
	for _, id := range ids {
		if !validWorkspaceID(id) {
			return nil, fmt.Errorf("invalid workspace lock ID %q", id)
		}
	}
	root, err := canonicalPath(stateRoot)
	if err != nil {
		return nil, fmt.Errorf("resolve workspace lock root: %w", err)
	}
	lockRoot := filepath.Join(root, "locks")
	if err = os.MkdirAll(lockRoot, 0o700); err != nil {
		return nil, fmt.Errorf("create workspace lock directory: %w", err)
	}
	if err = os.Chmod(lockRoot, 0o700); err != nil {
		return nil, fmt.Errorf("secure workspace lock directory: %w", err)
	}
	locks := &WorkspaceLocks{locks: make([]*workspaceFileLock, 0, len(ids))}
	for _, id := range ids {
		lock, lockErr := acquireWorkspaceFileLock(filepath.Join(lockRoot, id+".lock"))
		if lockErr != nil {
			_ = locks.Close()
			return nil, fmt.Errorf("lock workspace %s: %w", id, lockErr)
		}
		locks.locks = append(locks.locks, lock)
	}
	return locks, nil
}

func acquireMigrationLocks(stateRoot, source, workspaceID string) (*WorkspaceLocks, error) {
	if !validWorkspaceID(workspaceID) {
		return nil, fmt.Errorf("invalid workspace lock ID %q", workspaceID)
	}
	canonicalSource, err := canonicalPath(source)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256([]byte(canonicalSource))
	names := []string{"legacy_" + hex.EncodeToString(sum[:]), workspaceID}
	slices.Sort(names)
	root, err := canonicalPath(stateRoot)
	if err != nil {
		return nil, err
	}
	lockRoot := filepath.Join(root, "locks")
	if err = os.MkdirAll(lockRoot, 0o700); err != nil {
		return nil, fmt.Errorf("create workspace lock directory: %w", err)
	}
	locks := &WorkspaceLocks{locks: make([]*workspaceFileLock, 0, len(names))}
	for _, name := range names {
		lock, lockErr := acquireWorkspaceFileLock(filepath.Join(lockRoot, name+".lock"))
		if lockErr != nil {
			_ = locks.Close()
			return nil, fmt.Errorf("lock migration %s: %w", name, lockErr)
		}
		locks.locks = append(locks.locks, lock)
	}
	return locks, nil
}

func (l *WorkspaceLocks) Close() error {
	if l == nil {
		return nil
	}
	var result error
	for index := len(l.locks) - 1; index >= 0; index-- {
		result = errors.Join(result, l.locks[index].close())
	}
	l.locks = nil
	return result
}

func validWorkspaceID(id string) bool {
	if len(id) != len("ws_")+32 || id[:3] != "ws_" {
		return false
	}
	for _, value := range id[3:] {
		if !strings.ContainsRune("0123456789abcdef", value) {
			return false
		}
	}
	return true
}
