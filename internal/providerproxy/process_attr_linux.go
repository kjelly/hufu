//go:build linux

package providerproxy

import (
	"os/exec"
	"syscall"
)

func configureProcessAttributes(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setpgid:   true,
		Pdeathsig: syscall.SIGKILL,
	}
}

func terminateProcess(cmd *exec.Cmd) {
	// The child is placed in its own process group before it starts, so this
	// also terminates helpers it spawned while handling a provider request.
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
		_ = cmd.Process.Kill()
	}
}
