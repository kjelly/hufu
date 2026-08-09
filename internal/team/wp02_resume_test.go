package team

import (
	"context"
	"testing"

	"github.com/kjelly/hufu/internal/agent"
)

// TestExecuteTask_ContractErrorMarksTodoTerminal verifies that when
// executeTask (the crash-resume path's direct entry point) detects a contract
// error, the resumed TODO is marked terminal (TaskError) rather than left
// pending and re-driven forever on the next crash-resume (reviewer P1).
//
// Refs: docs/hufu-generic-task-reliability-mechanisms.md §4.3, §5, WP-02
func TestExecuteTask_ContractErrorMarksTodoTerminal(t *testing.T) {
	workspace := t.TempDir()
	c := &Coordinator{
		session:     &TeamSession{Workspace: workspace, Config: agent.TeamConfig{Name: "resume", Reliability: agent.ReliabilityConfig{VerifierLintMode: agent.VerifierLintError, HardEnforcement: true}}},
		sessionData: NewSession(),
		taskTracker: NewTaskTracker(),
	}
	c.taskTracker.TodoList().onChange = func() { c.saveCheckpoint() }

	// Add a pending task with a non-asserting verifier (contract error).
	item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{
		Agent:     "worker",
		Desc:      "resume task",
		Kind:      TaskKindOutcome,
		Verify:    "test -f artifact || echo FAIL",
		Execution: ExecutionContract{Kind: ExecutionKindInline},
	}})[0]
	c.taskTracker.TodoList().UpdateStatus(item.ID, TaskPending, "")

	// Simulate the crash-resume path: executeTask directly (no ExecuteTasks preflight).
	task := taskDefFromTodoItem(item)
	_, err := c.executeTask(context.Background(), task, item.ID)
	if err == nil {
		t.Fatal("expected contract error from executeTask on resume")
	}

	// The resumed TODO must be marked terminal (TaskError), not left pending.
	updated := c.taskTracker.TodoList().Items()
	var resumed *TodoItem
	for _, it := range updated {
		if it.ID == item.ID {
			resumed = it
			break
		}
	}
	if resumed == nil {
		t.Fatal("resumed task not found in todo list")
	}
	if resumed.Status == TaskPending {
		t.Errorf("resumed task left TaskPending — will be re-driven forever; status=%q", resumed.Status)
	}
	if isInterruptedStatus(resumed.Status) {
		t.Errorf("resumed task still in interrupted status %q — will be re-driven", resumed.Status)
	}
}

// TestExecuteTask_WarnModeEmitsWarningOnResume verifies that the crash-resume
// path (executeTask called directly, bypassing ExecuteTasks preflight) still
// emits a contract_warning event for warn-mode findings (reviewer P2).
//
// Refs: docs/hufu-generic-task-reliability-mechanisms.md §4.3, WP-02
func TestExecuteTask_WarnModeEmitsWarningOnResume(t *testing.T) {
	var warnCount int
	workspace := t.TempDir()
	c := &Coordinator{
		session:     &TeamSession{Workspace: workspace, Config: agent.TeamConfig{Name: "resume", Reliability: agent.ReliabilityConfig{VerifierLintMode: agent.VerifierLintWarn, HardEnforcement: true}}},
		sessionData: NewSession(),
		taskTracker: NewTaskTracker(),
	}
	c.taskTracker.TodoList().onChange = func() { c.saveCheckpoint() }
	c.SetStatusReporter(func(e StatusEvent) {
		if e.Type == "contract_warning" {
			warnCount++
		}
	})

	// Add a pending task with a non-asserting verifier (warn-mode finding).
	item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{
		Agent:     "worker",
		Desc:      "resume task",
		Kind:      TaskKindOutcome,
		Verify:    "test -f artifact || echo FAIL",
		Execution: ExecutionContract{Kind: ExecutionKindInline},
	}})[0]
	c.taskTracker.TodoList().UpdateStatus(item.ID, TaskPending, "")

	// Simulate the crash-resume path: executeTask directly (no ExecuteTasks preflight).
	task := taskDefFromTodoItem(item)
	_, err := c.executeTask(context.Background(), task, item.ID)
	// In warn mode the contract check passes (warning, not error), but
	// executeTask fails later on agent resolution. The contract_warning event
	// must have been emitted before the agent resolution failure.
	_ = err

	if warnCount != 1 {
		t.Errorf("expected exactly 1 contract_warning event on resume in warn mode, got %d", warnCount)
	}
}

// TestResumeInterruptedTasks_ContractErrorNotRedriven is the end-to-end
// crash-resume test: a task that previously failed with a contract error is
// terminal (TaskError), so isInterruptedStatus returns false and
// ResumeInterruptedTasks does NOT select it for re-execution (reviewer P1).
//
// Refs: docs/hufu-generic-task-reliability-mechanisms.md §4.3, §5, WP-02
func TestResumeInterruptedTasks_ContractErrorNotRedriven(t *testing.T) {
	workspace := t.TempDir()
	c := &Coordinator{
		session:     &TeamSession{Workspace: workspace, Config: agent.TeamConfig{Name: "resume", Reliability: agent.ReliabilityConfig{VerifierLintMode: agent.VerifierLintError, HardEnforcement: true}}},
		sessionData: NewSession(),
		taskTracker: NewTaskTracker(),
	}
	c.taskTracker.TodoList().onChange = func() { c.saveCheckpoint() }

	// Add a pending task with a contract error.
	item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{
		Agent:     "worker",
		Desc:      "resume task",
		Kind:      TaskKindOutcome,
		Verify:    "test -f artifact || echo FAIL",
		Execution: ExecutionContract{Kind: ExecutionKindInline},
	}})[0]
	c.taskTracker.TodoList().UpdateStatus(item.ID, TaskPending, "")

	// Drive it via executeTask (simulating the first resume attempt).
	task := taskDefFromTodoItem(item)
	_, _ = c.executeTask(context.Background(), task, item.ID)

	// Now the task should be terminal. Verify isInterruptedStatus does not
	// re-select it (the predicate that ResumeInterruptedTasks uses).
	updated := c.taskTracker.TodoList().Items()
	var resumed *TodoItem
	for _, it := range updated {
		if it.ID == item.ID {
			resumed = it
			break
		}
	}
	if resumed == nil {
		t.Fatal("task not found")
	}
	if isInterruptedStatus(resumed.Status) {
		t.Errorf("task status %q is still interrupted — ResumeInterruptedTasks would re-drive a contract-failed task", resumed.Status)
	}
}
