package team

import (
	"testing"
	"time"
)

func TestStatusEventWithAgent(t *testing.T) {
	event := StatusEvent{Type: "start", TeamName: "test-team"}
	event = event.withAgent("test-agent")

	if event.Agent != "test-agent" {
		t.Errorf("Agent = %q, want %q", event.Agent, "test-agent")
	}
	if event.Type != "start" {
		t.Errorf("Type = %q, want %q", event.Type, "start")
	}
}

func TestStatusEventWithMessage(t *testing.T) {
	event := StatusEvent{Type: "error", TeamName: "test-team", Message: "initial"}
	event = event.withMessage("new message")

	if event.Message != "new message" {
		t.Errorf("Message = %q, want %q", event.Message, "new message")
	}
}

func TestStatusEventWithStep(t *testing.T) {
	event := StatusEvent{Type: "step", TeamName: "test-team"}
	event = event.withStep(5)

	if event.Step != 5 {
		t.Errorf("Step = %d, want %d", event.Step, 5)
	}
}

func TestStatusEventWithTool(t *testing.T) {
	event := StatusEvent{Type: "tool_call", TeamName: "test-team"}
	event = event.withTool("bash", "ls -la")

	if event.ToolName != "bash" {
		t.Errorf("ToolName = %q, want %q", event.ToolName, "bash")
	}
	if event.ToolArgs != "ls -la" {
		t.Errorf("ToolArgs = %q, want %q", event.ToolArgs, "ls -la")
	}
}

func TestStatusEventWithToolResult(t *testing.T) {
	event := StatusEvent{Type: "tool_result", TeamName: "test-team"}
	event = event.withToolResult("bash", "output")

	if event.ToolName != "bash" {
		t.Errorf("ToolName = %q, want %q", event.ToolName, "bash")
	}
	if event.ToolResult != "output" {
		t.Errorf("ToolResult = %q, want %q", event.ToolResult, "output")
	}
}

func TestStatusEventWithTodos(t *testing.T) {
	todos := []*TodoItem{
		{ID: "1", Agent: "agent1", Desc: "task1", Status: TaskPending},
		{ID: "2", Agent: "agent2", Desc: "task2", Status: TaskPending},
	}
	event := StatusEvent{Type: "todos_updated", TeamName: "test-team"}
	event = event.withTodos(todos)

	if len(event.Todos) != 2 {
		t.Errorf("len(Todos) = %d, want %d", len(event.Todos), 2)
	}
	if event.Todos[0].ID != "1" {
		t.Errorf("Todos[0].ID = %q, want %q", event.Todos[0].ID, "1")
	}
}

func TestStatusEventWithSkillName(t *testing.T) {
	event := StatusEvent{Type: "skill_used", TeamName: "test-team"}
	event = event.withSkillName("test-skill")

	if event.SkillName != "test-skill" {
		t.Errorf("SkillName = %q, want %q", event.SkillName, "test-skill")
	}
}

func TestStatusEventWithModel(t *testing.T) {
	event := StatusEvent{Type: "start", TeamName: "test-team"}
	event = event.withModel("ollama/qwen3:8b")

	if event.Model != "ollama/qwen3:8b" {
		t.Errorf("Model = %q, want %q", event.Model, "ollama/qwen3:8b")
	}
}

func TestStatusEventWithTiming(t *testing.T) {
	event := StatusEvent{Type: "done", TeamName: "test-team"}
	event = event.withTiming(2*time.Minute+30*time.Second, 1*time.Minute+45*time.Second, 45*time.Second)

	if event.Duration != 150*time.Second {
		t.Errorf("Duration = %v, want %v", event.Duration, 150*time.Second)
	}
	if event.ModelTime != 105*time.Second {
		t.Errorf("ModelTime = %v, want %v", event.ModelTime, 105*time.Second)
	}
	if event.ToolTime != 45*time.Second {
		t.Errorf("ToolTime = %v, want %v", event.ToolTime, 45*time.Second)
	}
}

func TestStatusEventLoopWarning(t *testing.T) {
	event := StatusEvent{
		Type:     "loop_warning",
		TeamName: "test-team",
		Message:  "Duplicate task delegation detected",
	}

	if event.Type != "loop_warning" {
		t.Errorf("Type = %q, want %q", event.Type, "loop_warning")
	}
	if event.Message != "Duplicate task delegation detected" {
		t.Errorf("Message = %q, want %q", event.Message, "Duplicate task delegation detected")
	}
}

func TestStatusEventLoopWarningWithMessage(t *testing.T) {
	event := StatusEvent{
		Type:     "loop_warning",
		TeamName: "test-team",
	}
	event = event.withMessage("This is a loop warning message")

	if event.Message != "This is a loop warning message" {
		t.Errorf("Message = %q, want %q", event.Message, "This is a loop warning message")
	}
}

func TestTaskTrackerNewTaskTracker(t *testing.T) {
	tracker := NewTaskTracker()

	if tracker == nil {
		t.Fatal("NewTaskTracker() returned nil")
	}

	todoList := tracker.TodoList()
	if todoList == nil {
		t.Fatal("TodoList() returned nil")
	}
}

func TestTodoListAddBatchStatus(t *testing.T) {
	tl := &TodoList{}

	items := []struct {
		Agent    string
		Desc     string
		Model    string
		Source   string
		ParentID string
	}{
		{Agent: "agent1", Desc: "task1"},
		{Agent: "agent2", Desc: "task2"},
		{Agent: "agent3", Desc: "task3"},
	}

	added := tl.AddBatch(items)

	if len(added) != 3 {
		t.Errorf("len(added) = %d, want %d", len(added), 3)
	}

	for i, item := range added {
		if item.ID != string(rune('0'+i+1)) {
			t.Errorf("Item %d ID = %q, want %q", i, item.ID, string(rune('0'+i+1)))
		}
		if item.Agent != items[i].Agent {
			t.Errorf("Item %d Agent = %q, want %q", i, item.Agent, items[i].Agent)
		}
		if item.Desc != items[i].Desc {
			t.Errorf("Item %d Desc = %q, want %q", i, item.Desc, items[i].Desc)
		}
		if item.Status != TaskPending {
			t.Errorf("Item %d Status = %q, want %q", i, item.Status, TaskPending)
		}
	}
}

func TestTodoListUpdateStatusStatus(t *testing.T) {
	tl := &TodoList{}

	items := []struct {
		Agent    string
		Desc     string
		Model    string
		Source   string
		ParentID string
	}{
		{Agent: "agent1", Desc: "task1"},
		{Agent: "agent2", Desc: "task2"},
	}

	tl.AddBatch(items)

	tl.UpdateStatus("1", TaskInProgress, "working on it")
	tl.UpdateStatus("2", TaskError, "failed")

	todos := tl.Items()
	if len(todos) != 2 {
		t.Errorf("len(todos) = %d, want %d", len(todos), 2)
	}

	if todos[0].Status != TaskInProgress {
		t.Errorf("todos[0].Status = %q, want %q", todos[0].Status, TaskInProgress)
	}
	if todos[0].Detail != "working on it" {
		t.Errorf("todos[0].Detail = %q, want %q", todos[0].Detail, "working on it")
	}

	if todos[1].Status != TaskError {
		t.Errorf("todos[1].Status = %q, want %q", todos[1].Status, TaskError)
	}
	if todos[1].Detail != "failed" {
		t.Errorf("todos[1].Detail = %q, want %q", todos[1].Detail, "failed")
	}
}

func TestTodoListUpdateStatusNonExistent(t *testing.T) {
	tl := &TodoList{}

	items := []struct {
		Agent    string
		Desc     string
		Model    string
		Source   string
		ParentID string
	}{
		{Agent: "agent1", Desc: "task1"},
	}

	tl.AddBatch(items)

	tl.UpdateStatus("999", TaskDone, "should not update")

	todos := tl.Items()
	if len(todos) != 1 {
		t.Errorf("len(todos) = %d, want %d", len(todos), 1)
	}
	if todos[0].Status != TaskPending {
		t.Errorf("todos[0].Status = %q, want %q (should not change)", todos[0].Status, TaskPending)
	}
}

func TestTodoListItems(t *testing.T) {
	tl := &TodoList{}

	items := []struct {
		Agent    string
		Desc     string
		Model    string
		Source   string
		ParentID string
	}{
		{Agent: "agent1", Desc: "task1"},
	}

	tl.AddBatch(items)

	todos := tl.Items()
	if len(todos) != 1 {
		t.Errorf("len(todos) = %d, want %d", len(todos), 1)
	}

	todos[0].Status = TaskDone
	todos = tl.Items()
	if todos[0].Status != TaskPending {
		t.Errorf("todos[0].Status = %q, want %q (should not be modified)", todos[0].Status, TaskPending)
	}
}

func TestTodoListClearStatus(t *testing.T) {
	tl := &TodoList{}

	items := []struct {
		Agent    string
		Desc     string
		Model    string
		Source   string
		ParentID string
	}{
		{Agent: "agent1", Desc: "task1"},
		{Agent: "agent2", Desc: "task2"},
	}

	tl.AddBatch(items)

	if len(tl.Items()) != 2 {
		t.Errorf("len(todos) = %d, want %d", len(tl.Items()), 2)
	}

	tl.Clear()

	if len(tl.Items()) != 0 {
		t.Errorf("len(todos) = %d, want %d", len(tl.Items()), 0)
	}
}

func TestTodoListNextCounter(t *testing.T) {
	tl := &TodoList{}

	tl.AddBatch([]struct {
		Agent    string
		Desc     string
		Model    string
		Source   string
		ParentID string
	}{
		{Agent: "agent1", Desc: "task1"},
		{Agent: "agent2", Desc: "task2"},
	})

	tl.Clear()
	tl.AddBatch([]struct {
		Agent    string
		Desc     string
		Model    string
		Source   string
		ParentID string
	}{
		{Agent: "agent3", Desc: "task3"},
	})

	todos := tl.Items()
	if len(todos) != 1 {
		t.Errorf("len(todos) = %d, want %d", len(todos), 1)
	}
	if todos[0].ID != "1" {
		t.Errorf("todos[0].ID = %q, want %q", todos[0].ID, "1")
	}
}

func TestTaskStatusConstants(t *testing.T) {
	if TaskPending != "pending" {
		t.Errorf("TaskPending = %q, want %q", TaskPending, "pending")
	}
	if TaskInProgress != "in_progress" {
		t.Errorf("TaskInProgress = %q, want %q", TaskInProgress, "in_progress")
	}
	if TaskDone != "done" {
		t.Errorf("TaskDone = %q, want %q", TaskDone, "done")
	}
	if TaskError != "error" {
		t.Errorf("TaskError = %q, want %q", TaskError, "error")
	}
	if TaskSkipped != "skipped" {
		t.Errorf("TaskSkipped = %q, want %q", TaskSkipped, "skipped")
	}
}

func TestTodoItemFields(t *testing.T) {
	now := time.Now()
	item := &TodoItem{
		ID:        "1",
		Agent:     "test-agent",
		Desc:      "test task",
		Status:    TaskPending,
		Detail:    "test detail",
		Model:     "ollama/qwen3:8b",
		StartedAt: now,
		EndedAt:   now.Add(5 * time.Minute),
		ModelTime: 3 * time.Minute,
		ToolTime:  2 * time.Minute,
		Source:    TaskSourceCoordinator,
		ParentID:  "",
	}

	if item.ID != "1" {
		t.Errorf("ID = %q, want %q", item.ID, "1")
	}
	if item.Agent != "test-agent" {
		t.Errorf("Agent = %q, want %q", item.Agent, "test-agent")
	}
	if item.Model != "ollama/qwen3:8b" {
		t.Errorf("Model = %q, want %q", item.Model, "ollama/qwen3:8b")
	}
	if item.ModelTime != 3*time.Minute {
		t.Errorf("ModelTime = %v, want %v", item.ModelTime, 3*time.Minute)
	}
	if item.ToolTime != 2*time.Minute {
		t.Errorf("ToolTime = %v, want %v", item.ToolTime, 2*time.Minute)
	}
}

func TestTodoItemStartedAtEndedAt(t *testing.T) {
	tl := &TodoList{}
	tl.AddBatch([]struct {
		Agent    string
		Desc     string
		Model    string
		Source   string
		ParentID string
	}{{Agent: "agent", Desc: "task", Model: "ollama/qwen3:8b"}})

	items := tl.Items()
	if !items[0].StartedAt.IsZero() {
		t.Errorf("StartedAt should be zero before TaskInProgress, got %v", items[0].StartedAt)
	}

	tl.UpdateStatus("1", TaskInProgress, "")
	items = tl.Items()
	if items[0].StartedAt.IsZero() {
		t.Error("StartedAt should be set after TaskInProgress")
	}
	if !items[0].EndedAt.IsZero() {
		t.Error("EndedAt should still be zero")
	}

	tl.UpdateStatus("1", TaskDone, "")
	items = tl.Items()
	if items[0].EndedAt.IsZero() {
		t.Error("EndedAt should be set after TaskDone")
	}
}

func TestTodoListUpdateTodoTiming(t *testing.T) {
	tl := &TodoList{}
	tl.AddBatch([]struct {
		Agent    string
		Desc     string
		Model    string
		Source   string
		ParentID string
	}{{Agent: "agent", Desc: "task"}})

	tl.UpdateTodoTiming("1", 3*time.Minute, 1*time.Minute)
	items := tl.Items()
	if items[0].ModelTime != 3*time.Minute {
		t.Errorf("ModelTime = %v, want %v", items[0].ModelTime, 3*time.Minute)
	}
	if items[0].ToolTime != 1*time.Minute {
		t.Errorf("ToolTime = %v, want %v", items[0].ToolTime, 1*time.Minute)
	}
}

func TestTaskSkippedEndedAt(t *testing.T) {
	tl := &TodoList{}
	tl.AddBatch([]struct {
		Agent    string
		Desc     string
		Model    string
		Source   string
		ParentID string
	}{{Agent: "test-agent", Desc: "test task"}})
	item := tl.Items()[0]
	tl.UpdateStatus(item.ID, TaskSkipped, "not executed")
	updated := tl.Items()[0]
	if updated.EndedAt.IsZero() {
		t.Error("TaskSkipped should set EndedAt")
	}
	if updated.Status != TaskSkipped {
		t.Errorf("expected TaskSkipped, got %s", updated.Status)
	}
	if updated.Detail != "not executed" {
		t.Errorf("expected 'not executed', got %q", updated.Detail)
	}
}
