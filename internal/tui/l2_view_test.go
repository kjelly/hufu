package tui

import (
	"regexp"
	"strings"
	"testing"

	"github.com/kjelly/hufu/internal/team"
)

// ansiReStrip matches ANSI escape sequences for stripping in view tests.
var ansiReStrip = regexp.MustCompile(`\x1b\[[0-9;?]*[A-Za-z]|\x1b\][^\x07]*\x07`)

// stripViewANSI removes ANSI codes from a view string for semantic assertions.
func stripViewANSI(s string) string {
	return ansiReStrip.ReplaceAllString(s, "")
}

// containsView checks if the stripped view contains a substring.
func containsView(view, want string) bool {
	return strings.Contains(stripViewANSI(view), want)
}

// ── L2: Help view ───────────────────────────────────────────────────────────

func TestView_HelpContent(t *testing.T) {
	m := New("test", TeamInfo{TeamName: "t"})
	m.width = 100
	m.height = 30
	m.inHelp = true

	view := m.View()
	for _, want := range []string{
		"keyboard reference",
		"switch column",
		"move cursor",
		"open detail view",
		"enter VISUAL mode",
		"quit",
		"ctrl+c",
	} {
		if !containsView(view, want) {
			t.Errorf("help view missing %q", want)
		}
	}
}

// ── L2: Info panel ───────────────────────────────────────────────────────────

func TestView_InfoPanelContent(t *testing.T) {
	info := TeamInfo{
		TeamName:     "my-team",
		DefaultModel: "qwen3:8b",
		Agents: []AgentInfoEntry{
			{Name: "researcher", Role: "worker", Model: "qwen3:8b"},
			{Name: "writer", Role: "worker", Model: "qwen3:8b"},
		},
		Skills:       []string{"code-review", "git-commit"},
		SidecarModel: "qwen3:1b",
		GuardModel:   "qwen3:8b",
		Workspace:    "/tmp/ws",
		SSHSessions:  2,
	}
	m := New("test", info)
	m.width = 100
	m.height = 30
	m.inInfo = true

	view := m.View()
	for _, want := range []string{
		"my-team",
		"researcher",
		"writer",
		"code-review",
		"git-commit",
		"qwen3:1b",
		"qwen3:8b",
	} {
		if !containsView(view, want) {
			t.Errorf("info panel missing %q", want)
		}
	}
}

// ── L2: Search view ─────────────────────────────────────────────────────────

func TestView_SearchContent(t *testing.T) {
	m := New("test", TeamInfo{TeamName: "t"})
	m.width = 100
	m.height = 30
	m.inSearch = true
	m.searchInput.SetValue("query text")

	view := m.View()
	for _, want := range []string{
		"Search",
		"query text",
		"enter search",
		"esc cancel",
	} {
		if !containsView(view, want) {
			t.Errorf("search view missing %q", want)
		}
	}
}

// ── L2: Prompt input view ──────────────────────────────────────────────────

func TestView_PromptInputContent(t *testing.T) {
	m := New("test", TeamInfo{TeamName: "t"})
	m.width = 100
	m.height = 30
	m.inPromptInput = true
	m.promptInput.SetValue("fix the bug")

	view := m.View()
	for _, want := range []string{
		"Additional Prompt",
		"fix the bug",
		"enter submit",
		"esc cancel",
	} {
		if !containsView(view, want) {
			t.Errorf("prompt input view missing %q", want)
		}
	}
}

// ── L2: Confirm dialog ──────────────────────────────────────────────────────

func TestView_ConfirmContent(t *testing.T) {
	m := New("test", TeamInfo{TeamName: "t"})
	m.width = 100
	m.height = 30
	m.inConfirm = true

	view := m.View()
	for _, want := range []string{
		"Quit hufu?",
		"No",
		"Yes",
		"Force",
	} {
		if !containsView(view, want) {
			t.Errorf("confirm view missing %q", want)
		}
	}
}

func TestView_ConfirmChoiceHighlight(t *testing.T) {
	m := New("test", TeamInfo{TeamName: "t"})
	m.width = 100
	m.height = 30
	m.inConfirm = true

	choices := []int{0, 1, 2}
	labels := []string{"No", "Yes", "Force"}

	for i, choice := range choices {
		m.confirmChoice = choice
		view := m.View()
		if !containsView(view, labels[i]) {
			t.Errorf("confirm view with choice %d missing %q", choice, labels[i])
		}
	}
}

// ── L2: Activity log view ───────────────────────────────────────────────────

func TestView_ActivityLogContent(t *testing.T) {
	m := New("test", TeamInfo{TeamName: "t"})
	m.width = 100
	m.height = 30
	m.inActivityLog = true
	m.recentLogs = []string{
		"Step 1 started",
		"Tool call: bash",
		"Step 2 completed",
	}
	m.initActivityVP()

	view := m.View()
	for _, want := range []string{
		"ACTIVITY LOG",
		"Step 1 started",
		"Tool call: bash",
		"Step 2 completed",
	} {
		if !containsView(view, want) {
			t.Errorf("activity log view missing %q", want)
		}
	}
}

func TestView_ActivityLogEmpty(t *testing.T) {
	m := New("test", TeamInfo{TeamName: "t"})
	m.width = 100
	m.height = 30
	m.inActivityLog = true
	m.initActivityVP()

	view := m.View()
	if !containsView(view, "No activity") {
		t.Errorf("empty activity log should show placeholder, got: %s", stripViewANSI(view)[:200])
	}
}

// ── L2: Result view ─────────────────────────────────────────────────────────

func TestView_ResultContent(t *testing.T) {
	m := New("test", TeamInfo{TeamName: "t"})
	m.width = 100
	m.height = 30
	m.finished = true
	m.result = "The final answer is 42."
	m.inResult = true
	m.vpReady = true
	m.vp.Width = 100
	m.vp.Height = 20
	m.vp.SetContent(m.result)

	view := m.View()
	for _, want := range []string{
		"Result",
		"The final answer is 42.",
		"esc",
	} {
		if !containsView(view, want) {
			t.Errorf("result view missing %q", want)
		}
	}
}

// ── L2: Memory view ─────────────────────────────────────────────────────────

func TestView_MemoryContent(t *testing.T) {
	m := New("test", TeamInfo{TeamName: "t", Workspace: ""})
	m.width = 100
	m.height = 30
	m.inMemory = true
	m.loadMemoryContent()

	view := m.View()
	// With empty workspace, should show "Short-term memory is empty"
	if !containsView(view, "Memory") {
		t.Errorf("memory view should contain 'Memory', got: %s", stripViewANSI(view)[:200])
	}
}

func TestView_MemoryWithWorkspace(t *testing.T) {
	tmpDir := t.TempDir()

	// Write STM file
	stmContent := "# Context\n\n- Item one\n- Item two\n\nSome text here."
	stmPath := tmpDir + "/stm.md"
	if err := writeFile(stmPath, stmContent); err != nil {
		t.Fatal(err)
	}

	// Write LTM file
	ltmContent := "# Long-term\n\n- Learned lesson 1\n- Learned lesson 2"
	ltmPath := tmpDir + "/ltm-test-team.md"
	if err := writeFile(ltmPath, ltmContent); err != nil {
		t.Fatal(err)
	}

	m := New("test", TeamInfo{TeamName: "test-team", Workspace: tmpDir})
	m.width = 100
	m.height = 30
	m.inMemory = true
	m.loadMemoryContent()

	view := m.View()
	for _, want := range []string{
		"Short-term Memory",
		"Context",
		"Item one",
		"Item two",
		"Long-term Memory",
		"Learned lesson 1",
		"Learned lesson 2",
	} {
		if !containsView(view, want) {
			t.Errorf("memory view with workspace missing %q", want)
		}
	}
}

// ── L2: Detail view ─────────────────────────────────────────────────────────

func TestView_DetailContent(t *testing.T) {
	m := New("test", TeamInfo{TeamName: "t"})
	m.width = 100
	m.height = 30
	m.tasks = sampleTasks()
	m.inDetail = true
	m.detailID = "t1"
	m.logs["t1"] = []string{"Starting research...", "Found 3 papers"}
	m.vpReady = true
	m.vp.Width = 100
	m.vp.Height = 20
	m.vp.SetContent(m.buildDetailContent())

	view := m.View()
	for _, want := range []string{
		"Task",
		"researcher",
		"Starting research",
		"Found 3 papers",
		"esc back",
	} {
		if !containsView(view, want) {
			t.Errorf("detail view missing %q", want)
		}
	}
}

func TestView_DetailEmptyLogs(t *testing.T) {
	m := New("test", TeamInfo{TeamName: "t"})
	m.width = 100
	m.height = 30
	m.tasks = []*team.TodoItem{
		{ID: "t1", Status: team.TaskPending, Desc: "New task", Agent: "worker"},
	}
	m.inDetail = true
	m.detailID = "t1"
	m.vpReady = true
	m.vp.Width = 100
	m.vp.Height = 20
	m.vp.SetContent(m.buildDetailContent())

	view := m.View()
	if !containsView(view, "no output yet") {
		t.Errorf("detail view with empty logs should show 'no output yet', got: %s", stripViewANSI(view)[:200])
	}
}

func TestView_DetailSkippedTask(t *testing.T) {
	m := New("test", TeamInfo{TeamName: "t"})
	m.width = 100
	m.height = 30
	m.tasks = []*team.TodoItem{
		{ID: "t1", Status: team.TaskSkipped, Desc: "Skipped task", Agent: "worker"},
	}
	m.inDetail = true
	m.detailID = "t1"
	m.vpReady = true
	m.vp.Width = 100
	m.vp.Height = 20
	m.vp.SetContent(m.buildDetailContent())

	view := m.View()
	if !containsView(view, "not executed") {
		t.Errorf("detail view for skipped task should show 'not executed', got: %s", stripViewANSI(view)[:200])
	}
}

// ── L2: AskUser view ─────────────────────────────────────────────────────────

func TestView_AskUserFreeText(t *testing.T) {
	m := New("test", TeamInfo{TeamName: "t"})
	m.width = 100
	m.height = 30
	m.inAskUser = true
	m.ask = askState{
		req: &AskUserMsg{
			Question: "Enter your name",
			Type:     "free_text",
			ReplyCh:  make(chan string, 1),
		},
	}
	m.ask.ti.SetValue("Alice")

	view := m.View()
	for _, want := range []string{
		"Enter your name",
		"Alice",
		"enter",
		"submit",
		"ctrl+c",
		"cancel",
	} {
		if !containsView(view, want) {
			t.Errorf("ask_user free_text view missing %q", want)
		}
	}
}

func TestView_AskUserSingleChoice(t *testing.T) {
	m := New("test", TeamInfo{TeamName: "t"})
	m.width = 100
	m.height = 30
	m.inAskUser = true
	m.ask = askState{
		req: &AskUserMsg{
			Question: "Pick one",
			Type:     "single_choice",
			Options: []AskUserOption{
				{Label: "Option A", Value: "a"},
				{Label: "Option B", Value: "b"},
			},
			ReplyCh: make(chan string, 1),
		},
	}

	view := m.View()
	for _, want := range []string{
		"Pick one",
		"Option A",
		"Option B",
		"enter",
		"navigate",
	} {
		if !containsView(view, want) {
			t.Errorf("ask_user single_choice view missing %q", want)
		}
	}
}

func TestView_AskUserMultipleChoice(t *testing.T) {
	m := New("test", TeamInfo{TeamName: "t"})
	m.width = 100
	m.height = 30
	m.inAskUser = true
	m.ask = askState{
		req: &AskUserMsg{
			Question: "Pick any",
			Type:     "multiple_choice",
			Options: []AskUserOption{
				{Label: "A", Value: "a"},
				{Label: "B", Value: "b"},
			},
			ReplyCh: make(chan string, 1),
		},
		selected: make([]bool, 2),
	}

	view := m.View()
	for _, want := range []string{
		"Pick any",
		"toggle",
		"confirm",
	} {
		if !containsView(view, want) {
			t.Errorf("ask_user multiple_choice view missing %q", want)
		}
	}
}

// ── L2: Column view ─────────────────────────────────────────────────────────

func TestView_ColumnTitles(t *testing.T) {
	m := New("test", TeamInfo{TeamName: "t"})
	m.width = 120
	m.height = 40
	m.tasks = sampleTasks()

	view := m.View()
	for _, want := range []string{
		"PENDING",
		"PLANNED",
		"IN PROGRESS",
		"DONE",
		"SKIP",
		"ERROR",
	} {
		if !containsView(view, want) {
			t.Errorf("column view missing %q", want)
		}
	}
}

func TestView_EmptyState(t *testing.T) {
	m := New("waiting for tasks", TeamInfo{TeamName: "t"})
	m.width = 120
	m.height = 40
	m.tasks = nil

	view := m.View()
	for _, want := range []string{
		"waiting for tasks",
		"PENDING",
	} {
		if !containsView(view, want) {
			t.Errorf("empty state view missing %q", want)
		}
	}
}

func TestView_Initialising(t *testing.T) {
	m := New("test", TeamInfo{TeamName: "t"})
	// width=0 simulates pre-WindowSizeMsg
	view := m.View()
	if !containsView(view, "Initialising") {
		t.Errorf("expected 'Initialising' when width=0, got %q", view)
	}
}

// ── L2: Footer variations ────────────────────────────────────────────────────

func TestView_FooterNotFinished(t *testing.T) {
	m := New("test", TeamInfo{TeamName: "t"})
	m.width = 120
	m.height = 40
	m.tasks = sampleTasks()

	view := m.View()
	if !containsView(view, "help") {
		t.Errorf("footer when not finished should contain 'help'")
	}
	if !containsView(view, "prompt") {
		t.Errorf("footer when not finished should contain 'prompt'")
	}
}

func TestView_FooterFinished(t *testing.T) {
	m := New("test", TeamInfo{TeamName: "t"})
	m.width = 120
	m.height = 40
	m.tasks = sampleTasks()
	m.finished = true

	view := m.View()
	if !containsView(view, "report") {
		t.Errorf("footer when finished should contain 'report'")
	}
	if !containsView(view, "quit") {
		t.Errorf("footer when finished should contain 'quit'")
	}
}

func TestView_FooterWithSearchResults(t *testing.T) {
	m := New("test", TeamInfo{TeamName: "t"})
	m.width = 120
	m.height = 40
	m.tasks = sampleTasks()
	m.searchResults = []*team.TodoItem{{ID: "t1"}}
	m.searchIdx = 0

	view := m.View()
	if !containsView(view, "next/prev") {
		t.Errorf("footer with search results should contain 'next/prev'")
	}
}

// ── L2: Detail footer with VISUAL mode ───────────────────────────────────────

func TestView_DetailFooterVisualMode(t *testing.T) {
	m := New("test", TeamInfo{TeamName: "t"})
	m.width = 100
	m.height = 30
	m.tasks = sampleTasks()
	m.inDetail = true
	m.inVisual = true
	m.detailID = "t1"
	m.logs["t1"] = []string{"line 1", "line 2", "line 3"}
	m.visualStart = 0
	m.visualEnd = 1
	m.vpReady = true
	m.vp.Width = 100
	m.vp.Height = 20

	view := m.View()
	for _, want := range []string{
		"VISUAL",
		"selected",
		"copy",
		"cancel",
	} {
		if !containsView(view, want) {
			t.Errorf("visual footer missing %q", want)
		}
	}
}

// ── L2: Status area with result box ─────────────────────────────────────────

func TestView_StatusAreaResultBox(t *testing.T) {
	m := New("test", TeamInfo{TeamName: "t"})
	m.width = 120
	m.height = 40
	m.tasks = sampleTasks()
	m.finished = true
	m.result = "The final answer is 42.\nLine 2.\nLine 3."

	view := m.View()
	for _, want := range []string{
		"Result",
		"The final answer is 42.",
		"Line 2.",
	} {
		if !containsView(view, want) {
			t.Errorf("status area with result box missing %q", want)
		}
	}
}

func TestView_StatusAreaResultTruncation(t *testing.T) {
	m := New("test", TeamInfo{TeamName: "t"})
	m.width = 120
	m.height = 40
	m.tasks = sampleTasks()
	m.finished = true

	// Long result exceeding maxResultLines
	var lines []string
	for i := 0; i < 20; i++ {
		lines = append(lines, "line "+string(rune('A'+i)))
	}
	m.result = strings.Join(lines, "\n")

	view := m.View()
	if !containsView(view, "more lines") {
		t.Errorf("truncated result should show 'more lines' indicator")
	}
}

// ── L2: Progress bar ─────────────────────────────────────────────────────────

func TestView_ProgressBarHiddenForFewTasks(t *testing.T) {
	m := New("test", TeamInfo{TeamName: "t"})
	m.width = 120
	m.height = 40
	m.tasks = []*team.TodoItem{
		{ID: "1", Status: team.TaskDone, Desc: "Task 1", Agent: "A"},
		{ID: "2", Status: team.TaskPending, Desc: "Task 2", Agent: "B"},
	}

	view := m.View()
	// Progress bar only shows when len(tasks) >= 3
	// So with 2 tasks, no progress bar
	if containsView(view, "tasks") && containsView(view, "errors") {
		// The progress bar text has both "tasks" and "errors" in a single line
		// Check it's not the progress bar format
		if strings.Contains(view, "active") && strings.Contains(view, "errors") {
			// Might be from footer — let's check more specifically
			if strings.Contains(stripViewANSI(view), "/2 tasks") {
				t.Error("progress bar should be hidden for < 3 tasks")
			}
		}
	}
}

func TestView_ProgressBarVisible(t *testing.T) {
	m := New("test", TeamInfo{TeamName: "t"})
	m.width = 120
	m.height = 40
	m.tasks = sampleTasks() // 7 tasks

	view := m.View()
	if !containsView(view, "tasks") {
		t.Error("progress bar should be visible with >= 3 tasks")
	}
}

// ── L2: Chat mode view ──────────────────────────────────────────────────────

func TestView_ChatModePromptInputOnStart(t *testing.T) {
	m := New("", TeamInfo{TeamName: "t", IsChat: true})
	m.width = 100
	m.height = 30

	if !m.inPromptInput {
		t.Error("expected inPromptInput=true in chat mode with empty prompt")
	}
	view := m.View()
	if !containsView(view, "Additional Prompt") {
		t.Error("chat mode with empty prompt should show prompt input")
	}
}

// ── L2: Compact mode view ──────────────────────────────────────────────────

func TestView_CompactModeTitles(t *testing.T) {
	m := New("test", TeamInfo{TeamName: "t"})
	m.width = 70 // 60 <= 70 < 80 → compact mode
	m.height = 30
	m.tasks = sampleTasks()

	view := m.View()
	for _, want := range []string{
		"Queued",
		"Active",
		"Finished",
	} {
		if !containsView(view, want) {
			t.Errorf("compact mode view missing %q", want)
		}
	}
}

// ── helper ──────────────────────────────────────────────────────────────────

func writeFile(path, content string) error {
	return osWriteFile(path, []byte(content), 0644)
}
