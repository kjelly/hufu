//go:build linux

package team

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"

	"golang.org/x/sys/unix"
)

func startTerminalPTY(cmd *exec.Cmd, rows, cols uint16) (*os.File, error) {
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("open PTY master: %w", err)
	}
	cleanup := func(err error) (*os.File, error) {
		_ = master.Close()
		return nil, err
	}
	if err := unix.IoctlSetPointerInt(int(master.Fd()), unix.TIOCSPTLCK, 0); err != nil {
		return cleanup(fmt.Errorf("unlock PTY slave: %w", err))
	}
	n, err := unix.IoctlGetInt(int(master.Fd()), unix.TIOCGPTN)
	if err != nil {
		return cleanup(fmt.Errorf("get PTY slave number: %w", err))
	}
	slave, err := os.OpenFile(fmt.Sprintf("/dev/pts/%d", n), os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		return cleanup(fmt.Errorf("open PTY slave: %w", err))
	}
	if rows == 0 {
		rows = 24
	}
	if cols == 0 {
		cols = 80
	}
	if err := unix.IoctlSetWinsize(int(master.Fd()), unix.TIOCSWINSZ, &unix.Winsize{Row: rows, Col: cols}); err != nil {
		_ = slave.Close()
		return cleanup(fmt.Errorf("set PTY size: %w", err))
	}
	cmd.Stdin = slave
	cmd.Stdout = slave
	cmd.Stderr = slave
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
	if err := cmd.Start(); err != nil {
		_ = slave.Close()
		return cleanup(fmt.Errorf("start PTY command: %w", err))
	}
	_ = slave.Close()
	return master, nil
}

func resizeTerminalPTY(master *os.File, rows, cols uint16) error {
	if err := unix.IoctlSetWinsize(int(master.Fd()), unix.TIOCSWINSZ, &unix.Winsize{Row: rows, Col: cols}); err != nil {
		return fmt.Errorf("resize PTY: %w", err)
	}
	return nil
}
