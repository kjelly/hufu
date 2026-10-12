//go:build aix || darwin || dragonfly || freebsd || illumos || netbsd || openbsd || solaris

package golangruntime

import (
	"os/exec"
	"syscall"
)

func configureProcessAttributes(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}
