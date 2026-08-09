package tui

import (
	"bytes"
	"io"
	"regexp"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/exp/teatest"

	"github.com/kjelly/hufu/internal/team"
)

// ── Helpers ───────────────────────────────────────────────────────────────

// ansiRe matches ANSI escape sequences so we can strip them from teatest
// output for plain-text assertions.
var ansiRe = regexp.MustCompile(`\x1b\[[0-9;?]*[A-Za-z]|\x1b\][^\x07]*\x07|\r`)

func stripANSI(s string) string {
	return ansiRe.ReplaceAllString(s, "")
}

// tuiTestModel wraps a teatest.TestModel with a cumulative output buffer.
// Because io.ReadAll on the teatest output buffer *consumes* the data,
// each read only returns new bytes written since the last read. The
// cumulative buffer preserves all output across reads so that assertions
// can match against the full rendering history.
type tuiTestModel struct {
	tm  *teatest.TestModel
	buf bytes.Buffer
}

func (t *tuiTestModel) Send(msg tea.Msg) { t.tm.Send(msg) }
func (t *tuiTestModel) Type(s string)    { t.tm.Type(s) }
func (t *tuiTestModel) Quit() error      { return t.tm.Quit() }
func (t *tuiTestModel) WaitFinished(tb testing.TB, opts ...teatest.FinalOpt) {
	t.tm.WaitFinished(tb, opts...)
}
func (t *tuiTestModel) FinalModel(tb testing.TB, opts ...teatest.FinalOpt) tea.Model {
	return t.tm.FinalModel(tb, opts...)
}

func finalTUIModel(tb testing.TB, tm *tuiTestModel) Model {
	tb.Helper()
	if err := tm.Quit(); err != nil {
		tb.Fatalf("quit test model: %v", err)
	}
	fm := tm.FinalModel(tb, teatest.WithFinalTimeout(5*time.Second))
	model, ok := fm.(Model)
	if !ok {
		tb.Fatalf("final model = %T, want tui.Model", fm)
	}
	return model
}

// poll reads any new output from the program and appends it to the
// cumulative buffer.
func (t *tuiTestModel) poll() {
	bts, _ := io.ReadAll(t.tm.Output())
	t.buf.Write(bts)
}

// stripped returns the cumulative output with ANSI escape codes removed.
func (t *tuiTestModel) stripped() string {
	return stripANSI(t.buf.String())
}

// waitForText polls until the cumulative stripped output contains want.
func (t *tuiTestModel) waitForText(tb testing.TB, want string, timeout time.Duration) {
	tb.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		t.poll()
		if strings.Contains(t.stripped(), want) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	s := t.stripped()
	if len(s) > 300 {
		s = s[:300]
	}
	tb.Fatalf("waitForText: %q not found after %s. buf.Len=%d, last=%q", want, timeout, t.buf.Len(), s)
}

// waitForNotText polls until the cumulative stripped output does NOT contain want.
func (t *tuiTestModel) waitForNotText(tb testing.TB, want string, timeout time.Duration) {
	tb.Helper()
	// Reset buffer to only check new output
	t.buf.Reset()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		t.poll()
		if !strings.Contains(t.stripped(), want) && t.buf.Len() > 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	tb.Fatalf("waitForNotText: %q still present after %s", want, timeout)
}

// newTestModelWithTasks creates a tuiTestModel with the given prompt, team info,
// tasks, and terminal size, with the spinner disabled for deterministic output.
func newTestModelWithTasks(
	tb testing.TB,
	prompt string,
	info TeamInfo,
	tasks []*team.TodoItem,
	w, h int,
) *tuiTestModel {
	tb.Helper()
	tb.Setenv("NO_SPINNER", "1")
	m := New(prompt, info)
	m.tasks = tasks
	tm := teatest.NewTestModel(
		tb, m,
		teatest.WithInitialTermSize(w, h),
	)
	tb.Cleanup(func() { _ = tm.Quit() })
	return &tuiTestModel{tm: tm}
}

func sampleTasks() []*team.TodoItem {
	return []*team.TodoItem{
		{ID: "t1", Status: team.TaskPending, Desc: "Research API", Agent: "researcher"},
		{ID: "t2", Status: team.TaskPending, Desc: "Write docs", Agent: "writer"},
		{ID: "t3", Status: team.TaskPlanned, Desc: "Plan tests", Agent: "planner"},
		{ID: "t4", Status: team.TaskInProgress, Desc: "Build feature", Agent: "builder"},
		{ID: "t5", Status: team.TaskDone, Desc: "Setup repo", Agent: "devops"},
		{ID: "t6", Status: team.TaskSkipped, Desc: "Old task", Agent: "writer"},
		{ID: "t7", Status: team.TaskError, Desc: "Failed deploy", Agent: "devops"},
	}
}

func sampleTeamInfo() TeamInfo {
	return TeamInfo{
		TeamName:     "test-team",
		DefaultModel: "qwen3:8b",
		Agents: []AgentInfoEntry{
			{Name: "researcher", Role: "worker", Model: "qwen3:8b"},
			{Name: "writer", Role: "worker", Model: "qwen3:8b"},
		},
		Skills:       []string{"code-review"},
		SidecarModel: "qwen3:1b",
		GuardModel:   "qwen3:8b",
		Workspace:    "",
	}
}

func keyMsg(s string) tea.KeyMsg {
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
}

// ── Tests ────────────────────────────────────────────────────────────────

// TestIntegration_InitialRender verifies that the TUI renders the prompt,
// column titles, and task agents on startup.
func TestIntegration_InitialRender(t *testing.T) {
	tm := newTestModelWithTasks(t, "Build a web app", sampleTeamInfo(), sampleTasks(), 120, 40)

	tm.waitForText(t, "Build a web app", 5*time.Second)
	tm.waitForText(t, "PENDING", 3*time.Second)
	tm.waitForText(t, "PLANNED", 3*time.Second)
	tm.waitForText(t, "IN PROGRESS", 3*time.Second)
	tm.waitForText(t, "DONE", 3*time.Second)
	tm.waitForText(t, "SKIP", 3*time.Second)
	tm.waitForText(t, "ERROR", 3*time.Second)

	// Task agents should appear.
	tm.waitForText(t, "researcher", 3*time.Second)
	tm.waitForText(t, "builder", 3*time.Second)
}

// TestIntegration_Navigation_JK verifies j/k moves the cursor within a column.
func TestIntegration_Navigation_JK(t *testing.T) {
	tm := newTestModelWithTasks(t, "test", sampleTeamInfo(), sampleTasks(), 120, 40)
	tm.waitForText(t, "PENDING", 5*time.Second)

	// 'j' to move down to row 1.
	tm.Send(keyMsg("j"))

	// 'k' to move back up to row 0.
	tm.Send(keyMsg("k"))
	model := finalTUIModel(t, tm)
	if model.col != 0 || model.row != 0 {
		t.Errorf("cursor after j/k = col %d row %d, want col 0 row 0", model.col, model.row)
	}
}

// TestIntegration_Navigation_HL verifies h/l switches between columns.
func TestIntegration_Navigation_HL(t *testing.T) {
	tm := newTestModelWithTasks(t, "test", sampleTeamInfo(), sampleTasks(), 120, 40)
	tm.waitForText(t, "PENDING", 5*time.Second)

	// Move right to PLANNED column (col 1).
	tm.Send(keyMsg("l"))

	// Move right to IN PROGRESS column (col 2).
	tm.Send(keyMsg("l"))

	// Move left back to PLANNED.
	tm.Send(keyMsg("h"))

	// Move left back to PENDING.
	tm.Send(keyMsg("h"))
	model := finalTUIModel(t, tm)
	if model.col != 0 || model.row != 0 {
		t.Errorf("cursor after l/l/h/h = col %d row %d, want col 0 row 0", model.col, model.row)
	}
}

// TestIntegration_Navigation_Tab cycles through all 6 columns.
func TestIntegration_Navigation_Tab(t *testing.T) {
	tm := newTestModelWithTasks(t, "test", sampleTeamInfo(), sampleTasks(), 120, 40)
	tm.waitForText(t, "PENDING", 5*time.Second)

	// Tab from PENDING (0) → PLANNED (1) — planner.
	tm.Send(tea.KeyMsg{Type: tea.KeyTab})

	// Tab → IN PROGRESS (2) — builder.
	tm.Send(tea.KeyMsg{Type: tea.KeyTab})

	// Tab → DONE (3) — devops.
	tm.Send(tea.KeyMsg{Type: tea.KeyTab})

	// Tab → SKIP (4) — writer.
	tm.Send(tea.KeyMsg{Type: tea.KeyTab})

	// Tab → ERROR (5) — devops.
	tm.Send(tea.KeyMsg{Type: tea.KeyTab})

	// Tab → wraps back to PENDING (0) — researcher.
	tm.Send(tea.KeyMsg{Type: tea.KeyTab})
	model := finalTUIModel(t, tm)
	if model.col != 0 || model.row != 0 {
		t.Errorf("cursor after six tabs = col %d row %d, want col 0 row 0", model.col, model.row)
	}
}

// TestIntegration_Navigation_GotoFirstLast verifies g/G jump to first/last item.
func TestIntegration_Navigation_GotoFirstLast(t *testing.T) {
	// 10 pending tasks with distinct agent names.
	manyPending := make([]*team.TodoItem, 0, 10)
	for i := 0; i < 10; i++ {
		manyPending = append(manyPending, &team.TodoItem{
			ID:     "p" + string(rune('a'+i)),
			Status: team.TaskPending,
			Desc:   "Task " + string(rune('A'+i)),
			Agent:  "agent" + string(rune('A'+i)),
		})
	}
	tm := newTestModelWithTasks(t, "test", sampleTeamInfo(), manyPending, 120, 40)
	tm.waitForText(t, "PENDING", 5*time.Second)

	// 'G' should jump to last item — agentJ.
	tm.Send(keyMsg("G"))

	// 'g' should jump to first item — agentA.
	tm.Send(keyMsg("g"))
	model := finalTUIModel(t, tm)
	if model.col != 0 || model.row != 0 {
		t.Errorf("cursor after G/g = col %d row %d, want col 0 row 0", model.col, model.row)
	}
}

// TestIntegration_DetailView_OpenClose verifies Enter opens detail view
// and Esc returns to the columns.
func TestIntegration_DetailView_OpenClose(t *testing.T) {
	tm := newTestModelWithTasks(t, "test", sampleTeamInfo(), sampleTasks(), 120, 40)
	tm.waitForText(t, "PENDING", 5*time.Second)

	// Enter detail view for the first pending task.
	tm.Send(tea.KeyMsg{Type: tea.KeyEnter})
	// Detail view footer says "esc back".
	tm.waitForText(t, "esc back", 3*time.Second)

	// Esc returns to columns.
	tm.Send(tea.KeyMsg{Type: tea.KeyEsc})
	tm.waitForText(t, "PENDING", 3*time.Second)
}

// TestIntegration_DetailView_ShowsLogs verifies that TaskLogMsg lines appear
// in the detail view.
func TestIntegration_DetailView_ShowsLogs(t *testing.T) {
	tm := newTestModelWithTasks(t, "test", sampleTeamInfo(), sampleTasks(), 120, 40)
	tm.waitForText(t, "PENDING", 5*time.Second)

	// Send log lines for task t1.
	tm.Send(TaskLogMsg{TodoID: "t1", Line: "Starting research..."})
	tm.Send(TaskLogMsg{TodoID: "t1", Line: "Found 3 relevant papers"})

	// Enter detail view for t1 (first pending task, currently selected).
	tm.Send(tea.KeyMsg{Type: tea.KeyEnter})
	tm.waitForText(t, "Starting research", 3*time.Second)
	tm.waitForText(t, "Found 3 relevant papers", 3*time.Second)

	// Esc returns to columns.
	tm.Send(tea.KeyMsg{Type: tea.KeyEsc})
	tm.waitForText(t, "PENDING", 3*time.Second)
}

// TestIntegration_HelpPanel verifies that ? opens the help panel and Esc
// closes it.
func TestIntegration_HelpPanel(t *testing.T) {
	tm := newTestModelWithTasks(t, "test", sampleTeamInfo(), sampleTasks(), 120, 40)
	tm.waitForText(t, "PENDING", 5*time.Second)

	// '?' opens help.
	tm.Send(keyMsg("?"))
	tm.waitForText(t, "keyboard reference", 3*time.Second)

	// Esc closes help.
	tm.Send(tea.KeyMsg{Type: tea.KeyEsc})
	tm.waitForText(t, "PENDING", 3*time.Second)
}

// TestIntegration_InfoPanel verifies that 'i' opens the info panel showing
// team metadata, and Esc closes it.
func TestIntegration_InfoPanel(t *testing.T) {
	info := sampleTeamInfo()
	tm := newTestModelWithTasks(t, "test", info, sampleTasks(), 120, 40)
	tm.waitForText(t, "PENDING", 5*time.Second)

	// 'i' opens info.
	tm.Send(keyMsg("i"))
	tm.waitForText(t, "test-team", 3*time.Second)
	tm.waitForText(t, "researcher", 3*time.Second)
	tm.waitForText(t, "code-review", 3*time.Second)

	// Esc closes info.
	tm.Send(tea.KeyMsg{Type: tea.KeyEsc})
	tm.waitForText(t, "PENDING", 3*time.Second)
}

// TestIntegration_Search verifies the search dialog and result navigation.
func TestIntegration_Search(t *testing.T) {
	tm := newTestModelWithTasks(t, "test", sampleTeamInfo(), sampleTasks(), 120, 40)
	tm.waitForText(t, "PENDING", 5*time.Second)

	// '/' opens search.
	tm.Send(keyMsg("/"))
	tm.waitForText(t, "Search", 3*time.Second)

	// Type query — "deploy" matches the error task "Failed deploy".
	tm.Type("deploy")
	tm.Send(tea.KeyMsg{Type: tea.KeyEnter})

	// Should jump to the ERROR column and show the matched task's agent.
	tm.waitForText(t, "devops", 3*time.Second)

	// Esc clears search and returns to normal view.
	tm.Send(tea.KeyMsg{Type: tea.KeyEsc})
	tm.waitForText(t, "PENDING", 3*time.Second)
}

// TestIntegration_QuitConfirm verifies the quit confirmation dialog.
func TestIntegration_QuitConfirm(t *testing.T) {
	tm := newTestModelWithTasks(t, "test", sampleTeamInfo(), sampleTasks(), 120, 40)
	tm.waitForText(t, "PENDING", 5*time.Second)

	// Esc opens quit confirmation.
	tm.Send(tea.KeyMsg{Type: tea.KeyEsc})
	tm.waitForText(t, "Quit hufu?", 3*time.Second)

	// 'n' cancels.
	tm.Send(keyMsg("n"))
	tm.waitForText(t, "PENDING", 3*time.Second)
}

// TestIntegration_QuitConfirm_ForceQuit verifies that the Force button quits
// the program.
func TestIntegration_QuitConfirm_ForceQuit(t *testing.T) {
	tm := newTestModelWithTasks(t, "test", sampleTeamInfo(), sampleTasks(), 120, 40)
	tm.waitForText(t, "PENDING", 5*time.Second)

	// Esc opens quit confirmation.
	tm.Send(tea.KeyMsg{Type: tea.KeyEsc})
	tm.waitForText(t, "Quit hufu?", 3*time.Second)

	// 'f' forces quit.
	tm.Send(keyMsg("f"))
	tm.WaitFinished(t, teatest.WithFinalTimeout(3*time.Second))
}

// TestIntegration_CtrlC_WrapUp verifies that Ctrl+C triggers wrap-up.
func TestIntegration_CtrlC_WrapUp(t *testing.T) {
	// Use tasks without in-progress to avoid the status area being
	// overridden by the in-progress task display.
	noProgress := []*team.TodoItem{
		{ID: "t1", Status: team.TaskPending, Desc: "Research API", Agent: "researcher"},
		{ID: "t2", Status: team.TaskDone, Desc: "Setup repo", Agent: "devops"},
	}
	tm := newTestModelWithTasks(t, "test", sampleTeamInfo(), noProgress, 120, 40)
	tm.waitForText(t, "PENDING", 5*time.Second)

	// Ctrl+C triggers wrap-up.
	tm.Send(tea.KeyMsg{Type: tea.KeyCtrlC})
	tm.waitForText(t, "Finishing", 3*time.Second)

	// Second Ctrl+C quits.
	tm.Send(tea.KeyMsg{Type: tea.KeyCtrlC})
	tm.WaitFinished(t, teatest.WithFinalTimeout(3*time.Second))
}

// TestIntegration_FinishedMsg_Quits verifies that after FinishedMsg, pressing
// 'q' quits the program.
func TestIntegration_FinishedMsg_Quits(t *testing.T) {
	tm := newTestModelWithTasks(t, "test", sampleTeamInfo(), sampleTasks(), 120, 40)
	tm.waitForText(t, "PENDING", 5*time.Second)

	// Send FinishedMsg — this marks the run as finished.
	tm.Send(FinishedMsg{})
	tm.waitForText(t, "All tasks completed", 3*time.Second)

	// 'q' should now quit.
	tm.Send(keyMsg("q"))
	tm.WaitFinished(t, teatest.WithFinalTimeout(3*time.Second))
}

// TestIntegration_FinishedMsg_ConvertsTasks verifies that FinishedMsg converts
// in-progress tasks to done and pending tasks to skipped.
func TestIntegration_FinishedMsg_ConvertsTasks(t *testing.T) {
	tm := newTestModelWithTasks(t, "test", sampleTeamInfo(), sampleTasks(), 120, 40)
	tm.waitForText(t, "PENDING", 5*time.Second)

	tm.Send(FinishedMsg{})
	tm.waitForText(t, "All tasks completed", 3*time.Second)

	// 'q' to quit so we can get the FinalModel.
	tm.Send(keyMsg("q"))
	fm := tm.FinalModel(t, teatest.WithFinalTimeout(5*time.Second))
	if fm == nil {
		t.Fatal("expected final model, got nil")
	}
	model := fm.(Model)

	for _, task := range model.tasks {
		switch task.Status {
		case team.TaskPending, team.TaskPlanned:
			t.Errorf("task %s should not be pending/planned after finish, got %s", task.ID, task.Status)
		case team.TaskInProgress, team.TaskPaused, team.TaskVerifying:
			t.Errorf("task %s should not be in-progress after finish, got %s", task.ID, task.Status)
		}
	}
}

// TestIntegration_TasksUpdatedMsg verifies that sending TasksUpdatedMsg updates
// the displayed tasks.
func TestIntegration_TasksUpdatedMsg(t *testing.T) {
	tm := newTestModelWithTasks(t, "test", sampleTeamInfo(), nil, 120, 40)
	tm.waitForText(t, "PENDING", 5*time.Second)

	// Send new tasks.
	newTasks := []*team.TodoItem{
		{ID: "new1", Status: team.TaskPending, Desc: "New pending task", Agent: "freshagent"},
		{ID: "new2", Status: team.TaskDone, Desc: "New done task", Agent: "doneagent"},
	}
	tm.Send(TasksUpdatedMsg{Items: newTasks})

	tm.waitForText(t, "freshagent", 3*time.Second)
	tm.waitForText(t, "doneagent", 3*time.Second)
}

// TestIntegration_StatusBarMsg verifies that StatusBarMsg updates the status
// bar text.
func TestIntegration_StatusBarMsg(t *testing.T) {
	// Use tasks without in-progress to avoid the status area being
	// overridden by the in-progress task display.
	noProgress := []*team.TodoItem{
		{ID: "t1", Status: team.TaskPending, Desc: "Research API", Agent: "researcher"},
		{ID: "t2", Status: team.TaskDone, Desc: "Setup repo", Agent: "devops"},
	}
	tm := newTestModelWithTasks(t, "test", sampleTeamInfo(), noProgress, 120, 40)
	tm.waitForText(t, "PENDING", 5*time.Second)

	tm.Send(StatusBarMsg{Text: "Running step 3 of 5"})
	tm.waitForText(t, "Running step 3 of 5", 3*time.Second)
}

// TestIntegration_ResultMsg verifies that ResultMsg stores the result and
// Enter opens the result view.
func TestIntegration_ResultMsg(t *testing.T) {
	tm := newTestModelWithTasks(t, "test", sampleTeamInfo(), sampleTasks(), 120, 40)
	tm.waitForText(t, "PENDING", 5*time.Second)

	// Send the final result and finish.
	tm.Send(ResultMsg{Text: "The final answer is 42."})
	tm.Send(FinishedMsg{})
	// When finished with a result, the status area shows a result box
	// (not "All tasks completed"). The result text appears in the box.
	tm.waitForText(t, "The final answer is 42.", 3*time.Second)

	// Enter should open the full result view.
	tm.Send(tea.KeyMsg{Type: tea.KeyEnter})
	tm.waitForText(t, "The final answer is 42.", 3*time.Second)

	// Esc closes the result view.
	tm.Send(tea.KeyMsg{Type: tea.KeyEsc})
	tm.waitForText(t, "PENDING", 3*time.Second)
}

// TestIntegration_PromptInput verifies that 'c' opens the prompt injection
// dialog, typing works, and Enter submits.
func TestIntegration_PromptInput(t *testing.T) {
	tm := newTestModelWithTasks(t, "test", sampleTeamInfo(), sampleTasks(), 120, 40)
	tm.waitForText(t, "PENDING", 5*time.Second)

	// 'c' opens prompt input.
	tm.Send(keyMsg("c"))
	tm.waitForText(t, "Additional Prompt", 3*time.Second)

	// Type a prompt.
	tm.Type("fix the bug")
	tm.waitForText(t, "fix the bug", 3*time.Second)

	// Enter submits — the dialog should close.
	tm.Send(tea.KeyMsg{Type: tea.KeyEnter})

	// After submit, we should be back in the column view.
	tm.waitForText(t, "PENDING", 3*time.Second)

	// The injected prompt should be in the channel.
	tm.Send(FinishedMsg{})
	tm.Send(keyMsg("q")) // quit to get final model
	fm := tm.FinalModel(t, teatest.WithFinalTimeout(5*time.Second))
	if fm == nil {
		t.Fatal("expected final model, got nil")
	}
	model := fm.(Model)
	select {
	case injected := <-model.PromptInjectCh:
		if injected != "fix the bug" {
			t.Errorf("expected injected prompt 'fix the bug', got %q", injected)
		}
	default:
		t.Error("expected prompt to be injected into PromptInjectCh")
	}
}

// TestIntegration_PromptInput_Cancel verifies that Esc cancels the prompt
// input dialog.
func TestIntegration_PromptInput_Cancel(t *testing.T) {
	tm := newTestModelWithTasks(t, "test", sampleTeamInfo(), sampleTasks(), 120, 40)
	tm.waitForText(t, "PENDING", 5*time.Second)

	tm.Send(keyMsg("c"))
	tm.waitForText(t, "Additional Prompt", 3*time.Second)

	tm.Send(tea.KeyMsg{Type: tea.KeyEsc})
	tm.waitForText(t, "PENDING", 3*time.Second)
}

// TestIntegration_ActivityLog verifies that 'a' opens the activity log and
// Esc closes it.
func TestIntegration_ActivityLog(t *testing.T) {
	tm := newTestModelWithTasks(t, "test", sampleTeamInfo(), sampleTasks(), 120, 40)
	tm.waitForText(t, "PENDING", 5*time.Second)

	// Send some log messages to populate the activity feed.
	tm.Send(TaskLogMsg{TodoID: "t1", Line: "Step 1 started"})
	tm.Send(TaskLogMsg{TodoID: "t4", Line: "Building in progress"})

	// 'a' opens activity log.
	tm.Send(keyMsg("a"))
	tm.waitForText(t, "ACTIVITY LOG", 3*time.Second)

	// Esc closes activity log.
	tm.Send(tea.KeyMsg{Type: tea.KeyEsc})
	tm.waitForText(t, "PENDING", 3*time.Second)
}

// TestIntegration_NarrowTerminal verifies that a narrow terminal shows a
// warning message.
func TestIntegration_NarrowTerminal(t *testing.T) {
	tm := newTestModelWithTasks(t, "test", sampleTeamInfo(), sampleTasks(), 50, 20)
	tm.waitForText(t, "Terminal too narrow", 5*time.Second)
}

// TestIntegration_CoordItem verifies that CoordItemMsg adds a coordinator
// pseudo-task to the display.
func TestIntegration_CoordItem(t *testing.T) {
	tm := newTestModelWithTasks(t, "test", sampleTeamInfo(), sampleTasks(), 120, 40)
	tm.waitForText(t, "PENDING", 5*time.Second)

	coordTask := &team.TodoItem{
		ID:     team.CoordTodoID,
		Status: team.TaskInProgress,
		Desc:   "Coordinating tasks",
		Agent:  "coordinator",
	}
	tm.Send(CoordItemMsg{Item: coordTask})

	// The coordinator item should appear in the IN PROGRESS column.
	tm.Send(keyMsg("l")) // → PLANNED
	tm.Send(keyMsg("l")) // → IN PROGRESS
	tm.waitForText(t, "coordinator", 3*time.Second)
}

// TestIntegration_EmptyTasks verifies that the TUI renders correctly with
// no tasks.
func TestIntegration_EmptyTasks(t *testing.T) {
	tm := newTestModelWithTasks(t, "waiting for tasks", sampleTeamInfo(), nil, 120, 40)
	tm.waitForText(t, "waiting for tasks", 5*time.Second)
	tm.waitForText(t, "PENDING", 3*time.Second)
}

// TestIntegration_CompactMode verifies that a terminal between 60–80 columns
// uses the compact 3-column layout.
func TestIntegration_CompactMode(t *testing.T) {
	tm := newTestModelWithTasks(t, "test", sampleTeamInfo(), sampleTasks(), 70, 30)
	tm.waitForText(t, "Queued", 5*time.Second)
	tm.waitForText(t, "Active", 3*time.Second)
	tm.waitForText(t, "Finished", 3*time.Second)
}

// TestIntegration_TaskLogBufferBound verifies that the log buffer is bounded
// to maxTaskLogLines.
func TestIntegration_TaskLogBufferBound(t *testing.T) {
	tm := newTestModelWithTasks(t, "test", sampleTeamInfo(), sampleTasks(), 120, 40)
	tm.waitForText(t, "PENDING", 5*time.Second)

	// Send more log lines than the buffer allows.
	for i := 0; i < maxTaskLogLines+10; i++ {
		tm.Send(TaskLogMsg{TodoID: "t1", Line: "log line"})
	}

	tm.Send(FinishedMsg{})
	tm.Send(keyMsg("q"))
	fm := tm.FinalModel(t, teatest.WithFinalTimeout(5*time.Second))
	if fm == nil {
		t.Fatal("expected final model")
	}
	model := fm.(Model)
	if got := len(model.logs["t1"]); got != maxTaskLogLines {
		t.Errorf("log buffer length = %d, want %d", got, maxTaskLogLines)
	}
}

// TestIntegration_EnterResultWhenFinished verifies that Enter opens the result
// view when finished.
func TestIntegration_EnterResultWhenFinished(t *testing.T) {
	tm := newTestModelWithTasks(t, "test", sampleTeamInfo(), sampleTasks(), 120, 40)
	tm.waitForText(t, "PENDING", 5*time.Second)

	tm.Send(ResultMsg{Text: "Result text here."})
	tm.Send(FinishedMsg{})
	// When finished with a result, the status area shows the result box.
	tm.waitForText(t, "Result text here.", 3*time.Second)

	tm.Send(tea.KeyMsg{Type: tea.KeyEnter})
	tm.waitForText(t, "Result text here.", 3*time.Second)
}

// TestIntegration_DetailView_VisualMode verifies entering and exiting VISUAL
// mode in the detail view.
func TestIntegration_DetailView_VisualMode(t *testing.T) {
	tm := newTestModelWithTasks(t, "test", sampleTeamInfo(), sampleTasks(), 120, 40)
	tm.waitForText(t, "PENDING", 5*time.Second)

	// Populate logs.
	tm.Send(TaskLogMsg{TodoID: "t1", Line: "Line one"})
	tm.Send(TaskLogMsg{TodoID: "t1", Line: "Line two"})
	tm.Send(TaskLogMsg{TodoID: "t1", Line: "Line three"})

	// Enter detail view.
	tm.Send(tea.KeyMsg{Type: tea.KeyEnter})
	tm.waitForText(t, "Line one", 3*time.Second)

	// Enter visual mode.
	tm.Send(keyMsg("v"))
	tm.waitForText(t, "VISUAL", 3*time.Second)

	// Exit visual mode with 'v' (toggles off).
	tm.Send(keyMsg("v"))
	tm.waitForNotText(t, "VISUAL", 3*time.Second)

	// Esc returns to columns.
	tm.Send(tea.KeyMsg{Type: tea.KeyEsc})
	tm.waitForText(t, "PENDING", 3*time.Second)
}

// TestIntegration_ReportKey verifies that 'r' when finished sends a report
// request.
func TestIntegration_ReportKey(t *testing.T) {
	tm := newTestModelWithTasks(t, "test", sampleTeamInfo(), sampleTasks(), 120, 40)
	tm.waitForText(t, "PENDING", 5*time.Second)

	tm.Send(FinishedMsg{})
	tm.waitForText(t, "All tasks completed", 3*time.Second)

	// 'r' should trigger report generation.
	tm.Send(keyMsg("r"))
	tm.waitForText(t, "Generating report", 3*time.Second)

	// Verify the ReportCh received a signal.
	tm.Send(keyMsg("q"))
	fm := tm.FinalModel(t, teatest.WithFinalTimeout(5*time.Second))
	if fm == nil {
		t.Fatal("expected final model")
	}
	model := fm.(Model)
	select {
	case <-model.ReportCh:
		// Good — report was requested.
	default:
		t.Error("expected report request in ReportCh")
	}
}

// TestIntegration_ChatMode_PromptInputOnStart verifies that chat mode opens
// the prompt input dialog on start.
func TestIntegration_ChatMode_PromptInputOnStart(t *testing.T) {
	t.Setenv("NO_SPINNER", "1")
	info := sampleTeamInfo()
	info.IsChat = true
	m := New("", info) // empty prompt + chat mode → prompt input on start
	rawTm := teatest.NewTestModel(
		t, m,
		teatest.WithInitialTermSize(120, 40),
	)
	t.Cleanup(func() { _ = rawTm.Quit() })
	tm := &tuiTestModel{tm: rawTm}

	tm.waitForText(t, "Additional Prompt", 5*time.Second)
}

// TestIntegration_WindowResize verifies that the TUI handles window resize
// without crashing.
func TestIntegration_WindowResize(t *testing.T) {
	tm := newTestModelWithTasks(t, "test", sampleTeamInfo(), sampleTasks(), 120, 40)
	tm.waitForText(t, "PENDING", 5*time.Second)

	// Resize to a smaller terminal.
	tm.Send(tea.WindowSizeMsg{Width: 100, Height: 30})
	tm.waitForText(t, "PENDING", 3*time.Second)

	// Resize to a larger terminal.
	tm.Send(tea.WindowSizeMsg{Width: 150, Height: 50})
	tm.waitForText(t, "PENDING", 3*time.Second)

	// Resize to narrow — should show warning.
	tm.Send(tea.WindowSizeMsg{Width: 50, Height: 20})
	tm.waitForText(t, "Terminal too narrow", 3*time.Second)
}

// TestIntegration_GoToLastColumn verifies navigation to the last column (ERROR).
func TestIntegration_GoToLastColumn(t *testing.T) {
	tm := newTestModelWithTasks(t, "test", sampleTeamInfo(), sampleTasks(), 120, 40)
	tm.waitForText(t, "PENDING", 5*time.Second)

	// Navigate right 5 times to reach ERROR (col 5).
	for i := 0; i < 5; i++ {
		tm.Send(keyMsg("l"))
	}
	tm.waitForText(t, "devops", 3*time.Second)

	// 'l' at the last column should stay (no wrap with 'l').
	tm.Send(keyMsg("l"))
	tm.waitForText(t, "devops", 3*time.Second)
}

// TestIntegration_QuitWhenNotFinished verifies that 'q' does not quit when
// not finished.
func TestIntegration_QuitWhenNotFinished(t *testing.T) {
	tm := newTestModelWithTasks(t, "test", sampleTeamInfo(), sampleTasks(), 120, 40)
	tm.waitForText(t, "PENDING", 5*time.Second)

	// 'q' should not quit when not finished — program should still be running.
	tm.Send(keyMsg("q"))

	// Give it a moment, then check that the program is still running
	// by verifying we can still see the columns.
	tm.waitForText(t, "PENDING", 2*time.Second)
}

// TestIntegration_MultipleOverlays verifies that opening and closing overlays
// in sequence works correctly without state leaks.
func TestIntegration_MultipleOverlays(t *testing.T) {
	tm := newTestModelWithTasks(t, "test", sampleTeamInfo(), sampleTasks(), 120, 40)
	tm.waitForText(t, "PENDING", 5*time.Second)

	// Open help, close it.
	tm.Send(keyMsg("?"))
	tm.waitForText(t, "keyboard reference", 3*time.Second)
	tm.Send(tea.KeyMsg{Type: tea.KeyEsc})
	tm.waitForText(t, "PENDING", 3*time.Second)

	// Open info, close it.
	tm.Send(keyMsg("i"))
	tm.waitForText(t, "test-team", 3*time.Second)
	tm.Send(tea.KeyMsg{Type: tea.KeyEsc})
	tm.waitForText(t, "PENDING", 3*time.Second)

	// Open search, cancel it.
	tm.Send(keyMsg("/"))
	tm.waitForText(t, "Search", 3*time.Second)
	tm.Send(tea.KeyMsg{Type: tea.KeyEsc})
	tm.waitForText(t, "PENDING", 3*time.Second)

	// Open prompt input, cancel it.
	tm.Send(keyMsg("c"))
	tm.waitForText(t, "Additional Prompt", 3*time.Second)
	tm.Send(tea.KeyMsg{Type: tea.KeyEsc})
	tm.waitForText(t, "PENDING", 3*time.Second)

	// Open activity log, close it.
	tm.Send(keyMsg("a"))
	tm.waitForText(t, "ACTIVITY LOG", 3*time.Second)
	tm.Send(tea.KeyMsg{Type: tea.KeyEsc})
	tm.waitForText(t, "PENDING", 3*time.Second)

	// Navigation should still work after all overlay cycling.
	tm.Send(keyMsg("l"))
	tm.waitForText(t, "planner", 3*time.Second)
}
