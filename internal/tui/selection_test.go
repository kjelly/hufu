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

func TestMapRenderedLineToLogIndex(t *testing.T) {
	m := New("test", TeamInfo{TeamName: "test"})
	todoID := "t1"
	m.detailID = todoID
	m.logs[todoID] = []string{
		"Short line", // Fits on 1 line (width 20 -> wraps at 18)
		"Very long line that will wrap onto multiple lines definitely.", // Takes multiple lines
		"Another short line",
	}

	// width = 20. wrapText wraps at width-2 = 18.
	// "Short line" (10 chars) -> 1 line
	// "Very long line that will wrap onto multiple lines definitely." (61 chars)
	// wrapped into chunks of max 18 chars:
	// "Very long line" (14 chars)
	// "that will wrap" (14 chars)
	// "onto multiple" (13 chars)
	// "lines definitely." (17 chars)
	// Total: 4 lines.
	// "Another short line" (18 chars) -> 1 line.

	tests := []struct {
		renderedLine int
		expectedIdx  int
	}{
		{renderedLine: 0, expectedIdx: 0},  // First line of "Short line" -> index 0
		{renderedLine: 1, expectedIdx: 1},  // First line of "Very long..." -> index 1
		{renderedLine: 2, expectedIdx: 1},  // Second line of "Very long..." -> index 1
		{renderedLine: 3, expectedIdx: 1},  // Third line of "Very long..." -> index 1
		{renderedLine: 4, expectedIdx: 1},  // Fourth line of "Very long..." -> index 1
		{renderedLine: 5, expectedIdx: 2},  // First line of "Another short..." -> index 2
		{renderedLine: 6, expectedIdx: 2},  // Second line of "Another short..." -> index 2
		{renderedLine: 7, expectedIdx: -1}, // Out of bounds -> index -1
	}

	for _, tt := range tests {
		got := m.mapRenderedLineToLogIndex(tt.renderedLine, 20)
		if got != tt.expectedIdx {
			t.Errorf("mapRenderedLineToLogIndex(%d, 20) = %d, want %d", tt.renderedLine, got, tt.expectedIdx)
		}
	}
}
