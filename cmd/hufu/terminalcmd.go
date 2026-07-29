package main

import (
	"fmt"
	"io"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/charmbracelet/x/term"
	"github.com/spf13/cobra"

	"github.com/anomalyco/hufu/internal/team"
)

var terminalWorkspace string

var terminalCmd = &cobra.Command{
	Use:   "terminal",
	Short: "Attach a local terminal to an interactive PTY session",
}

var terminalAttachCmd = &cobra.Command{
	Use:   "attach <session-id>",
	Short: "Take over an active PTY session; press Ctrl-] to return control to hufu",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return attachTerminal(os.Stdin, os.Stdout, terminalWorkspacePath(), args[0])
	},
}

func terminalWorkspacePath() string {
	if terminalWorkspace != "" {
		return terminalWorkspace
	}
	return getWorkspace()
}

func attachTerminal(in *os.File, out io.Writer, workspace, sessionID string) error {
	if !term.IsTerminal(in.Fd()) {
		return fmt.Errorf("terminal attach requires an interactive TTY")
	}
	attachment, err := team.DialTerminalBroker(workspace)
	if err != nil {
		return err
	}
	defer func() { _ = attachment.Close() }()

	state, err := term.MakeRaw(in.Fd())
	if err != nil {
		return fmt.Errorf("set terminal raw mode: %w", err)
	}
	defer func() { _ = term.Restore(in.Fd(), state) }()

	snapshot, err := attachment.Attach(sessionID)
	if err != nil {
		return err
	}
	if cols, rows, sizeErr := term.GetSize(in.Fd()); sizeErr == nil && cols > 0 && rows > 0 {
		if err := attachment.Resize(uint16(rows), uint16(cols)); err != nil {
			return err
		}
	}
	_, _ = fmt.Fprint(out, "\x1b[2J\x1b[H")
	lastScreen := ""
	renderTerminalScreen(out, snapshot.Screen, &lastScreen)
	if snapshot.EOF {
		return nil
	}

	inputDone := make(chan struct{})
	inputErr := make(chan error, 1)
	var once sync.Once
	stopInput := func() { once.Do(func() { close(inputDone) }) }
	defer stopInput()
	go readTerminalAttachmentInput(in, attachment, inputDone, inputErr, stopInput)

	ticker := time.NewTicker(75 * time.Millisecond)
	defer ticker.Stop()
	resize := make(chan os.Signal, 1)
	signal.Notify(resize, syscall.SIGWINCH)
	defer signal.Stop(resize)
	for {
		select {
		case <-inputDone:
			return nil
		case err := <-inputErr:
			return err
		case <-resize:
			if cols, rows, sizeErr := term.GetSize(in.Fd()); sizeErr == nil && cols > 0 && rows > 0 {
				if err := attachment.Resize(uint16(rows), uint16(cols)); err != nil {
					return err
				}
			}
		case <-ticker.C:
			snapshot, err := attachment.Read()
			if err != nil {
				return err
			}
			renderTerminalScreen(out, snapshot.Screen, &lastScreen)
			if snapshot.EOF {
				return nil
			}
		}
	}
}

func readTerminalAttachmentInput(in *os.File, attachment *team.TerminalAttachment, done <-chan struct{}, errs chan<- error, stop func()) {
	buf := make([]byte, 256)
	for {
		n, err := in.Read(buf)
		if n > 0 {
			for _, b := range buf[:n] {
				if b == 0x1d { // Ctrl-]
					stop()
					return
				}
				if err := attachment.Write([]byte{b}); err != nil {
					errs <- err
					return
				}
			}
		}
		if err != nil {
			if err != io.EOF {
				errs <- err
			}
			return
		}
		select {
		case <-done:
			return
		default:
		}
	}
}

func renderTerminalScreen(out io.Writer, screen string, last *string) {
	if screen == *last {
		return
	}
	_, _ = fmt.Fprintf(out, "\x1b[H\x1b[2J%s", screen)
	*last = screen
}

func init() {
	terminalCmd.PersistentFlags().StringVarP(&terminalWorkspace, "workspace", "w", "", "Workspace directory (default: <cwd>/workspace)")
	terminalCmd.AddCommand(terminalAttachCmd)
}
