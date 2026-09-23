//go:build windows

package fsutil

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/windows"
)

type fileLockHandle struct {
	file       *os.File
	overlapped windows.Overlapped
}

func tryLockExclusive(path string) (*fileLockHandle, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	handle := &fileLockHandle{file: file}
	err = windows.LockFileEx(windows.Handle(file.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &handle.overlapped)
	if err != nil {
		_ = file.Close()
		if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
			return nil, ErrWouldBlock
		}
		return nil, err
	}
	if err = file.Truncate(0); err != nil {
		_ = windows.UnlockFileEx(windows.Handle(file.Fd()), 0, 1, 0, &handle.overlapped)
		_ = file.Close()
		return nil, fmt.Errorf("truncate lock file: %w", err)
	}
	return handle, nil
}

func (h *fileLockHandle) close() error {
	if h == nil || h.file == nil {
		return nil
	}
	err := windows.UnlockFileEx(windows.Handle(h.file.Fd()), 0, 1, 0, &h.overlapped)
	err = errors.Join(err, h.file.Close())
	h.file = nil
	return err
}
