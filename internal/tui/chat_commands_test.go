package tui

import (
	"errors"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/kjelly/hufu/internal/conversation"
	"github.com/kjelly/hufu/internal/team"
)

func TestChatRestoresHistoryOnceWithoutRunning(t *testing.T) {
	submits := 0
	m := NewChatModel(ChatOptions{
		Presentation: Options{Theme: ThemeDark},
		Actions:      ChatActions{Submit: func(conversation.Command) conversation.Ack { submits++; return conversation.Ack{} }},
		History: []ChatHistoryEntry{
			{Role: "user", Content: "old question one"},
			{Role: "assistant", Content: "old answer one"},
			{Role: "user", Content: "old question two"},
			{Role: "assistant", Content: "old answer two"},
		},
		LastResult: &team.RunResult{Outcome: team.RunOutcomeBlocked, UnresolvedTasks: []team.TaskReference{{}}},
	})
	m = chatStep(t, m, tea.WindowSizeMsg{Width: 100, Height: 40})
	view := plainView(m)
	for _, want := range []string{"old question one", "old answer one", "old question two", "old answer two", "(restored)", "unresolved work", "hufu session status"} {
		if strings.Count(view, want) < 1 {
			t.Fatalf("restored view missing %q:\n%s", want, view)
		}
	}
	if strings.Count(view, "old answer two") != 1 {
		t.Fatalf("restored message shown more than once:\n%s", view)
	}
	if submits != 0 {
		t.Fatalf("opening a restored session submitted %d commands", submits)
	}
	if m.lastResult == nil || m.lastResult.Outcome != team.RunOutcomeBlocked {
		t.Fatal("restored outcome not kept")
	}
}

func TestChatSlashCommands(t *testing.T) {
	tests := []struct {
		input       string
		wantKind    conversation.CommandKind
		wantTarget  string
		wantOverlay string
		wantView    chatViewMode
		wantNotice  string
	}{
		{input: "/help", wantOverlay: "Help"},
		{input: "/agents", wantOverlay: "@coder"},
		{input: "/skills", wantOverlay: "(no skills loaded)"},
		{input: "/config", wantOverlay: "Team: alpha"},
		{input: "/status", wantOverlay: "Outcome: no run yet"},
		{input: "/tasks", wantView: viewTasks},
		{input: "/cost", wantNotice: "no run yet"},
		{input: "/save", wantNotice: "usage: /save <path>"},
		{input: "/theme light", wantNotice: "Theme: light"},
		{input: "/theme pink", wantNotice: "pink"},
		{input: "/cancel", wantKind: conversation.CommandCancelTurn},
		{input: "/wrap", wantKind: conversation.CommandWrapUp},
		{input: "/reset", wantKind: conversation.CommandResetContext},
		{input: "/team", wantOverlay: "unavailable with --default"},
		{input: "/team ops", wantKind: conversation.CommandSwitchTeam, wantTarget: "ops"},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			m, runtime := newTestChat(t, 100, 30)
			m.composer.SetValue(tt.input)
			next, cmd := m.Update(keyPress("enter"))
			m = next.(ChatModel)
			if cmd != nil {
				if msg, ok := cmd().(chatAckMsg); ok {
					m = chatStep(t, m, msg)
				}
			}
			if tt.wantKind != "" {
				if len(runtime.commands) != 1 || runtime.commands[0].Kind != tt.wantKind || runtime.commands[0].Target != tt.wantTarget {
					t.Fatalf("commands = %+v, want one %s %q", runtime.commands, tt.wantKind, tt.wantTarget)
				}
			} else if len(runtime.commands) != 0 {
				t.Fatalf("read-only command reached the runtime: %+v", runtime.commands)
			}
			if tt.wantOverlay != "" && (m.overlay != overlayText || !strings.Contains(m.overlayTitle+"\n"+m.overlayBody, tt.wantOverlay)) {
				t.Fatalf("overlay = %v %q %q, want %q", m.overlay, m.overlayTitle, m.overlayBody, tt.wantOverlay)
			}
			if m.view != tt.wantView {
				t.Fatalf("view = %v, want %v", m.view, tt.wantView)
			}
			if tt.wantNotice != "" && !strings.Contains(m.notice, tt.wantNotice) {
				t.Fatalf("notice = %q, want %q", m.notice, tt.wantNotice)
			}
			if m.composer.Value() != "" {
				t.Fatalf("command left the composer with %q", m.composer.Value())
			}
		})
	}
}

func TestChatCostAndSaveUseActions(t *testing.T) {
	m, _ := newTestChat(t, 100, 30)
	var costRun, savedPath string
	m.actions.Cost = func(runID string) (string, error) { costRun = runID; return "Total: $0.01", nil }
	m.actions.Save = func(path string) error { savedPath = path; return nil }
	m = chatSubmit(t, m, "go")
	m = chatEvent(t, m, conversation.TurnStarted{TurnID: "turn-1"})
	m = chatEvent(t, m, conversation.TurnSettled{Result: conversation.TurnResult{TurnID: "turn-1", Answer: "ok", RunIDs: []string{"run-7"}, Result: &team.RunResult{RunID: "run-7", Outcome: team.RunOutcomeCompleted}}})

	m.composer.SetValue("/cost")
	next, cmd := m.Update(keyPress("enter"))
	m = chatStep(t, next.(ChatModel), cmd())
	if costRun != "run-7" || m.overlay != overlayText || !strings.Contains(m.overlayBody, "Total: $0.01") {
		t.Fatalf("cost run=%q overlay=%v body=%q", costRun, m.overlay, m.overlayBody)
	}
	m = chatStep(t, m, keyPress("esc"))

	m.composer.SetValue("/save /tmp/chat.md")
	next, cmd = m.Update(keyPress("enter"))
	m = chatStep(t, next.(ChatModel), cmd())
	if savedPath != "/tmp/chat.md" || !strings.Contains(m.notice, "Saved conversation to /tmp/chat.md") {
		t.Fatalf("save path=%q notice=%q", savedPath, m.notice)
	}

	m.actions.Save = func(string) error { return errors.New("disk full") }
	m.composer.SetValue("/save /tmp/x.md")
	next, cmd = m.Update(keyPress("enter"))
	m = chatStep(t, next.(ChatModel), cmd())
	if !strings.Contains(m.notice, "disk full") {
		t.Fatalf("save failure notice = %q", m.notice)
	}

	m.composer.SetValue("/status")
	m = chatStep(t, m, keyPress("enter"))
	if !strings.Contains(m.overlayBody, "run-7") || !strings.Contains(m.overlayBody, "Execution completed") {
		t.Fatalf("status = %q", m.overlayBody)
	}
}

func TestChatControlResults(t *testing.T) {
	m, _ := newTestChat(t, 100, 30)
	m = chatEvent(t, m, conversation.ControlDone{Kind: conversation.CommandResetContext})
	m = chatEvent(t, m, conversation.ControlDone{Kind: conversation.CommandSwitchTeam, Target: "ops"})
	m = chatEvent(t, m, conversation.ControlDone{Kind: conversation.CommandSwitchTeam, Target: "nope", Err: errors.New(`team "nope" not found`)})
	m = chatStep(t, m, ChatNoticeMsg{Text: "closing the previous team failed: boom"})
	view := plainView(m)
	for _, want := range []string{"Conversation context cleared.", "Switched to team ops.", `/team failed: team "nope" not found`, "closing the previous team failed: boom"} {
		if !strings.Contains(view, want) {
			t.Fatalf("view missing %q:\n%s", want, view)
		}
	}
}

func TestChatPaletteInsertsWithoutRunning(t *testing.T) {
	m, runtime := newTestChat(t, 100, 30)
	m = chatStep(t, m, keyPress("ctrl+p"))
	if m.overlay != overlayPalette {
		t.Fatal("Ctrl+P did not open the palette")
	}
	for _, r := range "cos" {
		m = chatStep(t, m, keyPress(string(r)))
	}
	matches := m.paletteMatches()
	if len(matches) == 0 || matches[0].name != "cost" {
		t.Fatalf("filtered matches = %+v, want cost first", matches)
	}
	m = chatStep(t, m, keyPress("enter"))
	if m.overlay != overlayNone || m.composer.Value() != "/cost " || m.focus != focusComposer {
		t.Fatalf("palette enter: overlay=%v composer=%q focus=%v", m.overlay, m.composer.Value(), m.focus)
	}
	if len(runtime.commands) != 0 || m.notice != "" {
		t.Fatalf("palette ran a command: commands=%+v notice=%q", runtime.commands, m.notice)
	}
	m = chatStep(t, m, keyPress("ctrl+p"))
	m = chatStep(t, m, keyPress("esc"))
	if m.overlay != overlayNone {
		t.Fatal("esc did not close the palette")
	}
}
