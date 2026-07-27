//go:build linux

package team

import (
	"fmt"
	"net"
	"os"

	"golang.org/x/sys/unix"
)

func verifyTerminalBrokerPeer(conn *net.UnixConn) error {
	raw, err := conn.SyscallConn()
	if err != nil {
		return fmt.Errorf("get terminal broker peer socket: %w", err)
	}
	var verifyErr error
	if err := raw.Control(func(fd uintptr) {
		cred, err := unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
		if err != nil {
			verifyErr = fmt.Errorf("read terminal broker peer credentials: %w", err)
			return
		}
		if cred.Uid != uint32(os.Getuid()) {
			verifyErr = fmt.Errorf("terminal broker peer UID %d is not current UID %d", cred.Uid, os.Getuid())
		}
	}); err != nil {
		return fmt.Errorf("control terminal broker socket: %w", err)
	}
	return verifyErr
}
