//go:build darwin
// +build darwin

package tools

import (
	"os/exec"
)

func setNetNamespace(cmd *exec.Cmd) error {
	return nil
}

func SetNetNamespace(cmd *exec.Cmd) error {
	return setNetNamespace(cmd)
}
