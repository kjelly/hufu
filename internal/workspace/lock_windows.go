//go:build windows

package workspace

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/windows"
)

type workspaceFileLock struct {
	file       *os.File
	overlapped windows.Overlapped
}

func acquireWorkspaceFileLock(path string) (*workspaceFileLock, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	lock := &workspaceFileLock{file: file}
	err = windows.LockFileEx(windows.Handle(file.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &lock.overlapped)
	if err != nil {
		_ = file.Close()
		if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
			return nil, ErrBusy
		}
		return nil, err
	}
	if err = file.Truncate(0); err != nil {
		_ = windows.UnlockFileEx(windows.Handle(file.Fd()), 0, 1, 0, &lock.overlapped)
		_ = file.Close()
		return nil, fmt.Errorf("truncate lock file: %w", err)
	}
	return lock, nil
}

func (l *workspaceFileLock) close() error {
	if l == nil || l.file == nil {
		return nil
	}
	err := windows.UnlockFileEx(windows.Handle(l.file.Fd()), 0, 1, 0, &l.overlapped)
	err = errors.Join(err, l.file.Close())
	l.file = nil
	return err
}
