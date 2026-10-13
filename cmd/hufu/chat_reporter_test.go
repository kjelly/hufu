package main

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/exp/teatest/v2"

	"github.com/kjelly/hufu/internal/conversation"
	"github.com/kjelly/hufu/internal/team"
	tuipkg "github.com/kjelly/hufu/internal/tui"
	"github.com/kjelly/hufu/internal/utils"
)

type stopRecording struct{}

// msgRecorder keeps every message the TUI reporter sends to its program.
type msgRecorder struct{ msgs []tea.Msg }

func (r msgRecorder) Init() tea.Cmd { return nil }

func (r msgRecorder) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if _, ok := msg.(stopRecording); ok {
		return r, tea.Quit
	}
	r.msgs = append(r.msgs, msg)
	return r, nil
}

func (r msgRecorder) View() tea.View { return tea.NewView("") }

// TestChatViewShowsOnlyRedactedSanitizedToolActivity follows tool activity
// from the status reporter into the chat view: redacted arguments stay
// redacted and terminal control sequences never reach the screen.
func TestChatViewShowsOnlyRedactedSanitizedToolActivity(t *testing.T) {
	secret := "ghp_" + strings.Repeat("c", 36)
	args := utils.RedactSecrets(`{"command":"curl -H 'Authorization: `+secret+`' https://example.test"}`) + "\x1b]52;c;ZXZpbA==\x07\x1b[2J"
	if strings.Contains(args, secret) {
		t.Fatal("fixture precondition: RedactSecrets left the token in place")
	}

	recorder := teatest.NewTestModel(t, msgRecorder{}, teatest.WithInitialTermSize(80, 24))
	report, stop := makeTUIReporter(recorder.GetProgram())
	report(team.StatusEvent{Type: "todos_updated", Todos: []*team.TodoItem{{ID: "a", Agent: "coder", Status: team.TaskInProgress}}})
	report(team.StatusEvent{Type: "tool_call", TodoID: "a", Agent: "coder", ToolName: "bash", ToolArgs: args})
	stop()
	recorder.Send(stopRecording{})
	recorded := recorder.FinalModel(t, teatest.WithFinalTimeout(5*time.Second)).(msgRecorder).msgs

	var runtimeCalls int
	chat := tuipkg.NewChatModel(tuipkg.ChatOptions{
		Presentation: tuipkg.Options{Theme: tuipkg.ThemeDark, DisplayPreset: tuipkg.DisplayDefault},
		Actions: tuipkg.ChatActions{Submit: func(cmd conversation.Command) conversation.Ack {
			runtimeCalls++
			return conversation.Ack{CommandID: cmd.ID, Status: conversation.AckRejected}
		}},
	})
	var model tea.Model = chat
	model, _ = model.Update(tea.WindowSizeMsg{Width: 140, Height: 30})
	model, _ = model.Update(tuipkg.ChatEventMsg{Event: conversation.TurnStarted{TurnID: "turn-1"}})
	for _, msg := range recorded {
		model, _ = model.Update(msg)
	}
	view := model.View().Content
	if strings.Contains(view, secret) {
		t.Fatal("a credential reached the chat view")
	}
	if strings.Contains(view, "\x1b]52") || strings.Contains(view, "\x1b[2J") {
		t.Fatalf("a terminal control sequence reached the chat view: %q", view)
	}
	if !strings.Contains(view, "bash") {
		t.Fatalf("tool activity missing from the progress card:\n%s", view)
	}
	if runtimeCalls != 0 {
		t.Fatalf("displaying tool activity called the runtime %d times", runtimeCalls)
	}
}
