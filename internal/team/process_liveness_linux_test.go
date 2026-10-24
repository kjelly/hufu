//go:build linux

package team

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// Cleanup assertions care whether a process can execute or retain resources.
// kill(pid, 0) also succeeds for zombies left behind by a non-reaping PID 1.
// Keep this distinction test-only: runtime identity checks have other callers.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	state, _, err := readTestProcessStat(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err == nil {
		return state != "Z"
	}
	if os.IsNotExist(err) || errors.Is(err, syscall.ESRCH) {
		return false
	}
	// Inaccessible or malformed proc data must not hide a possible leak.
	return !errors.Is(syscall.Kill(pid, 0), syscall.ESRCH)
}

func processGroupAlive(pgid int) bool {
	if pgid <= 0 {
		return false
	}
	return testProcessGroupAliveInProc("/proc", pgid)
}

func testProcessGroupAliveInProc(procRoot string, pgid int) bool {
	entries, err := os.ReadDir(procRoot)
	if err != nil {
		return true // Failure to observe the group is not proof of cleanup.
	}
	for _, entry := range entries {
		if _, err := strconv.Atoi(entry.Name()); err != nil {
			continue
		}
		state, group, err := readTestProcessStat(filepath.Join(procRoot, entry.Name(), "stat"))
		if os.IsNotExist(err) || errors.Is(err, syscall.ESRCH) {
			continue // A process may disappear while /proc is being scanned.
		}
		if err != nil {
			return true
		}
		// Include the leader as well as every other member of the group.
		if group == pgid && state != "Z" {
			return true
		}
	}
	return false
}

func readTestProcessStat(path string) (state string, pgid int, err error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", 0, err
	}
	// comm is parenthesized and may itself contain spaces and parentheses.
	lastParen := strings.LastIndex(string(data), ")")
	if lastParen < 0 {
		return "", 0, fmt.Errorf("invalid proc stat in %s", path)
	}
	fields := strings.Fields(string(data)[lastParen+1:])
	if len(fields) < 3 || len(fields[0]) != 1 {
		return "", 0, fmt.Errorf("short or invalid proc stat in %s", path)
	}
	pgid, err = strconv.Atoi(fields[2])
	if err != nil {
		return "", 0, err
	}
	return fields[0], pgid, nil
}
