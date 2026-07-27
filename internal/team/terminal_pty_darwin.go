//go:build darwin

package team

import (
	"fmt"
	"os"
	"os/exec"

	"github.com/creack/pty"
)

func startTerminalPTY(cmd *exec.Cmd, rows, cols uint16) (*os.File, error) {
	if rows == 0 {
		rows = 24
	}
	if cols == 0 {
		cols = 80
	}
	master, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: rows, Cols: cols})
	if err != nil {
		return nil, fmt.Errorf("start PTY command: %w", err)
	}
	return master, nil
}

func resizeTerminalPTY(master *os.File, rows, cols uint16) error {
	if err := pty.Setsize(master, &pty.Winsize{Rows: rows, Cols: cols}); err != nil {
		return fmt.Errorf("resize PTY: %w", err)
	}
	return nil
}
