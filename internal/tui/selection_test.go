package tui

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func TestVisualSelection_StartPoint(t *testing.T) {
	// 1. Setup Model with logs
	m := New("test", TeamInfo{TeamName: "test"})
	todoID := "t1"
	m.detailID = todoID
	m.inDetail = true
	m.logs[todoID] = []string{
		"Line 1",
		"Line 2",
		"Line 3",
		"Line 4",
		"Line 5",
	}
	m.width = 100
	m.height = 40
	m.vpReady = true
	m.vp.Height = 20

	// 2. Initial state check
	if m.cursorLine != 0 {
		t.Errorf("Expected initial cursorLine 0, got %d", m.cursorLine)
	}

	// 3. Move cursor down to Line 3 (index 2)
	m1, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
	m2, _ := m1.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
	curr := m2.(Model)
	if curr.cursorLine != 2 {
		t.Errorf("Expected cursorLine 2 after two 'j' presses, got %d", curr.cursorLine)
	}

	// 4. Enter VISUAL mode (v)
	m3, _ := curr.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("v")})
	curr = m3.(Model)
	if !curr.inVisual {
		t.Error("Expected to be in Visual mode after 'v'")
	}
	if curr.visualStart != 2 {
		t.Errorf("Expected visualStart 2 (matching cursorLine), got %d", curr.visualStart)
	}

	// 5. Move down one more line (Line 4, index 3)
	m4, _ := curr.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
	curr = m4.(Model)
	if curr.visualEnd != 3 {
		t.Errorf("Expected visualEnd 3 after 'j' in visual mode, got %d", curr.visualEnd)
	}
	
	// 6. Verify selection range is [2, 3]
	if curr.visualStart != 2 || curr.visualEnd != 3 {
		t.Errorf("Expected selection range [2, 3], got [%d, %d]", curr.visualStart, curr.visualEnd)
	}

	// 7. Test Yank (y)
	m5, _ := curr.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("y")})
	curr = m5.(Model)
	if curr.inVisual {
		t.Error("Expected to exit Visual mode after 'y'")
	}

	// 8. Test Cancellation (Esc)
	// Enter visual mode again
	m6, _ := curr.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("v")})
	curr = m6.(Model)
	if !curr.inVisual {
		t.Error("Expected to re-enter Visual mode")
	}
	m7, _ := curr.Update(tea.KeyMsg{Type: tea.KeyEsc})
	curr = m7.(Model)
	if curr.inVisual {
		t.Error("Expected to exit Visual mode after 'Esc'")
	}
}
