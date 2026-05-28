package tui

import (
	"strings"
	"testing"

	"github.com/anomalyco/hufu/internal/team"
	tea "github.com/charmbracelet/bubbletea"
)

// setupSpeckitModel creates a model with diverse tasks for testing navigation.
func setupSpeckitModel() Model {
	m := New("speckit test", TeamInfo{TeamName: "test-team"})
	m.width = 100
	m.height = 40
	m.tasks = []*team.TodoItem{
		{ID: "t1", Status: team.TaskPending, Desc: "Pending Task", Agent: "A1"},
		{ID: "t2", Status: team.TaskInProgress, Desc: "Working Task", Agent: "A2"},
		{ID: "t3", Status: team.TaskDone, Desc: "Finished Task", Agent: "A3"},
	}
	return m
}

func TestSpeckit_Navigation(t *testing.T) {
	m := setupSpeckitModel()

	// 1. Test Horizontal (l: Right)
	m1, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("l")})
	curr := m1.(Model)
	if curr.col != 1 {
		t.Errorf("Expected col 1 after 'l', got %d", curr.col)
	}

	// 2. Test Horizontal (h: Left)
	m2, _ := curr.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("h")})
	curr = m2.(Model)
	if curr.col != 0 {
		t.Errorf("Expected col 0 after 'h', got %d", curr.col)
	}

	// 3. Test Boundary (h at col 0)
	m3, _ := curr.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("h")})
	curr = m3.(Model)
	if curr.col != 0 {
		t.Errorf("Expected col 0 (clamped) after 'h', got %d", curr.col)
	}
}

func TestSpeckit_ViewSwitching(t *testing.T) {
	m := setupSpeckitModel()

	// 1. Enter Detail View
	// Current cursor is col 0, row 0 (Pending Task "t1")
	m1, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	curr := m1.(Model)
	if !curr.inDetail {
		t.Error("Expected to be in Detail View after 'Enter'")
	}
	if curr.detailID != "t1" {
		t.Errorf("Expected detailID 't1', got %q", curr.detailID)
	}

	// 2. Exit Detail View
	m2, _ := curr.Update(tea.KeyMsg{Type: tea.KeyEsc})
	curr = m2.(Model)
	if curr.inDetail {
		t.Error("Expected to exit Detail View after 'Esc'")
	}

	// 3. Toggle Activity Log
	m3, _ := curr.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("a")})
	curr = m3.(Model)
	if !curr.inActivityLog {
		t.Error("Expected inActivityLog to be true after 'a'")
	}
}

func TestSpeckit_Search(t *testing.T) {
	m := setupSpeckitModel()

	// 1. Activate Search
	m1, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("/")})
	curr := m1.(Model)
	if !curr.inSearch {
		t.Error("Expected inSearch to be true after '/'")
	}

	// 2. Execute Search for "Finished"
	curr.searchInput.SetValue("Finished")
	m2, _ := curr.Update(tea.KeyMsg{Type: tea.KeyEnter})
	final := m2.(Model)

	if final.searchQuery != "Finished" {
		t.Errorf("Expected searchQuery 'Finished', got %q", final.searchQuery)
	}

	// 3. Verify Jump to Col 3 (DONE column)
	if final.col != 3 {
		t.Errorf("Expected search to jump to DONE column (3), got col %d", final.col)
	}

	view := final.View()
	if !strings.Contains(view, "Finished Task") {
		t.Error("View should contain the searched task")
	}
}
