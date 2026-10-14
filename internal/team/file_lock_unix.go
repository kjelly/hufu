//go:build aix || android || darwin || dragonfly || freebsd || hurd || illumos || ios || linux || netbsd || openbsd || solaris

package team

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// lockExclusiveFile takes a non-blocking exclusive lock on file. The kernel
// releases it when the process exits, so a crash never strands an owner.
func lockExclusiveFile(file *os.File) error {
	if file == nil {
		return fmt.Errorf("%w: lock file is unavailable", errFileLockHeld)
	}
	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		if err == unix.EAGAIN || err == unix.EWOULDBLOCK {
			return fmt.Errorf("%w: %v", errFileLockHeld, err)
		}
		return err
	}
	return nil
}

func unlockExclusiveFile(file *os.File) error {
	if file == nil {
		return nil
	}
	return unix.Flock(int(file.Fd()), unix.LOCK_UN)
}
