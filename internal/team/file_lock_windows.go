//go:build windows

package team

import (
	"fmt"
	"os"

	"golang.org/x/sys/windows"
)

func lockExclusiveFile(file *os.File) error {
	if file == nil {
		return fmt.Errorf("%w: lock file is unavailable", errFileLockHeld)
	}
	var overlapped windows.Overlapped
	err := windows.LockFileEx(windows.Handle(file.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &overlapped)
	if err == windows.ERROR_LOCK_VIOLATION {
		return fmt.Errorf("%w: %v", errFileLockHeld, err)
	}
	return err
}

func unlockExclusiveFile(file *os.File) error {
	if file == nil {
		return nil
	}
	var overlapped windows.Overlapped
	return windows.UnlockFileEx(windows.Handle(file.Fd()), 0, 1, 0, &overlapped)
}
