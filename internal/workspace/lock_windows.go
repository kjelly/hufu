//go:build windows

package workspace

import (
	"errors"

	"github.com/kjelly/hufu/internal/fsutil"
)

type workspaceFileLock struct {
	lock *fsutil.FileLock
}

func acquireWorkspaceFileLock(path string) (*workspaceFileLock, error) {
	lock, err := fsutil.TryLockExclusive(path)
	if err != nil {
		if errors.Is(err, fsutil.ErrWouldBlock) {
			return nil, ErrBusy
		}
		return nil, err
	}
	return &workspaceFileLock{lock: lock}, nil
}

func (l *workspaceFileLock) close() error {
	if l == nil || l.lock == nil {
		return nil
	}
	err := l.lock.Close()
	l.lock = nil
	return err
}
