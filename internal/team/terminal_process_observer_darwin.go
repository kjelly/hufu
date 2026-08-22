//go:build darwin

package team

import (
	"bytes"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

// Darwin does not expose Waitid through x/sys. ps observes the zombie state
// without consuming it; cmd.Wait remains the sole reaper.
func observeTerminalLeader(pid int) (terminalLeaderObservation, error) {
	if pid <= 0 {
		return terminalLeaderUnknown, fmt.Errorf("invalid terminal leader PID %d", pid)
	}
	var out bytes.Buffer
	cmd := exec.Command("ps", "-o", "state=", "-p", strconv.Itoa(pid))
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return terminalLeaderUnknown, err
	}
	state := strings.TrimSpace(out.String())
	if state == "" {
		return terminalLeaderUnknown, fmt.Errorf("ps returned no state for terminal leader %d", pid)
	}
	if strings.HasPrefix(state, "Z") {
		return terminalLeaderExited, nil
	}
	return terminalLeaderRunning, nil
}

func terminalProcessGroupContained(pgid, leaderPID int) (bool, error) {
	if pgid <= 0 || leaderPID <= 0 {
		return false, fmt.Errorf("invalid terminal group identity")
	}
	output, err := exec.Command("ps", "-axo", "pid=,pgid=,state=").Output()
	if err != nil {
		return false, err
	}
	for _, line := range strings.Split(string(output), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		pid, pidErr := strconv.Atoi(fields[0])
		group, groupErr := strconv.Atoi(fields[1])
		if pidErr != nil || groupErr != nil || pid == leaderPID || group != pgid || strings.HasPrefix(fields[2], "Z") {
			continue
		}
		return false, nil
	}
	return true, nil
}
