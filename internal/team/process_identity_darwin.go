//go:build darwin
// +build darwin

package team

import (
	"bytes"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
)

func getProcessIdentity(pid int) (*ProcessIdentity, error) {
	if pid <= 0 {
		return nil, fmt.Errorf("invalid pid: %d", pid)
	}
	pgid, _ := syscall.Getpgid(pid)

	cmd := exec.Command("ps", "-o", "lstart=", "-p", strconv.Itoa(pid))
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return nil, err
	}
	startStr := strings.TrimSpace(out.String())
	if startStr == "" {
		return nil, fmt.Errorf("empty start time for pid %d", pid)
	}

	return &ProcessIdentity{
		PID:      pid,
		PGID:     pgid,
		StartStr: startStr,
	}, nil
}

func verifyProcessIdentity(expected *ProcessIdentity) (bool, error) {
	if expected == nil || expected.PID <= 0 || expected.PGID <= 0 || expected.StartStr == "" {
		return false, nil
	}
	current, err := getProcessIdentity(expected.PID)
	if err != nil {
		return false, nil
	}
	if expected.PGID > 0 && current.PGID != expected.PGID {
		return false, nil
	}
	if expected.StartStr != "" && current.StartStr != "" && expected.StartStr != current.StartStr {
		return false, nil
	}
	return true, nil
}
