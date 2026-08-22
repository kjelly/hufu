//go:build linux

package team

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

// observeTerminalLeader uses WNOWAIT so the manager can establish group
// containment while the child remains waitable. cmd.Wait is the sole reaper.
func observeTerminalLeader(pid int) (terminalLeaderObservation, error) {
	if pid <= 0 {
		return terminalLeaderUnknown, fmt.Errorf("invalid terminal leader PID %d", pid)
	}
	var info unix.Siginfo
	if err := unix.Waitid(unix.P_PID, pid, &info, unix.WEXITED|unix.WNOHANG|unix.WNOWAIT, nil); err != nil {
		if err == unix.ECHILD {
			return terminalLeaderReaped, nil
		}
		return terminalLeaderUnknown, err
	}
	if info.Signo == 0 {
		return terminalLeaderRunning, nil
	}
	return terminalLeaderExited, nil
}

// terminalProcessGroupContained ignores zombies: they cannot execute or
// retain terminal resources. The leader is explicitly excluded because it is
// intentionally still waitable until containment is proven.
func terminalProcessGroupContained(pgid, leaderPID int) (bool, error) {
	if pgid <= 0 || leaderPID <= 0 {
		return false, fmt.Errorf("invalid terminal group identity")
	}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return false, err
	}
	for _, entry := range entries {
		pid, parseErr := strconv.Atoi(entry.Name())
		if parseErr != nil || pid == leaderPID {
			continue
		}
		data, readErr := os.ReadFile("/proc/" + entry.Name() + "/stat")
		if readErr != nil {
			if os.IsNotExist(readErr) || readErr == syscall.ESRCH {
				continue
			}
			return false, readErr
		}
		lastParen := strings.LastIndex(string(data), ")")
		if lastParen < 0 {
			return false, fmt.Errorf("invalid proc stat for PID %d", pid)
		}
		fields := strings.Fields(string(data)[lastParen+1:])
		if len(fields) < 3 {
			return false, fmt.Errorf("short proc stat for PID %d", pid)
		}
		group, parseErr := strconv.Atoi(fields[2])
		if parseErr != nil || group != pgid || fields[0] == "Z" {
			continue
		}
		return false, nil
	}
	return true, nil
}
