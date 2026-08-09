package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/kjelly/hufu/internal/team"
)

// ── L1: AskUser dialog ─────────────────────────────────────────────────────

func TestAskUser_FreeText(t *testing.T) {
	m := New("test", TeamInfo{TeamName: "t"})
	m.width = 100
	m.height = 30

	replyCh := make(chan string, 1)
	m2, cmd := m.Update(AskUserMsg{
		Question: "What is your name?",
		Type:     "free_text",
		ReplyCh:  replyCh,
	})
	model := m2.(Model)
	if !model.inAskUser {
		t.Fatal("expected inAskUser=true after AskUserMsg")
	}
	if cmd == nil {
		t.Error("expected textinput.Blink command for free_text focus")
	}

	// Type some text
	m3, _ := model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("Alice")})
	model = m3.(Model)

	// Submit with enter
	m4, _ := model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	model = m4.(Model)
	if model.inAskUser {
		t.Error("expected inAskUser=false after enter submit")
	}

	select {
	case resp := <-replyCh:
		if !strings.Contains(resp, "Alice") {
			t.Errorf("reply does not contain 'Alice': %q", resp)
		}
	default:
		t.Error("expected response on replyCh")
	}
}

func TestAskUser_SingleChoice(t *testing.T) {
	m := New("test", TeamInfo{TeamName: "t"})
	m.width = 100
	m.height = 30

	replyCh := make(chan string, 1)
	m2, _ := m.Update(AskUserMsg{
		Question: "Pick one",
		Type:     "single_choice",
		Options: []AskUserOption{
			{Label: "Option A", Value: "a"},
			{Label: "Option B", Value: "b"},
			{Label: "Option C", Value: "c"},
		},
		ReplyCh: replyCh,
	})
	model := m2.(Model)
	if !model.inAskUser {
		t.Fatal("expected inAskUser=true")
	}

	// Move down to Option B (index 1)
	m3, _ := model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
	model = m3.(Model)
	if model.ask.cursor != 1 {
		t.Fatalf("expected cursor 1, got %d", model.ask.cursor)
	}

	// Select
	m4, _ := model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	model = m4.(Model)
	if model.inAskUser {
		t.Error("expected inAskUser=false after enter")
	}

	select {
	case resp := <-replyCh:
		if !strings.Contains(resp, "b") {
			t.Errorf("reply should contain 'b': %q", resp)
		}
	default:
		t.Error("expected response on replyCh")
	}
}

func TestAskUser_MultipleChoice(t *testing.T) {
	m := New("test", TeamInfo{TeamName: "t"})
	m.width = 100
	m.height = 30

	replyCh := make(chan string, 1)
	m2, _ := m.Update(AskUserMsg{
		Question: "Pick any",
		Type:     "multiple_choice",
		Options: []AskUserOption{
			{Label: "A", Value: "a"},
			{Label: "B", Value: "b"},
			{Label: "C", Value: "c"},
		},
		ReplyCh: replyCh,
	})
	model := m2.(Model)

	// Toggle option A (index 0)
	m3, _ := model.Update(tea.KeyMsg{Type: tea.KeySpace})
	model = m3.(Model)

	// Move down to B and toggle
	m4, _ := model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
	model = m4.(Model)
	m5, _ := model.Update(tea.KeyMsg{Type: tea.KeySpace})
	model = m5.(Model)

	if !model.ask.selected[0] || !model.ask.selected[1] {
		t.Errorf("expected options 0 and 1 selected, got %v", model.ask.selected)
	}

	// Confirm
	m6, _ := model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	model = m6.(Model)
	if model.inAskUser {
		t.Error("expected inAskUser=false after enter")
	}

	select {
	case <-replyCh:
		// good
	default:
		t.Error("expected response on replyCh")
	}
}

func TestAskUser_Cancel(t *testing.T) {
	m := New("test", TeamInfo{TeamName: "t"})
	m.width = 100
	m.height = 30

	replyCh := make(chan string, 1)
	m2, _ := m.Update(AskUserMsg{
		Question: "Pick one",
		Type:     "single_choice",
		Options:  []AskUserOption{{Label: "A", Value: "a"}},
		ReplyCh:  replyCh,
	})
	model := m2.(Model)

	m3, _ := model.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	model = m3.(Model)
	if model.inAskUser {
		t.Error("expected inAskUser=false after ctrl+c")
	}

	select {
	case resp := <-replyCh:
		if strings.Contains(resp, "a") {
			t.Error("cancel should send empty/default response, not option value")
		}
	default:
		t.Error("expected response on replyCh after cancel")
	}
}

func TestAskUser_BoundsCheck_SingleChoiceCursor(t *testing.T) {
	m := New("test", TeamInfo{TeamName: "t"})
	m.width = 100
	m.height = 30

	replyCh := make(chan string, 1)
	m2, _ := m.Update(AskUserMsg{
		Question: "Pick one",
		Type:     "single_choice",
		Options:  []AskUserOption{{Label: "A", Value: "a"}},
		AllowAny: true,
		ReplyCh:  replyCh,
	})
	model := m2.(Model)

	// Cursor should be at 0 initially; move down to the "custom" option (index 1)
	m3, _ := model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
	model = m3.(Model)
	if model.ask.cursor != 1 {
		t.Fatalf("expected cursor 1 (custom), got %d", model.ask.cursor)
	}

	// Enter on custom should switch to free-text mode, not crash
	m4, _ := model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	model = m4.(Model)
	if !model.ask.freeMode {
		t.Error("expected freeMode=true after entering custom option")
	}
	if !model.inAskUser {
		t.Error("ask_user should still be active in free-text mode")
	}
}

// ── L1: Confirm dialog ─────────────────────────────────────────────────────

func TestConfirmDialog_Navigation(t *testing.T) {
	m := New("test", TeamInfo{TeamName: "t"})
	m.width = 100
	m.height = 30

	// Open confirm dialog
	m2, _ := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	model := m2.(Model)
	if !model.inConfirm {
		t.Fatal("expected inConfirm=true after esc")
	}
	if model.confirmChoice != 0 {
		t.Errorf("initial choice = %d, want 0", model.confirmChoice)
	}

	// Right to "Yes"
	m3, _ := model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("l")})
	model = m3.(Model)
	if model.confirmChoice != 1 {
		t.Errorf("after 'l' = %d, want 1", model.confirmChoice)
	}

	// Right to "Force"
	m4, _ := model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("l")})
	model = m4.(Model)
	if model.confirmChoice != 2 {
		t.Errorf("after 'l' = %d, want 2", model.confirmChoice)
	}

	// Left back to "Yes"
	m5, _ := model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("h")})
	model = m5.(Model)
	if model.confirmChoice != 1 {
		t.Errorf("after 'h' = %d, want 1", model.confirmChoice)
	}

	// Tab to "Force"
	m6, _ := model.Update(tea.KeyMsg{Type: tea.KeyTab})
	model = m6.(Model)
	if model.confirmChoice != 2 {
		t.Errorf("after tab = %d, want 2", model.confirmChoice)
	}

	// Left to "Yes"
	m7, _ := model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("h")})
	model = m7.(Model)
	if model.confirmChoice != 1 {
		t.Errorf("after 'h' = %d, want 1", model.confirmChoice)
	}
}

func TestConfirmDialog_CancelWithN(t *testing.T) {
	m := New("test", TeamInfo{TeamName: "t"})
	m.width = 100
	m.height = 30

	m2, _ := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	model := m2.(Model)

	m3, _ := model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("n")})
	model = m3.(Model)
	// Note: "f" returns tea.Quit without clearing inConfirm
	if model.inConfirm {
		t.Error("expected inConfirm=false after 'n'")
	}
}

func TestConfirmDialog_CancelWithEsc(t *testing.T) {
	m := New("test", TeamInfo{TeamName: "t"})
	m.width = 100
	m.height = 30

	m2, _ := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	model := m2.(Model)

	m3, _ := model.Update(tea.KeyMsg{Type: tea.KeyEsc})
	model = m3.(Model)
	// Note: "f" returns tea.Quit without clearing inConfirm
	if model.inConfirm {
		t.Error("expected inConfirm=false after esc")
	}
}

func TestConfirmDialog_YTriggersWrapUp(t *testing.T) {
	m := New("test", TeamInfo{TeamName: "t"})
	m.width = 100
	m.height = 30

	m2, _ := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	model := m2.(Model)

	m3, _ := model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("y")})
	model = m3.(Model)
	// Note: "f" returns tea.Quit without clearing inConfirm
	if model.inConfirm {
		t.Error("expected inConfirm=false after 'y'")
	}
	if !model.wrapUpRequested {
		t.Error("expected wrapUpRequested=true after 'y'")
	}
}

func TestConfirmDialog_ForceQuit(t *testing.T) {
	m := New("test", TeamInfo{TeamName: "t"})
	m.width = 100
	m.height = 30

	m2, _ := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	model := m2.(Model)

	_, cmd := model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("f")})
	// 'f' returns tea.Quit immediately; it does not clear inConfirm
	if cmd == nil {
		t.Error("expected tea.Quit command after 'f'")
	}
}

// ── L1: Mouse toggle ───────────────────────────────────────────────────────

func TestMouseToggle(t *testing.T) {
	m := New("test", TeamInfo{TeamName: "t"})
	m.width = 100
	m.height = 30
	m.tasks = []*team.TodoItem{
		{ID: "1", Status: team.TaskPending, Desc: "Task", Agent: "A"},
	}

	// 'm' toggles mouse on
	m2, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("m")})
	model := m2.(Model)
	if !model.mouseEnabled || !model.mouseManuallyEnabled {
		t.Error("expected mouseEnabled=true after 'm'")
	}
	if cmd == nil {
		t.Error("expected enableMouseCmd after 'm'")
	}

	// 'm' toggles mouse off
	m3, cmd := model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("m")})
	model = m3.(Model)
	if model.mouseEnabled || model.mouseManuallyEnabled {
		t.Error("expected mouseEnabled=false after second 'm'")
	}
	if cmd == nil {
		t.Error("expected disableMouseCmd after second 'm'")
	}
}

// ── L1: Memory view ────────────────────────────────────────────────────────

func TestMemoryView_OpenClose(t *testing.T) {
	m := New("test", TeamInfo{TeamName: "t", Workspace: ""})
	m.width = 100
	m.height = 30

	// 'M' opens memory
	m2, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("M")})
	model := m2.(Model)
	if !model.inMemory {
		t.Error("expected inMemory=true after 'M'")
	}
	if !model.memoryReady {
		t.Error("expected memoryReady=true after 'M'")
	}

	// Esc closes
	m3, _ := model.Update(tea.KeyMsg{Type: tea.KeyEsc})
	model = m3.(Model)
	if model.inMemory {
		t.Error("expected inMemory=false after esc")
	}
	if model.memoryReady {
		t.Error("expected memoryReady=false after esc")
	}
}

func TestMemoryView_ScrollTopBottom(t *testing.T) {
	m := New("test", TeamInfo{TeamName: "t", Workspace: ""})
	m.width = 100
	m.height = 30

	// Open memory
	m2, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("M")})
	model := m2.(Model)

	// 'g' goes to top
	m3, _ := model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("g")})
	model = m3.(Model)
	if model.memoryVP.YOffset != 0 {
		t.Errorf("expected YOffset 0 after 'g', got %d", model.memoryVP.YOffset)
	}

	// 'G' goes to bottom
	m4, _ := model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("G")})
	model = m4.(Model)
	// Just verify no panic; YOffset should be at or near the end
}

func TestMemoryView_CtrlC(t *testing.T) {
	m := New("test", TeamInfo{TeamName: "t"})
	m.width = 100
	m.height = 30

	m2, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("M")})
	model := m2.(Model)

	m3, _ := model.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	model = m3.(Model)
	if !model.wrapUpRequested {
		t.Error("expected wrapUpRequested=true after ctrl+c in memory view")
	}
}

// ── L1: ctrl+d / ctrl+u navigation ──────────────────────────────────────────

func TestNavigation_CtrlD_CtrlU(t *testing.T) {
	tasks := makeTasks(20, team.TaskPending)
	m := Model{
		tasks:  tasks,
		height: 30,
		width:  100,
		col:    0,
		row:    0,
	}
	m.scrollOff = [6]int{}

	// ctrl+d should jump down by quarter page
	m2, _ := m.Update(tea.KeyMsg{Type: tea.KeyCtrlD})
	model := m2.(Model)
	if model.row == 0 {
		t.Error("expected row > 0 after ctrl+d")
	}

	// ctrl+u should jump back up
	m3, _ := model.Update(tea.KeyMsg{Type: tea.KeyCtrlU})
	model = m3.(Model)
	if model.row != 0 {
		t.Errorf("expected row 0 after ctrl+u, got %d", model.row)
	}
}

// ── L1: Rapid key presses don't panic ───────────────────────────────────────

func TestRapidKeysNoPanic(t *testing.T) {
	m := New("rapid test", TeamInfo{TeamName: "t"})
	m.width = 100
	m.height = 30
	m.tasks = sampleTasks()

	keys := []tea.KeyMsg{
		{Type: tea.KeyRunes, Runes: []rune("j")},
		{Type: tea.KeyRunes, Runes: []rune("j")},
		{Type: tea.KeyRunes, Runes: []rune("j")},
		{Type: tea.KeyRunes, Runes: []rune("k")},
		{Type: tea.KeyRunes, Runes: []rune("l")},
		{Type: tea.KeyRunes, Runes: []rune("l")},
		{Type: tea.KeyRunes, Runes: []rune("h")},
		{Type: tea.KeyRunes, Runes: []rune("G")},
		{Type: tea.KeyRunes, Runes: []rune("g")},
		{Type: tea.KeyEnter},
		{Type: tea.KeyEsc},
		{Type: tea.KeyRunes, Runes: []rune("?")},
		{Type: tea.KeyEsc},
		{Type: tea.KeyRunes, Runes: []rune("i")},
		{Type: tea.KeyEsc},
		{Type: tea.KeyRunes, Runes: []rune("/")},
		{Type: tea.KeyEsc},
		{Type: tea.KeyRunes, Runes: []rune("a")},
		{Type: tea.KeyEsc},
		{Type: tea.KeyRunes, Runes: []rune("c")},
		{Type: tea.KeyEsc},
		{Type: tea.KeyTab},
		{Type: tea.KeyTab},
		{Type: tea.KeyTab},
	}

	model := m
	for _, key := range keys {
		next, _ := model.Update(key)
		model = next.(Model)
	}

	// Should not have panicked — test passes if we reach here.
	_ = model.View()
}

// ── L1: CoordStatusMsg ─────────────────────────────────────────────────────

func TestCoordStatusMsg(t *testing.T) {
	m := New("test", TeamInfo{TeamName: "t"})
	m.coordItem = &team.TodoItem{ID: team.CoordTodoID, Status: team.TaskPending}

	m2, _ := m.Update(CoordStatusMsg{Status: team.TaskInProgress})
	model := m2.(Model)
	if model.coordItem.Status != team.TaskInProgress {
		t.Errorf("expected coordItem status in_progress, got %s", model.coordItem.Status)
	}
}

// ── L1: WrapUpMsg ──────────────────────────────────────────────────────────

func TestWrapUpMsg(t *testing.T) {
	m := New("test", TeamInfo{TeamName: "t"})
	m.width = 100
	m.height = 30
	m.tasks = sampleTasks()

	m2, _ := m.Update(WrapUpMsg{})
	model := m2.(Model)
	if !model.wrapUpRequested {
		t.Error("expected wrapUpRequested=true after WrapUpMsg")
	}
}

// ── L1: copySuccessMsg ────────────────────────────────────────────────────

func TestCopySuccessMsg(t *testing.T) {
	m := New("test", TeamInfo{TeamName: "t"})
	m.width = 100
	m.height = 30

	m2, _ := m.Update(copySuccessMsg{Lines: 5})
	model := m2.(Model)
	if !strings.Contains(model.statusText, "5") {
		t.Errorf("expected statusText to contain '5', got %q", model.statusText)
	}
}

// ── L1: WindowSizeMsg with overlays ─────────────────────────────────────────

func TestWindowSizeMsg_WithDetailOverlay(t *testing.T) {
	m := New("test", TeamInfo{TeamName: "t"})
	m.width = 100
	m.height = 30
	m.tasks = sampleTasks()
	m.inDetail = true
	m.detailID = "t1"
	m.vpReady = true
	m.vp.Width = 100
	m.vp.Height = 20

	m2, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	model := m2.(Model)
	if model.width != 120 {
		t.Errorf("expected width 120, got %d", model.width)
	}
	if model.height != 40 {
		t.Errorf("expected height 40, got %d", model.height)
	}
	if !model.inDetail {
		t.Error("expected inDetail to remain true after resize")
	}
}

func TestWindowSizeMsg_WithSearchOverlay(t *testing.T) {
	m := New("test", TeamInfo{TeamName: "t"})
	m.width = 100
	m.height = 30
	m.inSearch = true

	m2, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	model := m2.(Model)
	if model.width != 80 {
		t.Errorf("expected width 80, got %d", model.width)
	}
	if !model.inSearch {
		t.Error("expected inSearch to remain true after resize")
	}
}

// ── L1: Mouse wheel scroll in columns ──────────────────────────────────────

func TestMouseWheelScroll(t *testing.T) {
	tasks := makeTasks(10, team.TaskPending)
	m := Model{
		tasks:        tasks,
		height:       30,
		width:        100,
		col:          0,
		row:          5,
		mouseEnabled: true,
		scrollOff:    [6]int{},
	}

	// Wheel down
	m2, _ := m.Update(tea.MouseMsg{
		Action: tea.MouseActionPress,
		Button: tea.MouseButtonWheelDown,
	})
	model := m2.(Model)
	if model.row != 6 {
		t.Errorf("expected row 6 after wheel down, got %d", model.row)
	}

	// Wheel up
	m3, _ := model.Update(tea.MouseMsg{
		Action: tea.MouseActionPress,
		Button: tea.MouseButtonWheelUp,
	})
	model = m3.(Model)
	if model.row != 5 {
		t.Errorf("expected row 5 after wheel up, got %d", model.row)
	}
}

// ── L1: Search cancel clears results ────────────────────────────────────────

func TestSearchCancelClearsResults(t *testing.T) {
	m := New("test", TeamInfo{TeamName: "t"})
	m.width = 100
	m.height = 30
	m.tasks = sampleTasks()
	m.searchQuery = "deploy"
	m.searchResults = []*team.TodoItem{{ID: "t7"}}

	// Esc should clear search results
	m2, _ := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	model := m2.(Model)
	if model.searchQuery != "" {
		t.Errorf("expected empty searchQuery after esc, got %q", model.searchQuery)
	}
	if len(model.searchResults) != 0 {
		t.Errorf("expected empty searchResults after esc, got %d", len(model.searchResults))
	}
}

// ── L1: Search next/previous match ─────────────────────────────────────────

func TestSearchNextPrevMatch(t *testing.T) {
	m := New("test", TeamInfo{TeamName: "t"})
	m.width = 100
	m.height = 30
	m.tasks = sampleTasks()
	m.searchResults = []*team.TodoItem{
		{ID: "t1", Desc: "Research API", Agent: "researcher"},
		{ID: "t5", Desc: "Setup repo", Agent: "devops"},
		{ID: "t7", Desc: "Failed deploy", Agent: "devops"},
	}
	m.searchIdx = 0

	// 'n' should advance to next match
	m2, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("n")})
	model := m2.(Model)
	if model.searchIdx != 1 {
		t.Errorf("expected searchIdx 1 after 'n', got %d", model.searchIdx)
	}

	// 'N' should go to previous match (wraps)
	m3, _ := model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("N")})
	model = m3.(Model)
	if model.searchIdx != 0 {
		t.Errorf("expected searchIdx 0 after 'N', got %d", model.searchIdx)
	}
}

// ── L1: FinishedMsg with wrapUpRequested triggers quit ─────────────────────

func TestFinishedMsg_WithWrapUpTriggersQuit(t *testing.T) {
	m := New("test", TeamInfo{TeamName: "t"})
	m.width = 100
	m.height = 30
	m.tasks = sampleTasks()
	m.wrapUpRequested = true

	m2, cmd := m.Update(FinishedMsg{})
	model := m2.(Model)
	if !model.finished {
		t.Error("expected finished=true after FinishedMsg")
	}
	if cmd == nil {
		t.Error("expected tea.Quit command when finished && wrapUpRequested")
	}
}

// ── L1: Help panel keys ────────────────────────────────────────────────────

func TestHelpPanel_CloseKeys(t *testing.T) {
	closeKeys := []string{"esc", "q", "?", "enter"}
	for _, key := range closeKeys {
		t.Run(key, func(t *testing.T) {
			m := New("test", TeamInfo{TeamName: "t"})
			m.width = 100
			m.height = 30
			m.inHelp = true

			var msg tea.KeyMsg
			switch key {
			case "esc":
				msg = tea.KeyMsg{Type: tea.KeyEsc}
			case "enter":
				msg = tea.KeyMsg{Type: tea.KeyEnter}
			default:
				msg = tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(key)}
			}

			m2, _ := m.Update(msg)
			model := m2.(Model)
			if model.inHelp {
				t.Errorf("expected inHelp=false after %q", key)
			}
		})
	}
}

// ── L1: Info panel close keys ───────────────────────────────────────────────

func TestInfoPanel_CloseKeys(t *testing.T) {
	closeKeys := []string{"esc", "q", "i", "enter"}
	for _, key := range closeKeys {
		t.Run(key, func(t *testing.T) {
			m := New("test", TeamInfo{TeamName: "t"})
			m.width = 100
			m.height = 30
			m.inInfo = true

			var msg tea.KeyMsg
			switch key {
			case "esc":
				msg = tea.KeyMsg{Type: tea.KeyEsc}
			case "enter":
				msg = tea.KeyMsg{Type: tea.KeyEnter}
			default:
				msg = tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(key)}
			}

			m2, _ := m.Update(msg)
			model := m2.(Model)
			if model.inInfo {
				t.Errorf("expected inInfo=false after %q", key)
			}
		})
	}
}

// ── L1: Activity log close keys ────────────────────────────────────────────

func TestActivityLog_CloseKeys(t *testing.T) {
	closeKeys := []string{"esc", "q", "a", "enter"}
	for _, key := range closeKeys {
		t.Run(key, func(t *testing.T) {
			m := New("test", TeamInfo{TeamName: "t"})
			m.width = 100
			m.height = 30
			m.inActivityLog = true

			var msg tea.KeyMsg
			switch key {
			case "esc":
				msg = tea.KeyMsg{Type: tea.KeyEsc}
			case "enter":
				msg = tea.KeyMsg{Type: tea.KeyEnter}
			default:
				msg = tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(key)}
			}

			m2, _ := m.Update(msg)
			model := m2.(Model)
			if model.inActivityLog {
				t.Errorf("expected inActivityLog=false after %q", key)
			}
		})
	}
}

// ── L1: Prompt input submit/cancel ──────────────────────────────────────────

func TestPromptInput_SubmitEmpty(t *testing.T) {
	m := New("test", TeamInfo{TeamName: "t"})
	m.width = 100
	m.height = 30

	// Open prompt input
	m2, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("c")})
	model := m2.(Model)
	if !model.inPromptInput {
		t.Fatal("expected inPromptInput=true")
	}

	// Submit with empty text — should close but not inject
	m3, _ := model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	model = m3.(Model)
	if model.inPromptInput {
		t.Error("expected inPromptInput=false after enter with empty text")
	}

	// No prompt should be in the channel
	select {
	case <-model.PromptInjectCh:
		t.Error("expected no prompt injected for empty submit")
	default:
		// Good
	}
}

func TestPromptInput_CancelWithCtrlC(t *testing.T) {
	m := New("test", TeamInfo{TeamName: "t"})
	m.width = 100
	m.height = 30

	m2, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("c")})
	model := m2.(Model)

	m3, _ := model.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	model = m3.(Model)
	if model.inPromptInput {
		t.Error("expected inPromptInput=false after ctrl+c")
	}
}

// ── L1: Result view open/close ──────────────────────────────────────────────

func TestResultView_OpenClose(t *testing.T) {
	m := New("test", TeamInfo{TeamName: "t"})
	m.width = 100
	m.height = 30
	m.finished = true
	m.result = "Result text"
	m.inResult = true
	m.vpReady = true
	m.vp.Width = 100
	m.vp.Height = 20

	// Esc closes
	m2, _ := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	model := m2.(Model)
	if model.inResult {
		t.Error("expected inResult=false after esc")
	}

	// Enter also closes
	m.inResult = true
	m3, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	model = m3.(Model)
	if model.inResult {
		t.Error("expected inResult=false after enter")
	}
}

func TestResultView_QuitWhenFinished(t *testing.T) {
	m := New("test", TeamInfo{TeamName: "t"})
	m.width = 100
	m.height = 30
	m.finished = true
	m.result = "Result text"
	m.inResult = true

	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("q")})
	// 'q' returns tea.Quit immediately; it does not clear inResult
	if cmd == nil {
		t.Error("expected tea.Quit when 'q' pressed while finished")
	}
}

// ── L1: AskUserCancelMsg ───────────────────────────────────────────────────

func TestAskUserCancelMsg(t *testing.T) {
	m := New("test", TeamInfo{TeamName: "t"})
	m.width = 100
	m.height = 30
	m.inAskUser = true
	m.ask.req = &AskUserMsg{ReplyCh: make(chan string, 1)}

	m2, _ := m.Update(AskUserCancelMsg{})
	model := m2.(Model)
	if model.inAskUser {
		t.Error("expected inAskUser=false after AskUserCancelMsg")
	}
}

func TestAskUserCancelMsg_FinishedWrapUp(t *testing.T) {
	m := New("test", TeamInfo{TeamName: "t"})
	m.width = 100
	m.height = 30
	m.inAskUser = true
	m.ask.req = &AskUserMsg{ReplyCh: make(chan string, 1)}
	m.finished = true
	m.wrapUpRequested = true

	m2, cmd := m.Update(AskUserCancelMsg{})
	model := m2.(Model)
	if model.inAskUser {
		t.Error("expected inAskUser=false after cancel")
	}
	if cmd == nil {
		t.Error("expected tea.Quit when finished && wrapUpRequested")
	}
}
