//go:build !linux && !darwin

package team

import "fmt"

func observeTerminalLeader(pid int) (terminalLeaderObservation, error) {
	return terminalLeaderUnknown, fmt.Errorf("non-reaping terminal leader observation is unsupported on this platform (pid %d)", pid)
}

func terminalProcessGroupContained(_, _ int) (bool, error) {
	return false, fmt.Errorf("terminal process-group containment observation is unsupported on this platform")
}
