package versionstore

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/kjelly/hufu/internal/fsutil"
)

// LockOwner describes who holds a project lock. It is written next to the
// lock file for diagnostics only; the flock itself is the authority.
type LockOwner struct {
	PID         int       `json:"pid"`
	WorkspaceID string    `json:"workspace_id"`
	Operation   string    `json:"operation"`
	StartedAt   time.Time `json:"started_at"`
}

// ProjectLock is the project-level (subject-root) version lock, one per
// Project.StateDir. It is never waited on: TryLockProject fails fast with
// ErrVersionOperationInProgress so independent locks cannot deadlock.
type ProjectLock struct {
	lock      *fsutil.FileLock
	ownerPath string
}

// TryLockProject takes the project lock of stateDir without waiting.
func TryLockProject(stateDir string, owner LockOwner) (*ProjectLock, error) {
	if !PlatformSupported() {
		return nil, ErrUnsupportedPlatform
	}
	root, err := filepath.Abs(StorageRoot(stateDir))
	if err != nil {
		return nil, fmt.Errorf("resolve workspace version lock: %w", err)
	}
	paths := layout{root: root}
	if err = paths.ensure(); err != nil {
		return nil, err
	}
	lock, err := fsutil.TryLockExclusive(filepath.Join(root, lockFileName))
	if err != nil {
		if errors.Is(err, fsutil.ErrWouldBlock) {
			holder, ok, _ := ReadLockOwner(stateDir)
			if ok {
				return nil, fmt.Errorf("%w: held by pid %d (%s, workspace %s) since %s", ErrVersionOperationInProgress,
					holder.PID, holder.Operation, holder.WorkspaceID, holder.StartedAt.Format(time.RFC3339))
			}
			return nil, ErrVersionOperationInProgress
		}
		return nil, fmt.Errorf("acquire workspace version lock: %w", err)
	}
	if owner.PID == 0 {
		owner.PID = os.Getpid()
	}
	if owner.StartedAt.IsZero() {
		owner.StartedAt = time.Now().UTC()
	}
	data, err := json.Marshal(owner)
	if err == nil {
		err = fsutil.AtomicWriteFile(filepath.Join(root, ownerFileName), data, 0o600)
	}
	if err != nil {
		return nil, errors.Join(fmt.Errorf("record workspace version lock owner: %w", err), lock.Close())
	}
	return &ProjectLock{lock: lock, ownerPath: filepath.Join(root, ownerFileName)}, nil
}

// Close removes the owner record and releases the lock.
func (l *ProjectLock) Close() error {
	if l == nil || l.lock == nil {
		return nil
	}
	removeErr := os.Remove(l.ownerPath)
	if errors.Is(removeErr, os.ErrNotExist) {
		removeErr = nil
	}
	err := errors.Join(removeErr, l.lock.Close())
	l.lock = nil
	return err
}

// ReadLockOwner returns the recorded owner of the project lock, if any. The
// record can be stale after a crash; the lock itself is released by the OS.
func ReadLockOwner(stateDir string) (LockOwner, bool, error) {
	data, err := os.ReadFile(filepath.Join(StorageRoot(stateDir), ownerFileName))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return LockOwner{}, false, nil
		}
		return LockOwner{}, false, err
	}
	var owner LockOwner
	if err = json.Unmarshal(data, &owner); err != nil {
		return LockOwner{}, false, fmt.Errorf("decode lock owner: %w", err)
	}
	return owner, true, nil
}
