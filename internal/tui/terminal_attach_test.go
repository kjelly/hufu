package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/kjelly/hufu/internal/team"
)

func TestDetailTerminalAttachStartsProcessCommand(t *testing.T) {
	m := New("test", TeamInfo{HufuBinary: "hufu", Workspace: "/tmp/workspace"})
	m.tasks = []*team.TodoItem{{ID: "task-1", Status: team.TaskInProgress}}
	m.inDetail = true
	m.detailID = "task-1"
	m.terminals["task-1"] = "term-1"

	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("t")})
	model := updated.(Model)
	if cmd == nil {
		t.Fatal("attach key did not return a process command")
	}
	if strings.Contains(model.statusText, "unavailable") || strings.Contains(model.statusText, "disabled") {
		t.Fatalf("unexpected attach status: %q", model.statusText)
	}
}

func TestDetailTerminalAttachExplainsMissingSession(t *testing.T) {
	m := New("test", TeamInfo{HufuBinary: "hufu", Workspace: "/tmp/workspace"})
	m.tasks = []*team.TodoItem{{ID: "task-1", Status: team.TaskInProgress}}
	m.inDetail = true
	m.detailID = "task-1"
	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("t")})
	if cmd != nil {
		t.Fatal("attach should not start without a PTY session")
	}
	if !strings.Contains(updated.(Model).statusText, "no PTY terminal session") {
		t.Fatalf("status = %q", updated.(Model).statusText)
	}
}
