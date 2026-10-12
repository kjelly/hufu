package main

import (
	"fmt"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/kjelly/hufu/internal/config"
	"github.com/kjelly/hufu/internal/conversation"
	"github.com/kjelly/hufu/internal/team"
	tuipkg "github.com/kjelly/hufu/internal/tui"
)

// chatEventDrainTimeout bounds how long shutdown waits for the controller's
// final events after Close returned.
const chatEventDrainTimeout = 2 * time.Second

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
		Actions:       tuipkg.ChatActions{Submit: ctrl.Submit, Close: closeCtrl},
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
	// The status reporter and execution log are wired once per session, so
	// execution_trace.log is not truncated on every turn.
	display := newCoordDisplay(s.tc)
	defer display.stopTimer()

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
