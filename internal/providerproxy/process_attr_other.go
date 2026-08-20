//go:build !linux && !aix && !darwin && !dragonfly && !freebsd && !illumos && !netbsd && !openbsd && !solaris

package providerproxy

import "os/exec"

// Platforms without Unix process-group support use the direct child kill path
// implemented here. Windows is included so the package does not reference
// Unix-only syscall.Kill APIs in its shared code.
func configureProcessAttributes(cmd *exec.Cmd) {}

func terminateProcess(cmd *exec.Cmd) {
	_ = cmd.Process.Kill()
}
