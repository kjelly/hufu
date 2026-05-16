package tui

import (
	"strings"
	"testing"

	"github.com/anomalyco/hufu/internal/team"
	tea "github.com/charmbracelet/bubbletea"
)

func setupSelectionModel() Model {
	m := New("test prompt", TeamInfo{TeamName: "test-team"})
	m.width = 100
	m.height = 40
	m.tasks = []*team.TodoItem{
		{ID: "t1", Status: team.TaskInProgress, Desc: "Working Task", Agent: "A1"},
	}
	m.inDetail = true
	m.detailID = "t1"
	m.vpReady = true
	m.vp.Width = 100
	m.vp.Height = 30
	m.logs = map[string][]string{
		"t1": {
			"line 1: first",
			"line 2: second",
			"line 3: third",
			"line 4: fourth",
			"line 5: fifth",
		},
	}
	m.vp.SetContent(m.buildDetailContent())
	return m
}

func TestUpdate_VisualMode_Activation(t *testing.T) {
	m := setupSelectionModel()
	m2, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("v")})
	result := m2.(Model)
	if !result.inVisual {
		t.Error("Expected inVisual to be true after pressing 'v'")
	}
	if result.visualStart != result.visualEnd {
		t.Errorf("Expected visualStart == visualEnd on activation, got start=%d end=%d",
			result.visualStart, result.visualEnd)
	}
}

func TestUpdate_VisualMode_ExitWithEsc(t *testing.T) {
	m := setupSelectionModel()
	m.inVisual = true
	m.visualStart = 0
	m.visualEnd = 2

	m2, _ := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	result := m2.(Model)
	if result.inVisual {
		t.Error("Expected inVisual to be false after pressing 'Esc'")
	}
	if result.visualStart != 0 || result.visualEnd != 0 {
		t.Error("Expected selection state to be reset after Esc")
	}
}

func TestUpdate_VisualMode_ExitWithV(t *testing.T) {
	m := setupSelectionModel()
	m.inVisual = true
	m.visualStart = 0
	m.visualEnd = 2

	m2, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("v")})
	result := m2.(Model)
	if result.inVisual {
		t.Error("Expected inVisual to be false after pressing 'v' again")
	}
	if result.visualStart != 0 || result.visualEnd != 0 {
		t.Error("Expected selection state to be reset after 'v' toggle")
	}
}

func TestUpdate_VisualMode_ExtendDown(t *testing.T) {
	m := setupSelectionModel()
	m2, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("v")})
	m3 := m2.(Model)
	start := m3.visualStart

	m4, _ := m3.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
	result := m4.(Model)
	if result.visualEnd <= start {
		t.Errorf("Expected visualEnd > start (%d) after 'j', got end=%d", start, result.visualEnd)
	}
}

func TestUpdate_VisualMode_ExtendUp(t *testing.T) {
	m := setupSelectionModel()
	m.inVisual = true
	m.visualStart = 3
	m.visualEnd = 3

	m2, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("k")})
	result := m2.(Model)
	if result.visualEnd >= result.visualStart {
		t.Errorf("Expected visualEnd < visualStart after 'k', got start=%d end=%d",
			result.visualStart, result.visualEnd)
	}
}

func TestUpdate_VisualMode_ArrowKeys(t *testing.T) {
	m := setupSelectionModel()
	m2, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("v")})
	m3 := m2.(Model)
	start := m3.visualStart

	m4, _ := m3.Update(tea.KeyMsg{Type: tea.KeyDown})
	result := m4.(Model)
	if result.visualEnd <= start {
		t.Errorf("Expected visualEnd > start (%d) after down arrow, got end=%d", start, result.visualEnd)
	}
}

func TestUpdate_VisualMode_BoundaryTop(t *testing.T) {
	m := setupSelectionModel()
	m.inVisual = true
	m.visualStart = 0
	m.visualEnd = 0

	for i := 0; i < 10; i++ {
		result, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("k")})
		m = result.(Model)
	}
	if m.visualEnd < 0 {
		t.Errorf("Expected visualEnd >= 0, got %d", m.visualEnd)
	}
}

func TestUpdate_VisualMode_BoundaryBottom(t *testing.T) {
	m := setupSelectionModel()
	m.inVisual = true
	m.visualStart = 0
	m.visualEnd = 0

	contentLines := len(m.logs[m.detailID])
	for i := 0; i < contentLines+10; i++ {
		result, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
		m = result.(Model)
	}
	if m.visualEnd >= contentLines {
		t.Errorf("Expected visualEnd < contentLines (%d), got %d", contentLines, m.visualEnd)
	}
}

func TestUpdate_VisualMode_CopyToClipboard(t *testing.T) {
	m := setupSelectionModel()
	m.inVisual = true
	m.visualStart = 0
	m.visualEnd = 2

	m2, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("y")})
	result := m2.(Model)

	if result.inVisual {
		t.Error("Expected inVisual to be false after pressing 'y'")
	}
	if cmd == nil {
		t.Error("Expected a command to be returned for clipboard copy")
	}
}

func TestUpdate_VisualMode_CopyExitsVisualMode(t *testing.T) {
	m := setupSelectionModel()
	m.inVisual = true

	m2, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("y")})
	result := m2.(Model)
	if result.inVisual {
		t.Error("Expected copying to exit VISUAL mode")
	}
}

func TestUpdate_VisualMode_MultiLineSelection(t *testing.T) {
	m := setupSelectionModel()
	m.visualStart = 1
	m.visualEnd = 3

	content := m.getVisualSelection()
	expected := "line 2: second\nline 3: third\nline 4: fourth"
	if content != expected {
		t.Errorf("Expected selection %q, got %q", expected, content)
	}
}

func TestUpdate_VisualMode_SelectionReversal(t *testing.T) {
	m := setupSelectionModel()
	m.inVisual = true
	m.visualStart = 2
	m.visualEnd = 2

	result, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
	m = result.(Model)
	result, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
	m = result.(Model)

	if m.visualEnd <= 2 {
		t.Errorf("Expected visualEnd > 2 after moving down, got %d", m.visualEnd)
	}

	result, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("k")})
	m = result.(Model)
	result, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("k")})
	m = result.(Model)
	result, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("k")})
	m = result.(Model)

	if m.visualEnd > m.visualStart {
		t.Errorf("Expected visualEnd <= visualStart for reverse selection, got start=%d end=%d",
			m.visualStart, m.visualEnd)
	}

	content := m.getVisualSelection()
	if !strings.Contains(content, "line 2: second") {
		t.Errorf("Expected reversed selection to still contain correct content, got %q", content)
	}
}

func TestModel_VisualMode_FooterIndicator(t *testing.T) {
	m := setupSelectionModel()
	m.inVisual = true
	m.visualStart = 0
	m.visualEnd = 2

	footer := m.footer()
	if !strings.Contains(footer, "VISUAL") {
		t.Errorf("Expected footer to contain 'VISUAL' indicator, got %q", footer)
	}
}

func TestModel_VisualMode_FooterShowsLineCount(t *testing.T) {
	m := setupSelectionModel()
	m.inVisual = true
	m.visualStart = 0
	m.visualEnd = 2

	footer := m.footer()
	if !strings.Contains(footer, "3 line") {
		t.Errorf("Expected footer to show '3 line(s)', got %q", footer)
	}
}

func TestModel_VisualMode_FooterActions(t *testing.T) {
	m := setupSelectionModel()
	m.inVisual = true
	m.visualStart = 0
	m.visualEnd = 0

	footer := m.footer()
	if !strings.Contains(footer, "y") {
		t.Error("Expected footer to show 'y' copy action")
	}
	if !strings.Contains(footer, "esc") {
		t.Error("Expected footer to show 'esc' cancel action")
	}
}

func TestModel_VisualMode_DetailFooterShowsV(t *testing.T) {
	m := setupSelectionModel()
	m.inVisual = false

	footer := m.footer()
	if !strings.Contains(footer, "v visual") {
		t.Errorf("Expected detail footer to show 'v visual' when not in visual mode, got %q", footer)
	}
}

func TestUpdate_VisualMode_FullWorkflow(t *testing.T) {
	m := setupSelectionModel()

	m2, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("v")})
	m3 := m2.(Model)
	if !m3.inVisual {
		t.Fatal("Failed to enter VISUAL mode")
	}

	m4, _ := m3.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
	m5 := m4.(Model)
	if m5.visualEnd <= m5.visualStart {
		t.Fatal("Failed to extend selection")
	}

	footer := m5.footer()
	if !strings.Contains(footer, "VISUAL") {
		t.Error("Footer should show VISUAL indicator")
	}

	m6, cmd := m5.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("y")})
	result := m6.(Model)
	if result.inVisual {
		t.Error("Should exit VISUAL mode after copy")
	}
	if cmd == nil {
		t.Error("Should return clipboard command")
	}
}

func TestUpdate_VisualMode_NotInDetailView(t *testing.T) {
	m := New("test prompt", TeamInfo{TeamName: "test-team"})
	m.width = 100
	m.height = 40
	m.inDetail = false

	m2, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("v")})
	result := m2.(Model)
	if result.inVisual {
		t.Error("Expected inVisual to be false when not in Detail View")
	}
}

func TestModel_VisualMode_HighlightRendering(t *testing.T) {
	m := setupSelectionModel()
	m.inVisual = true
	m.visualStart = 1
	m.visualEnd = 3

	content := m.buildDetailContent()
	if content == "" {
		t.Error("Expected non-empty content in visual mode")
	}

	originalContent := m.logs["t1"]
	for i, line := range originalContent {
		wrapped := wrapText(line, m.width)
		if i >= m.visualStart && i <= m.visualEnd {
			if !strings.Contains(content, visualStyle.Render(wrapped)) {
				t.Errorf("Expected line %d to be highlighted in visual mode", i)
			}
		}
	}
}

func TestModel_VisualMode_GetSelection(t *testing.T) {
	m := setupSelectionModel()

	m.visualStart = 0
	m.visualEnd = 0
	content := m.getVisualSelection()
	if content != "line 1: first" {
		t.Errorf("Expected single line selection, got %q", content)
	}

	m.visualStart = 0
	m.visualEnd = 4
	content = m.getVisualSelection()
	expected := "line 1: first\nline 2: second\nline 3: third\nline 4: fourth\nline 5: fifth"
	if content != expected {
		t.Errorf("Expected full selection, got %q", content)
	}
}

func TestModel_VisualMode_EmptyLogs(t *testing.T) {
	m := New("test prompt", TeamInfo{TeamName: "test-team"})
	m.inDetail = true
	m.detailID = "empty-task"
	m.vpReady = true

	content := m.getVisualSelection()
	if content != "" {
		t.Errorf("Expected empty selection for empty logs, got %q", content)
	}
}

func TestUpdate_VisualMode_CancelResetsSelection(t *testing.T) {
	m := setupSelectionModel()
	m.inVisual = true
	m.visualStart = 0
	m.visualEnd = 3

	content := m.getVisualSelection()
	if content == "" {
		t.Error("Expected non-empty selection before cancel")
	}

	m2, _ := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	result := m2.(Model)

	if result.visualStart != 0 || result.visualEnd != 0 {
		t.Error("Expected selection state to be reset after cancelling visual mode")
	}
}

func TestModel_VisualMode_GotoTop(t *testing.T) {
	m := setupSelectionModel()
	m.inVisual = true
	m.visualStart = 2
	m.visualEnd = 3

	m2, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("g")})
	result := m2.(Model)
	if result.visualEnd != 0 {
		t.Errorf("Expected visualEnd=0 after 'g', got %d", result.visualEnd)
	}
}

func TestModel_VisualMode_GotoBottom(t *testing.T) {
	m := setupSelectionModel()
	m.inVisual = true
	m.visualStart = 0
	m.visualEnd = 0

	m2, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("G")})
	result := m2.(Model)
	contentLines := len(m.logs["t1"])
	if result.visualEnd != contentLines-1 {
		t.Errorf("Expected visualEnd=%d after 'G', got %d", contentLines-1, result.visualEnd)
	}
}