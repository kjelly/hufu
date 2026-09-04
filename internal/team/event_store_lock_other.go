//go:build !aix && !android && !darwin && !dragonfly && !freebsd && !hurd && !illumos && !ios && !linux && !netbsd && !openbsd && !solaris && !windows

package team

import (
	"fmt"
	"os"
)

func lockEventStoreFile(*os.File) error {
	return fmt.Errorf("%w: platform does not provide an event-store interprocess lock", ErrEventStoreWriterUnavailable)
}

func unlockEventStoreFile(*os.File) error { return nil }
