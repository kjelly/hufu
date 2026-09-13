//go:build !aix && !android && !darwin && !dragonfly && !freebsd && !hurd && !illumos && !ios && !linux && !netbsd && !openbsd && !solaris && !windows

package improve

import "os"

// Platforms without the repository's supported advisory-lock APIs still use
// the process-wide mutex in HandoffStore.
func lockHandoffFile(*os.File) error {
	return nil
}

func unlockHandoffFile(*os.File) error {
	return nil
}
