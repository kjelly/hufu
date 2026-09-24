package tui

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"

	"github.com/kjelly/hufu/internal/team"
)

func attemptDetailModel(t *testing.T, attempts []OperatorAttemptDetail) (Model, *team.TodoItem) {
	t.Helper()
	m := NewWithOptions("prompt", TeamInfo{}, Options{
		Theme: ThemeLight, DisplayPreset: DisplayEpaper, NoColor: true, Spinner: true, Owner: true,
	})
	m.width, m.height = 160, 40
	item := &team.TodoItem{ID: "7", Agent: "coder", Desc: "write the feature", Status: team.TaskInProgress}
	m.tasks = []*team.TodoItem{item}
	updated, _ := m.Update(OperatorDetailsMsg{Attempts: attempts})
	return updated.(Model), item
}

func TestAttemptsRenderAsPlainTextInEpaper(t *testing.T) {
	m, item := attemptDetailModel(t, []OperatorAttemptDetail{
		{TaskID: "7", Attempt: 1, Target: "ollama/qwen3", Workspace: "isolated", Activity: "terminal", Status: "failed", Duration: 4 * time.Second, Tokens: 120, TokensKnown: true, FailureClass: "workspace_conflict"},
		{TaskID: "7", Attempt: 2, Target: "openai/gpt-5", Workspace: "isolated", Activity: "running", Status: "in_progress", Duration: 42 * time.Second, Fallbacks: 1},
		{TaskID: "8", Attempt: 1, Target: "other-task", Activity: "running"},
	})
	header := m.renderDetailHeader(item)
	if header != ansi.Strip(header) {
		t.Fatalf("no-color e-paper header contains escape sequences: %q", header)
	}
	for _, want := range []string{
		"─── Attempts ───",
		"#1 · ollama/qwen3 · isolated · terminal · failed · 4s · 120 tokens · fallbacks 0 · last failure workspace_conflict",
		"#2 · openai/gpt-5 · isolated · running · in_progress · 42s · tokens unknown · fallbacks 1",
	} {
		if !strings.Contains(header, want) {
			t.Fatalf("header missing %q:\n%s", want, header)
		}
	}
	if strings.Contains(header, "other-task") {
		t.Fatal("another task's attempt was shown")
	}
	for _, frame := range []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"} {
		if strings.Contains(header, frame) {
			t.Fatalf("the attempts block shows a spinner frame %q", frame)
		}
	}
}

func TestAttemptsBlockIsBounded(t *testing.T) {
	var attempts []OperatorAttemptDetail
	for i := 1; i <= attemptDisplayLimit+2; i++ {
		attempts = append(attempts, OperatorAttemptDetail{TaskID: "7", Attempt: i, Target: "ollama/qwen3", Activity: "terminal"})
	}
	m, item := attemptDetailModel(t, attempts)
	header := m.renderDetailHeader(item)
	if !strings.Contains(header, "(2 earlier attempt(s) not shown)") || strings.Contains(header, "#1 ·") || !strings.Contains(header, fmt.Sprintf("#%d ·", attemptDisplayLimit+2)) {
		t.Fatalf("bounded attempts block:\n%s", header)
	}
}

func TestTaskWithoutAttemptsHasNoAttemptsBlock(t *testing.T) {
	m, item := attemptDetailModel(t, nil)
	if strings.Contains(m.renderDetailHeader(item), "Attempts") {
		t.Fatal("a task without attempts rendered an Attempts block")
	}
}
