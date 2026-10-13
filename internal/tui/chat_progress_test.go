package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"

	"github.com/kjelly/hufu/internal/conversation"
	"github.com/kjelly/hufu/internal/team"
)

func TestProgressRowsShowTaskDescriptionAndID(t *testing.T) {
	m, _ := newTestChat(t, 100, 30)
	m = chatSubmit(t, m, "review the last commits")
	m = chatEvent(t, m, conversation.TurnStarted{TurnID: "turn-1"})
	m = chatStep(t, m, TasksUpdatedMsg{Items: []*team.TodoItem{
		{ID: "3", Agent: "reviewer", Status: team.TaskInProgress, Desc: "Review commits a1..c3 for\nsecurity issues"},
		{ID: "4", Agent: "reviewer", Status: team.TaskInProgress, Desc: "Review commits c4..f9 for regressions"},
	}})
	m = chatStep(t, m, TaskLogMsg{TodoID: "4", Line: "⟹ bash git log -5"})
	view := plainView(m)
	for _, want := range []string{"#3", "#4", "Review commits a1..c3 for security issues", "Review commits c4..f9 for regressions", "⟹ bash git log -5"} {
		if !strings.Contains(view, want) {
			t.Fatalf("progress card missing %q:\n%s", want, view)
		}
	}
}

func TestProgressRowsStayWithinNarrowWidth(t *testing.T) {
	m, _ := newTestChat(t, 40, 20)
	m = chatSubmit(t, m, "go")
	m = chatEvent(t, m, conversation.TurnStarted{TurnID: "turn-1"})
	m = chatStep(t, m, TasksUpdatedMsg{Items: []*team.TodoItem{
		{ID: "a-very-long-task-identifier", Agent: "documentation-reviewer", Status: team.TaskInProgress, Desc: strings.Repeat("long description ", 10)},
	}})
	for i, line := range strings.Split(m.View().Content, "\n") {
		if w := ansi.StringWidth(line); w > 40 {
			t.Fatalf("line %d is %d cells wide: %q", i, w, ansi.Strip(line))
		}
	}
}

func TestChatShowsElapsedTimeAndActivityWhileRunning(t *testing.T) {
	start := time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)
	m, _ := newTestChat(t, 120, 30)
	m = chatSubmit(t, m, "go")
	next, cmd := m.Update(ChatEventMsg{Event: conversation.TurnStarted{TurnID: "turn-1", StartedAt: start}})
	m = next.(ChatModel)
	if cmd == nil {
		t.Fatal("a started turn did not schedule the elapsed-time tick")
	}
	m = chatStep(t, m, StatusBarMsg{Text: "reviewer  step 3"})
	next, cmd = m.Update(chatTickMsg{turnID: "turn-1", at: start.Add(83 * time.Second)})
	m = next.(ChatModel)
	if cmd == nil {
		t.Fatal("tick chain stopped while the turn is running")
	}
	view := plainView(m)
	if !strings.Contains(view, "running 1m23s") {
		t.Fatalf("header missing elapsed time:\n%s", view)
	}
	if !strings.Contains(view, "⟳ reviewer step 3") {
		t.Fatalf("status line missing current activity:\n%s", view)
	}

	m = settle(t, m, "turn-1", "done", &team.RunResult{Outcome: team.RunOutcomeUnverified}, nil, false)
	if _, cmd := m.Update(chatTickMsg{turnID: "turn-1", at: start.Add(90 * time.Second)}); cmd != nil {
		t.Fatal("tick chain kept running after the turn settled")
	}
	view = plainView(m)
	if strings.Contains(view, "⟳ reviewer") || strings.Contains(view, "running") {
		t.Fatalf("running indicators remain after settlement:\n%s", view)
	}
}

func TestChatTitleShowsTurnDuration(t *testing.T) {
	m, _ := newTestChat(t, 100, 30)
	m = chatSubmit(t, m, "go")
	m = chatEvent(t, m, conversation.TurnStarted{TurnID: "turn-1"})
	m = chatEvent(t, m, conversation.TurnSettled{Result: conversation.TurnResult{TurnID: "turn-1", Answer: "ok", Elapsed: 90 * time.Second, Result: &team.RunResult{Outcome: team.RunOutcomeCompleted}}})
	if !strings.Contains(plainView(m), "Coordinator · completed · 1m30s") {
		t.Fatalf("assistant title missing duration:\n%s", plainView(m))
	}
}

func TestFormatElapsed(t *testing.T) {
	tests := []struct {
		in   time.Duration
		want string
	}{
		{0, "0s"}, {42 * time.Second, "42s"}, {83 * time.Second, "1m23s"}, {2*time.Hour + 5*time.Minute, "2h05m"},
	}
	for _, tt := range tests {
		if got := formatElapsed(tt.in); got != tt.want {
			t.Errorf("formatElapsed(%v) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestChatEpaperTicksRarely(t *testing.T) {
	m := NewChatModel(ChatOptions{Presentation: Options{Theme: ThemeMono, DisplayPreset: DisplayEpaper}})
	if got := m.tickInterval(); got < 10*time.Second {
		t.Fatalf("e-paper tick interval = %v, want a slow refresh", got)
	}
}
