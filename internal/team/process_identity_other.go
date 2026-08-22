//go:build !linux && !darwin
// +build !linux,!darwin

package team

import "fmt"

func getProcessIdentity(pid int) (*ProcessIdentity, error) {
	if pid <= 0 {
		return nil, fmt.Errorf("invalid pid: %d", pid)
	}
	return &ProcessIdentity{PID: pid}, nil
}

func getTerminalLaunchIdentity(pid int) (*ProcessIdentity, error) {
	if pid <= 0 {
		return nil, fmt.Errorf("invalid pid: %d", pid)
	}
	return nil, fmt.Errorf("terminal process-group custody is unsupported on this platform")
}

func verifyProcessIdentity(expected *ProcessIdentity) (bool, error) {
	// This build has no reliable process-start identity primitive. Treat every
	// restored live PID as unverifiable rather than risking PID-reuse damage.
	return false, nil
}
