package tui

import (
	"strings"
	"testing"

	"github.com/anomalyco/hufu/internal/team"
	tea "github.com/charmbracelet/bubbletea"
)

func TestUpdate_Navigation(t *testing.T) {
	// Initialize a basic model
	m := New("test prompt", TeamInfo{TeamName: "test-team"})
	m.width = 100
	m.height = 40
	m.tasks = []*team.TodoItem{
		{ID: "1", Status: team.TaskPending, Desc: "Task 1", Agent: "Agent 1"},
		{ID: "2", Status: team.TaskPending, Desc: "Task 2", Agent: "Agent 1"},
	}

	// 1. Test 'j' (Down)
	m2, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
	model2 := m2.(Model)
	if model2.row != 1 {
		t.Errorf("expected row 1 after 'j', got %d", model2.row)
	}

	// 2. Test 'k' (Up)
	m3, _ := model2.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("k")})
	model3 := m3.(Model)
	if model3.row != 0 {
		t.Errorf("expected row 0 after 'k', got %d", model3.row)
	}

	// 3. Test 'l' (Right - recently fixed)
	m4, _ := model3.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("l")})
	model4 := m4.(Model)
	if model4.col != 1 {
		t.Errorf("expected col 1 after 'l', got %d", model4.col)
	}

	// 4. Test 'h' (Left)
	m5, _ := model4.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("h")})
	model5 := m5.(Model)
	if model5.col != 0 {
		t.Errorf("expected col 0 after 'h', got %d", model5.col)
	}
}

func TestView_Rendering(t *testing.T) {
	m := New("test prompt", TeamInfo{TeamName: "test-team"})
	m.width = 100
	m.height = 40
	m.tasks = []*team.TodoItem{
		{ID: "1", Status: team.TaskInProgress, Desc: "CRITICAL TASK", Agent: "RESEARCHER"},
	}

	// Mocking viewport ready to ensure View() attempts to render body
	m.vpReady = true
	m.vp.Width = 100
	m.vp.Height = 30

	view := m.View()
	t.Logf("DEBUG VIEW:\n%s", view)

	// Check for key elements
	tests := []string{
		"test prompt",   // Original prompt
		"RESEARCHER",    // Agent name
		"CRITICAL TASK", // Task description
	}

	for _, s := range tests {
		if !strings.Contains(view, s) {
			t.Errorf("View output missing expected string: %q", s)
		}
	}
}

func TestUpdate_Messages(t *testing.T) {
	m := New("initial prompt", TeamInfo{})

	// Test TasksUpdatedMsg
	newTasks := []*team.TodoItem{{ID: "99", Desc: "New Task"}}
	m2, _ := m.Update(TasksUpdatedMsg{Items: newTasks})
	model2 := m2.(Model)

	found := false
	for _, t := range model2.tasks {
		if t.ID == "99" {
			found = true
			break
		}
	}
	if !found {
		t.Error("TasksUpdatedMsg failed to update model tasks")
	}

	// Test StatusBarMsg
	m3, _ := model2.Update(StatusBarMsg{Text: "Operation Successful"})
	if m3.(Model).statusText != "Operation Successful" {
		t.Errorf("expected status text 'Operation Successful', got %q", m3.(Model).statusText)
	}
}

func TestUpdate_FinishedMsgConvertsStragglers(t *testing.T) {
	m := New("prompt", TeamInfo{})
	m.tasks = []*team.TodoItem{
		{ID: "1", Status: team.TaskInProgress},
		{ID: "2", Status: team.TaskVerifying},
		{ID: "3", Status: team.TaskPaused},
		{ID: "4", Status: team.TaskPending},
	}
	m.coordItem = &team.TodoItem{ID: team.CoordTodoID, Status: team.TaskVerifying}

	updated, _ := m.Update(FinishedMsg{})
	model := updated.(Model)

	if model.tasks[0].Status != team.TaskDone {
		t.Errorf("expected in-progress task to become done, got %s", model.tasks[0].Status)
	}
	if model.tasks[1].Status != team.TaskDone {
		t.Errorf("expected verifying task to become done, got %s", model.tasks[1].Status)
	}
	if model.tasks[2].Status != team.TaskDone {
		t.Errorf("expected paused task to become done, got %s", model.tasks[2].Status)
	}
	if model.tasks[3].Status != team.TaskSkipped {
		t.Errorf("expected pending task to become skipped, got %s", model.tasks[3].Status)
	}
	if model.coordItem == nil || model.coordItem.Status != team.TaskDone {
		t.Fatalf("expected coord item to become done, got %#v", model.coordItem)
	}
}
