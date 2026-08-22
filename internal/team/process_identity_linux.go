//go:build linux
// +build linux

package team

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
)

func getProcessIdentity(pid int) (*ProcessIdentity, error) {
	if pid <= 0 {
		return nil, fmt.Errorf("invalid pid: %d", pid)
	}
	pgid, _ := syscall.Getpgid(pid)

	statBytes, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return nil, err
	}
	statStr := string(statBytes)
	lastParen := strings.LastIndex(statStr, ")")
	if lastParen == -1 {
		return nil, fmt.Errorf("invalid proc stat format for pid %d", pid)
	}
	fields := strings.Fields(statStr[lastParen+1:])
	var startTicks int64
	if len(fields) >= 20 {
		startTicks, _ = strconv.ParseInt(fields[19], 10, 64)
	}

	execPath, _ := os.Readlink(fmt.Sprintf("/proc/%d/exe", pid))

	return &ProcessIdentity{
		PID:       pid,
		PGID:      pgid,
		StartTime: startTicks,
		ExecPath:  execPath,
	}, nil
}

// getTerminalLaunchIdentity captures only the process-group ownership needed
// for immediate launch custody. Durable identity validation remains separate.
func getTerminalLaunchIdentity(pid int) (*ProcessIdentity, error) {
	if pid <= 0 {
		return nil, fmt.Errorf("invalid pid: %d", pid)
	}
	pgid, err := syscall.Getpgid(pid)
	if err != nil {
		return nil, err
	}
	if pgid != pid {
		return nil, fmt.Errorf("leader PID %d does not own process group %d", pid, pgid)
	}
	return &ProcessIdentity{PID: pid, PGID: pgid}, nil
}

func verifyProcessIdentity(expected *ProcessIdentity) (bool, error) {
	if expected == nil || expected.PID <= 0 || expected.PGID <= 0 || expected.StartTime <= 0 {
		return false, nil
	}
	current, err := getProcessIdentity(expected.PID)
	if err != nil {
		return false, nil
	}
	if expected.PGID > 0 && current.PGID != expected.PGID {
		return false, nil
	}
	if expected.StartTime > 0 && current.StartTime > 0 && expected.StartTime != current.StartTime {
		return false, nil
	}
	if expected.ExecPath != "" && current.ExecPath != "" && expected.ExecPath != current.ExecPath {
		return false, nil
	}
	return true, nil
}
