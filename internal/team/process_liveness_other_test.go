//go:build !linux && !windows

package team

import (
	"errors"
	"syscall"
)

func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	return !errors.Is(syscall.Kill(pid, 0), syscall.ESRCH)
}

func processGroupAlive(pgid int) bool {
	return terminalProcessGroupAlive(pgid)
}
