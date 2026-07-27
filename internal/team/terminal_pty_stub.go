//go:build !linux

package team

import (
	"fmt"
	"net"
	"os"
	"os/exec"
)

func startTerminalPTY(_ *exec.Cmd, _ uint16, _ uint16) (*os.File, error) {
	return nil, fmt.Errorf("PTY terminal sessions are supported only on linux and darwin")
}

func resizeTerminalPTY(_ *os.File, _ uint16, _ uint16) error {
	return fmt.Errorf("PTY terminal sessions are supported only on linux and darwin")
}

func verifyTerminalBrokerPeer(_ *net.UnixConn) error {
	return fmt.Errorf("terminal broker peer verification is unsupported on this platform")
}

