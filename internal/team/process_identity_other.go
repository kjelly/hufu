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

func verifyProcessIdentity(expected *ProcessIdentity) (bool, error) {
	if expected == nil || expected.PID <= 0 {
		return false, nil
	}
	return isPIDAlive(expected.PID), nil
}
