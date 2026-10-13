package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/kjelly/hufu/internal/config"
	"github.com/kjelly/hufu/internal/conversation"
	inspectpkg "github.com/kjelly/hufu/internal/inspect"
	"github.com/kjelly/hufu/internal/team"
	tuipkg "github.com/kjelly/hufu/internal/tui"
)

// chatEventDrainTimeout bounds how long shutdown waits for the controller's
// final events after Close returned.
const chatEventDrainTimeout = 2 * time.Second

// chatDisplay owns the status reporter of the team the chat currently
// drives; /team swaps it on the controller's actor goroutine.
type chatDisplay struct {
	mu      sync.Mutex
	current *coordDisplay
}

func (d *chatDisplay) swap(next *coordDisplay) {
	d.mu.Lock()
	previous := d.current
	d.current = next
	d.mu.Unlock()
	if previous != nil {
		previous.stopTimer()
	}
}

// runChatUI runs the chat-first TUI. The team stays loaded for the whole
// session; each prompt is one controller turn.
func (s *chatSession) runChatUI() error {
	presentation, err := resolvedTUIOptions(s.rootCtx)
	if err != nil {
		return err
	}
	gracefulTimeout, err := resolvedGracefulWrapUpTimeout(opts.gracefulWrapUpTimeout, opts.timeoutOverride, config.LoadConfig().GracefulWrapUpTimeout)
	if err != nil {
		return err
	}
	sessionCtx, cancelSession := signal.NotifyContext(s.rootCtx, os.Interrupt, syscall.SIGTERM)
	defer cancelSession()

	adapter := newChatRuntimeAdapter(s.tc, s.teamName)
	ctrl := conversation.New(adapter, conversation.Config{})
	closeCtrl := func() conversation.CloseReport { return ctrl.Close(gracefulTimeout) }
	model := tuipkg.NewChatModel(tuipkg.ChatOptions{
		Presentation:  presentation,
		TeamInfo:      buildChatTeamInfo(s.tc, s.registry),
		Actions:       chatUIActions(adapter, ctrl.Submit, closeCtrl),
		CommandPrefix: fmt.Sprintf("chat-%d", os.Getpid()),
		History:       chatHistory(s.tc.sessionData),
		LastResult:    s.tc.coordinator.LastRunResult(),
	})
	programOptions := []tea.ProgramOption{tea.WithoutSignalHandler()}
	if presentation.DisplayPreset == tuipkg.DisplayEpaper {
		programOptions = append(programOptions, tea.WithFPS(1))
	}
	program := tea.NewProgram(model, programOptions...)
	deactivate := activateTUIProgram(program)
	defer deactivate()
	// The status reporter and execution log are wired once per team, so
	// execution_trace.log is not truncated on every turn.
	display := &chatDisplay{}
	display.swap(newCoordDisplay(s.tc))
	defer display.swap(nil)
	s.installChatTeamHooks(adapter, display, program.Send)

	ctrl.Start(sessionCtx)
	forwarded := make(chan struct{})
	go func() {
		defer close(forwarded)
		for ev := range ctrl.Events() {
			program.Send(tuipkg.ChatEventMsg{Event: ev})
		}
	}()
	go func() {
		<-sessionCtx.Done()
		program.Send(tuipkg.ChatCloseRequestMsg{})
	}()

	_, runErr := program.Run()
	if report := closeCtrl(); report.TimedOut {
		active := new(atomic.Pointer[team.Coordinator])
		current, _ := adapter.Current()
		active.Store(current.coordinator)
		emergencyFinalizeCoordinator(active)
	}
	select {
	case <-forwarded:
	case <-time.After(chatEventDrainTimeout):
	}
	s.tc, s.teamName = adapter.Current()
	if runErr != nil {
		return fmt.Errorf("chat TUI: %w", runErr)
	}
	if sessionCtx.Err() != nil {
		// SIGINT/SIGTERM from outside the terminal ended the session.
		return errInterrupted{}
	}
	return nil
}

// chatUIActions binds the view's commands to the controller and to the
// team the adapter currently drives.
func chatUIActions(adapter *chatRuntimeAdapter, submit func(conversation.Command) conversation.Ack, closeCtrl func() conversation.CloseReport) tuipkg.ChatActions {
	return tuipkg.ChatActions{
		Submit: submit,
		Close:  closeCtrl,
		Cost: func(runID string) (string, error) {
			tc, _ := adapter.Current()
			return renderChatCost(tc.session.Workspace, runID)
		},
		Save: func(path string) error {
			tc, _ := adapter.Current()
			return saveChatTranscript(tc, path)
		},
	}
}

// renderChatCost renders the same read-only projection as
// `hufu inspect cost <run-id>`.
func renderChatCost(workspace, runID string) (string, error) {
	envelope, err := inspectpkg.InspectCost(context.Background(), inspectpkg.InspectQuery{Workspace: workspace, RunID: runID})
	if err != nil {
		return "", fmt.Errorf("inspecting cost of %s: %w", runID, err)
	}
	var out bytes.Buffer
	if err := renderInspectText(&out, envelope); err != nil {
		return "", fmt.Errorf("rendering cost of %s: %w", runID, err)
	}
	return out.String(), nil
}

// installChatTeamHooks implements /reset and /team for the chat TUI. Both
// run on the controller's actor goroutine while no turn is active.
func (s *chatSession) installChatTeamHooks(adapter *chatRuntimeAdapter, display *chatDisplay, send func(tea.Msg)) {
	adapter.resetHook = func(_ context.Context, tc *teamContext) error {
		tc.coordinator.ResetConversation()
		return nil
	}
	adapter.switchHook = func(ctx context.Context, current *teamContext, name string) (*teamContext, error) {
		next, err := switchChatTeamByName(ctx, name, s.registry)
		if err != nil {
			return nil, err
		}
		display.swap(newCoordDisplay(next))
		if err := current.Close(); err != nil {
			send(tuipkg.ChatNoticeMsg{Text: fmt.Sprintf("closing the previous team failed: %v", err)})
		}
		send(tuipkg.TeamInfoMsg{Info: buildChatTeamInfo(next, s.registry)})
		return next, nil
	}
}

// chatHistory converts the restored session transcript for display.
func chatHistory(data *team.SessionData) []tuipkg.ChatHistoryEntry {
	if data == nil {
		return nil
	}
	entries := make([]tuipkg.ChatHistoryEntry, 0, len(data.Entries))
	for _, entry := range data.Entries {
		entries = append(entries, tuipkg.ChatHistoryEntry{Role: entry.Role, Content: entry.Content})
	}
	return entries
}
