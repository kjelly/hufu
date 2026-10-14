//go:build !aix && !android && !darwin && !dragonfly && !freebsd && !hurd && !illumos && !ios && !linux && !netbsd && !openbsd && !solaris && !windows

package team

import (
	"fmt"
	"os"
)

func lockExclusiveFile(*os.File) error {
	return fmt.Errorf("%w: platform does not provide an interprocess file lock", errFileLockHeld)
}

func unlockExclusiveFile(*os.File) error { return nil }
