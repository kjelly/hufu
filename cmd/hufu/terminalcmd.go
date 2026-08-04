package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sort"
	"sync"
	"syscall"
	"time"

	"github.com/charmbracelet/x/term"
	"github.com/spf13/cobra"

	"github.com/anomalyco/hufu/internal/team"
)

var terminalWorkspace string
var terminalListJSON bool
var terminalTransferReason string
var terminalTransferAuthorization string
var terminalTransferMode string

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

type terminalListEntry struct {
	ID                  string                     `json:"id"`
	RunID               string                     `json:"run_id,omitempty"`
	OwnerTaskID         string                     `json:"owner_task_id,omitempty"`
	ControllerTaskID    string                     `json:"controller_task_id,omitempty"`
	Agent               string                     `json:"agent,omitempty"`
	State               team.TerminalSessionState  `json:"state"`
	Custodian           team.TerminalCustodian     `json:"custodian"`
	CleanupState        team.TerminalCleanupState  `json:"cleanup_state"`
	CleanupReason       team.TerminalCleanupReason `json:"cleanup_reason,omitempty"`
	CleanupRequestedAt  time.Time                  `json:"cleanup_requested_at,omitempty"`
	CleanupCompletedAt  time.Time                  `json:"cleanup_completed_at,omitempty"`
	HandoffReason       string                     `json:"handoff_reason,omitempty"`
	HandoffAuthorizedBy string                     `json:"handoff_authorized_by,omitempty"`
	HandedOffAt         time.Time                  `json:"handed_off_at,omitempty"`
	OutputRefs          []team.ArtifactRef         `json:"output_refs,omitempty"`
	Guidance            string                     `json:"guidance"`
}

var terminalListCmd = &cobra.Command{
	Use:   "list",
	Short: "List terminal sessions and their cleanup status",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		return listTerminalSessions(cmd.OutOrStdout(), terminalWorkspacePath(), terminalListJSON)
	},
}

var terminalTransferCmd = &cobra.Command{
	Use:   "transfer <session-id> <destination-task-id>",
	Short: "Operator-authorize transfer of a detached terminal session to a paused task",
	Args:  cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		if terminalTransferReason == "" || terminalTransferAuthorization == "" || terminalTransferMode == "" {
			return fmt.Errorf("transfer requires --reason, --authorization, and --mode")
		}
		mode := team.TerminalMode(terminalTransferMode)
		if mode != team.TerminalModePipe && mode != team.TerminalModePTY {
			return fmt.Errorf("transfer mode must be %q or %q", team.TerminalModePipe, team.TerminalModePTY)
		}
		attachment, err := team.DialTerminalBroker(terminalWorkspacePath())
		if err != nil {
			return err
		}
		defer func() { _ = attachment.Close() }()
		if err := attachment.Transfer(args[0], args[1], mode, terminalTransferReason, terminalTransferAuthorization); err != nil {
			return err
		}
		_, err = fmt.Fprintf(cmd.OutOrStdout(), "Transferred terminal session %s to task %s.\n", args[0], args[1])
		return err
	},
}

func listTerminalSessions(out io.Writer, workspace string, asJSON bool) error {
	sessions, err := team.LoadTerminalSessions(workspace)
	if err != nil {
		return err
	}
	entries := make([]terminalListEntry, 0, len(sessions))
	for _, session := range sessions {
		entries = append(entries, terminalListEntry{
			ID: session.ID, RunID: session.RunID, OwnerTaskID: session.OwnerTaskID, ControllerTaskID: session.ControllerTaskID, Agent: session.Agent,
			State: session.State, Custodian: session.Custodian, CleanupState: session.CleanupState,
			CleanupReason: session.CleanupReason, CleanupRequestedAt: session.CleanupRequestedAt,
			CleanupCompletedAt: session.CleanupCompletedAt, OutputRefs: session.OutputRefs,
			HandoffReason: session.HandoffReason, HandoffAuthorizedBy: session.HandoffAuthorizedBy, HandedOffAt: session.HandedOffAt,
			Guidance: terminalGuidance(session),
		})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].ID < entries[j].ID })
	if asJSON {
		return json.NewEncoder(out).Encode(entries)
	}
	if len(entries) == 0 {
		_, err := fmt.Fprintln(out, "No terminal sessions.")
		return err
	}
	for _, entry := range entries {
		if _, err := fmt.Fprintf(out, "%s  state=%s cleanup=%s custody=%s controller=%s  %s\n", entry.ID, entry.State, entry.CleanupState, entry.Custodian, entry.ControllerTaskID, entry.Guidance); err != nil {
			return err
		}
	}
	return nil
}

func terminalGuidance(session team.TerminalSession) string {
	switch session.CleanupState {
	case team.TerminalCleanupCompleted:
		return "contained; safe to retry"
	case team.TerminalCleanupManual:
		return "manual intervention required; reconcile before retry"
	}
	if session.State == team.TerminalSessionUnknown {
		return "unknown after restart; reconcile before retry"
	}
	if session.Running || session.State == team.TerminalSessionRunning {
		if session.ControllerTaskID != "" && session.ControllerTaskID != session.OwnerTaskID {
			return "active; explicitly handed off to another task"
		}
		return "active; wait for exit or close it from the controlling task"
	}
	return "exited; safe to retry"
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
	terminalListCmd.Flags().BoolVar(&terminalListJSON, "json", false, "Output machine-readable JSON")
	terminalCmd.AddCommand(terminalListCmd)
	terminalTransferCmd.Flags().StringVar(&terminalTransferReason, "reason", "", "Reason for the operator-authorized handoff")
	terminalTransferCmd.Flags().StringVar(&terminalTransferAuthorization, "authorization", "", "Operator authorization or incident reference")
	terminalTransferCmd.Flags().StringVar(&terminalTransferMode, "mode", "", "Destination-accepted terminal mode: pipe or pty")
	terminalCmd.AddCommand(terminalTransferCmd)
}
