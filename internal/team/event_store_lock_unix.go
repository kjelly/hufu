//go:build aix || android || darwin || dragonfly || freebsd || hurd || illumos || ios || linux || netbsd || openbsd || solaris

package team

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

func lockEventStoreFile(file *os.File) error {
	if file == nil {
		return fmt.Errorf("%w: lock file is unavailable", ErrEventStoreWriterUnavailable)
	}
	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		if err == unix.EAGAIN || err == unix.EWOULDBLOCK {
			return fmt.Errorf("%w: %v", ErrEventStoreWriterUnavailable, err)
		}
		return err
	}
	return nil
}

func unlockEventStoreFile(file *os.File) error {
	if file == nil {
		return nil
	}
	return unix.Flock(int(file.Fd()), unix.LOCK_UN)
}
