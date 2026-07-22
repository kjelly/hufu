//go:build linux
// +build linux

package tools

import (
	"os"
	"os/exec"
	"syscall"
)

func setNetNamespace(cmd *exec.Cmd) error {
	uid := os.Getuid()
	gid := os.Getgid()
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Cloneflags |= syscall.CLONE_NEWNET | syscall.CLONE_NEWUSER
	cmd.SysProcAttr.UidMappings = []syscall.SysProcIDMap{
		{ContainerID: 0, HostID: uid, Size: 1},
	}
	cmd.SysProcAttr.GidMappings = []syscall.SysProcIDMap{
		{ContainerID: 0, HostID: gid, Size: 1},
	}
	return nil
}

func SetNetNamespace(cmd *exec.Cmd) error {
	return setNetNamespace(cmd)
}
