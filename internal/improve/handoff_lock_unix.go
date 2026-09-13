//go:build aix || android || darwin || dragonfly || freebsd || hurd || illumos || ios || linux || netbsd || openbsd || solaris

package improve

import (
	"os"

	"golang.org/x/sys/unix"
)

func lockHandoffFile(file *os.File) error {
	return unix.Flock(int(file.Fd()), unix.LOCK_EX)
}

func unlockHandoffFile(file *os.File) error {
	return unix.Flock(int(file.Fd()), unix.LOCK_UN)
}
