package tui

import (
	"fmt"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/kjelly/hufu/internal/team"
)

func uxKey(m Model, key string) Model {
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(key)})
	return next.(Model)
}

func TestDetailSearchStaysInCurrentTask(t *testing.T) {
	m := New("task", TeamInfo{})
	m.width, m.height = 100, 30
	m.tasks = []*team.TodoItem{{ID: "a", Status: team.TaskInProgress}}
	m.detailID, m.inDetail = "a", true
	m.logs["a"] = []string{"start", "needle first", "middle", "needle second"}
	m.searchResults = []*team.TodoItem{{ID: "other"}}
	m.vpReady = true
	m.vp.Width, m.vp.Height = 100, 20
	m = uxKey(m, "/")
	m = uxKey(m, "needle")
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = next.(Model)
	if !m.inDetail || m.detailSearchActive || m.cursorLine != 1 {
		t.Fatalf("search entered wrong task or line: detail=%t cursor=%d", m.inDetail, m.cursorLine)
	}
	m = uxKey(m, "n")
	if !m.inDetail || m.cursorLine != 3 {
		t.Fatalf("next match left detail or missed second result: detail=%t cursor=%d", m.inDetail, m.cursorLine)
	}
	m = uxKey(m, "N")
	if m.cursorLine != 1 {
		t.Fatalf("previous match cursor=%d, want 1", m.cursorLine)
	}
	m = uxKey(m, "/")
	m.detailSearchInput.SetValue("absent")
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = next.(Model)
	if !m.inDetail || m.cursorLine != 1 {
		t.Fatalf("no-match search changed focus: detail=%t cursor=%d", m.inDetail, m.cursorLine)
	}
	next, _ = m.Update(TaskLogMsg{TodoID: "a", Line: "new output"})
	m = next.(Model)
	next, _ = m.Update(detailRefreshMsg{})
	m = next.(Model)
	if m.cursorLine != 1 {
		t.Fatalf("new log line stole search focus: cursor=%d", m.cursorLine)
	}
}

func TestAskUserCustomEscapePreservesChoices(t *testing.T) {
	m := New("", TeamInfo{})
	m.width, m.height = 80, 24
	replies := make(chan string, 1)
	next, _ := m.Update(AskUserMsg{Question: "Choose", Type: "multiple_choice", AllowAny: true,
		Options: []AskUserOption{{Label: "first"}, {Label: "second"}}, ReplyCh: replies})
	m = next.(Model)
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeySpace})
	m = next.(Model)
	m = uxKey(m, "j")
	m = uxKey(m, "j")
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = next.(Model)
	if !m.ask.freeMode {
		t.Fatal("custom input did not open")
	}
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = next.(Model)
	if !m.inAskUser || m.ask.freeMode || m.ask.cursor != 2 || !m.ask.selected[0] || len(replies) != 0 {
		t.Fatalf("Esc did not preserve choice dialog: %+v", m.ask)
	}
}

func TestDashboardSmallLayoutAndCompactNavigation(t *testing.T) {
	m := New(strings.Repeat("long prompt ", 50), TeamInfo{})
	m.width, m.height = 80, 24
	m.tasks = []*team.TodoItem{
		{ID: "pending", Agent: "a", Desc: "pending", Status: team.TaskPending},
		{ID: "planned", Agent: "b", Desc: "planned", Status: team.TaskPlanned},
		{ID: "active", Agent: "c", Desc: "active", Status: team.TaskInProgress},
	}
	for i := range 6 {
		m.recentLogs = append(m.recentLogs, fmt.Sprintf("activity %d", i))
	}
	if !m.isDashboardCollapsed() || m.colBodyHeight()-2 < 8 {
		t.Fatalf("small dashboard has %d card lines", m.colBodyHeight()-2)
	}
	if h := lipgloss.Height(m.View()); h > m.height {
		t.Fatalf("dashboard view height %d exceeds terminal %d", h, m.height)
	}
	m = uxKey(m, "j")
	if m.col != 1 || m.row != 0 {
		t.Fatalf("compact down did not select planned task: col=%d row=%d", m.col, m.row)
	}
	m = uxKey(m, "l")
	if m.col != 2 {
		t.Fatalf("compact right stayed in Queued: col=%d", m.col)
	}
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyShiftTab})
	m = next.(Model)
	if compactGroupForCol(m.col) != 0 {
		t.Fatalf("shift+tab group=%d, want Queued", compactGroupForCol(m.col))
	}
	m = uxKey(m, "z")
	if m.isDashboardCollapsed() {
		t.Fatal("z did not expand dashboard")
	}
	m = uxKey(m, "z")
	if !m.isDashboardCollapsed() {
		t.Fatal("z did not restore compact header")
	}
}

func TestDashboardMouseUsesRenderedCardCoordinates(t *testing.T) {
	for _, width := range []int{80, 120} {
		m := New("task", TeamInfo{})
		m.width, m.height = width, 24
		m.mouseEnabled = true
		m.tasks = []*team.TodoItem{{ID: "p", Agent: "a", Desc: "pending", Status: team.TaskPending},
			{ID: "d", Agent: "b", Desc: "done", Status: team.TaskDone}}
		m.vpReady = true
		m.vp.Width, m.vp.Height = width, 10
		colW := (width - 5) / 6
		visualCol := 3
		if width == 80 {
			colW = (width - 2) / 3
			visualCol = 2
		}
		x := visualCol * (colW + 1)
		y := m.dashboardLayout().bodyY + 2
		next, _ := m.Update(tea.MouseMsg{X: x, Y: y, Action: tea.MouseActionPress, Button: tea.MouseButtonLeft})
		got := next.(Model)
		if !got.inDetail || got.detailID != "d" {
			t.Fatalf("width %d: clicked done card, opened %q", width, got.detailID)
		}
	}
}

func TestAskUserLongOptionsStayInViewport(t *testing.T) {
	m := New("", TeamInfo{})
	m.width, m.height = 80, 24
	opts := make([]AskUserOption, 10)
	for i := range opts {
		opts[i].Label = fmt.Sprintf("Option %d", i+1)
	}
	next, _ := m.Update(AskUserMsg{Question: strings.Repeat("long question ", 30), Type: "single_choice",
		Options: opts, ReplyCh: make(chan string, 1)})
	m = next.(Model)
	m.ask.cursor = 9
	view := ansi.Strip(m.askUserView())
	if lipgloss.Height(view) > 24 || !strings.Contains(view, "Option 10") || !strings.Contains(view, "enter select") {
		t.Fatalf("last option or hint inaccessible in 80x24 dialog:\n%s", view)
	}
}

func TestRunningQuitHintIsVisible(t *testing.T) {
	m := New("task", TeamInfo{})
	m.width, m.height = 80, 24
	m.tasks = []*team.TodoItem{{ID: "1", Agent: "agent", Desc: "running", Status: team.TaskInProgress}}
	m = uxKey(m, "q")
	if !strings.Contains(ansi.Strip(m.View()), "Press Esc for quit options") {
		t.Fatal("q hint hidden by in-progress task")
	}
}

func TestToolOutputExpansionKeepsBoundedSource(t *testing.T) {
	result := strings.Repeat("a", 4100) + "suffix-after-preview"
	preview := RenderToolResult("grep", result)
	full := RenderToolResultExpanded("grep", result)
	if strings.Contains(preview, "suffix-after-preview") || !strings.Contains(full, "suffix-after-preview") {
		t.Fatal("preview and expanded source do not differ at the 4000-rune boundary")
	}
	m := New("task", TeamInfo{})
	m.width, m.height = 100, 30
	m.tasks = []*team.TodoItem{{ID: "task", Status: team.TaskInProgress}}
	m.detailID, m.inDetail = "task", true
	m.vpReady = true
	m.vp.Width, m.vp.Height = 100, 20
	next, _ := m.Update(TaskLogMsg{TodoID: "task", Line: preview, FullLine: full})
	m = next.(Model)
	if strings.Contains(m.buildDetailContent(), "suffix-after-preview") {
		t.Fatal("collapsed detail exposed expanded output")
	}
	m = uxKey(m, "o")
	if m.expandedLogIndex != 0 || !strings.Contains(strings.ReplaceAll(m.buildDetailContent(), "\n", ""), "suffix-after-preview") {
		t.Fatal("o did not expose bounded full output")
	}
	m = uxKey(m, "o")
	if strings.Contains(m.buildDetailContent(), "suffix-after-preview") {
		t.Fatal("o did not collapse output")
	}
	m.storeExpandedLog("task", 1, strings.Repeat("x", maxExpandedToolRunes+100))
	if len([]rune(m.fullLogs["task"][1])) > maxExpandedToolRunes+30 {
		t.Fatal("expanded output exceeded configured bound")
	}
	m.logs["task"] = make([]string, maxTaskLogLines+1)
	m.fullLogs["task"] = map[int]string{0: "old", maxTaskLogLines: "new"}
	m.trimTaskLogs("task")
	if len(m.fullLogs["task"]) != 1 || m.fullLogs["task"][maxTaskLogLines-1] != "new" {
		t.Fatalf("expanded entries did not follow trimmed log indexes: %#v", m.fullLogs["task"])
	}
}
