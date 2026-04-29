package team

import (
	"testing"
)

// TestStatusEventWithAgent tests the withAgent method
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

// TestStatusEventWithMessage tests the withMessage method
func TestStatusEventWithMessage(t *testing.T) {
	event := StatusEvent{Type: "error", TeamName: "test-team", Message: "initial"}
	event = event.withMessage("new message")

	if event.Message != "new message" {
		t.Errorf("Message = %q, want %q", event.Message, "new message")
	}
}

// TestStatusEventWithStep tests the withStep method
func TestStatusEventWithStep(t *testing.T) {
	event := StatusEvent{Type: "step", TeamName: "test-team"}
	event = event.withStep(5)

	if event.Step != 5 {
		t.Errorf("Step = %d, want %d", event.Step, 5)
	}
}

// TestStatusEventWithTool tests the withTool method
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

// TestStatusEventWithToolResult tests the withToolResult method
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

// TestStatusEventWithTodos tests the withTodos method
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

// TestStatusEventWithSkillName tests the withSkillName method
func TestStatusEventWithSkillName(t *testing.T) {
	event := StatusEvent{Type: "skill_used", TeamName: "test-team"}
	event = event.withSkillName("test-skill")

	if event.SkillName != "test-skill" {
		t.Errorf("SkillName = %q, want %q", event.SkillName, "test-skill")
	}
}

// TestStatusEventLoopWarning tests the loop_warning event type
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

// TestStatusEventLoopWarningWithMessage tests loop_warning with custom message
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

// TestTaskTrackerNewTaskTracker tests NewTaskTracker
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

// TestTodoListAddBatchStatus tests TodoList.AddBatch with status tracking
func TestTodoListAddBatchStatus(t *testing.T) {
	tl := &TodoList{}

	items := []struct {
		Agent string
		Desc  string
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

// TestTodoListUpdateStatusStatus tests TodoList.UpdateStatus with status tracking
func TestTodoListUpdateStatusStatus(t *testing.T) {
	tl := &TodoList{}

	items := []struct {
		Agent string
		Desc  string
	}{
		{Agent: "agent1", Desc: "task1"},
		{Agent: "agent2", Desc: "task2"},
	}

	tl.AddBatch(items)

	// Update first item
	tl.UpdateStatus("1", TaskInProgress, "working on it")

	// Update second item with error
	tl.UpdateStatus("2", TaskError, "failed")

	// Verify updates
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

// TestTodoListUpdateStatusNonExistent tests UpdateStatus with non-existent ID
func TestTodoListUpdateStatusNonExistent(t *testing.T) {
	tl := &TodoList{}

	items := []struct {
		Agent string
		Desc  string
	}{
		{Agent: "agent1", Desc: "task1"},
	}

	tl.AddBatch(items)

	// Try to update non-existent ID
	tl.UpdateStatus("999", TaskDone, "should not update")

	todos := tl.Items()
	if len(todos) != 1 {
		t.Errorf("len(todos) = %d, want %d", len(todos), 1)
	}
	if todos[0].Status != TaskPending {
		t.Errorf("todos[0].Status = %q, want %q (should not change)", todos[0].Status, TaskPending)
	}
}

// TestTodoListItems tests TodoList.Items
func TestTodoListItems(t *testing.T) {
	tl := &TodoList{}

	items := []struct {
		Agent string
		Desc  string
	}{
		{Agent: "agent1", Desc: "task1"},
	}

	tl.AddBatch(items)

	todos := tl.Items()
	if len(todos) != 1 {
		t.Errorf("len(todos) = %d, want %d", len(todos), 1)
	}

	// Modify returned slice should not affect internal state
	todos[0].Status = TaskDone
	todos = tl.Items()
	if todos[0].Status != TaskPending {
		t.Errorf("todos[0].Status = %q, want %q (should not be modified)", todos[0].Status, TaskPending)
	}
}

// TestTodoListClearStatus tests TodoList.Clear with status tracking
func TestTodoListClearStatus(t *testing.T) {
	tl := &TodoList{}

	items := []struct {
		Agent string
		Desc  string
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

// TestTodoListNextCounter tests that next counter resets correctly after Clear
func TestTodoListNextCounter(t *testing.T) {
	tl := &TodoList{}

	// Add some items
	tl.AddBatch([]struct {
		Agent string
		Desc  string
	}{
		{Agent: "agent1", Desc: "task1"},
		{Agent: "agent2", Desc: "task2"},
	})

	// Clear and add more
	tl.Clear()
	tl.AddBatch([]struct {
		Agent string
		Desc  string
	}{
		{Agent: "agent3", Desc: "task3"},
	})

	// The next ID should be 1 (reset after clear)
	todos := tl.Items()
	if len(todos) != 1 {
		t.Errorf("len(todos) = %d, want %d", len(todos), 1)
	}
	if todos[0].ID != "1" {
		t.Errorf("todos[0].ID = %q, want %q", todos[0].ID, "1")
	}
}

// TestTaskStatusConstants tests that all TaskStatus constants are defined
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
}

// TestTodoItemFields tests TodoItem structure
func TestTodoItemFields(t *testing.T) {
	item := &TodoItem{
		ID:     "1",
		Agent:  "test-agent",
		Desc:   "test task",
		Status: TaskPending,
		Detail: "test detail",
	}

	if item.ID != "1" {
		t.Errorf("ID = %q, want %q", item.ID, "1")
	}
	if item.Agent != "test-agent" {
		t.Errorf("Agent = %q, want %q", item.Agent, "test-agent")
	}
	if item.Desc != "test task" {
		t.Errorf("Desc = %q, want %q", item.Desc, "test task")
	}
	if item.Status != TaskPending {
		t.Errorf("Status = %q, want %q", item.Status, TaskPending)
	}
	if item.Detail != "test detail" {
		t.Errorf("Detail = %q, want %q", item.Detail, "test detail")
	}
}
