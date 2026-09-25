package team

import (
	"context"
	"testing"
)

// admitInterruptedCatalogTask compiles and admits one catalog task, leaves it
// at status, and rebuilds the Todo list from the durable events as a resumed
// process would see it.
func admitInterruptedCatalogTask(t *testing.T, c *Coordinator, id string, status TaskStatus) *CatalogActionBinding {
	t.Helper()
	if err := c.AdmitExecutionPolicy(); err != nil {
		t.Fatal(err)
	}
	entry, _ := c.session.ActionCatalog.Lookup(id)
	arguments := `{"service":"api"}`
	c.actionProposals.mu.Lock()
	c.actionProposals.addLocked(TeamActionProposal{
		TeamActionProposedPayload: TeamActionProposedPayload{ProposalID: "tap_resume", ActionID: entry.ID, EntryHash: entry.Hash, ArgumentsHash: runInputHash([]byte(arguments)), Assessment: "recommended"},
		IdempotencyKey:            "resume-key",
	})
	c.actionProposals.mu.Unlock()
	compiled, err := c.compileCatalogActionTasks([]TaskDef{catalogRequest("runtime-engineer", id, arguments)})
	if err != nil {
		t.Fatal(err)
	}
	task := compiled[0]
	task.CatalogAction.InvocationID = "tai_resume000000000000000000000000"
	_, item := createAdmittedTestTask(t, c, task)
	if status == TaskInProgress {
		if err := c.commitTaskTransitionFromCurrent(context.Background(), item.ID, TaskInProgress, "executing structured action", "", attemptStartMetadata(1)); err != nil {
			t.Fatal(err)
		}
	}
	c.taskTracker.TodoList().Restore(ReduceToTodoList(mustReadEvents(t, c.eventStore)))
	restored := todoItemByID(c.taskTracker.TodoList().Items(), item.ID)
	if restored == nil || restored.Status != status || restored.CatalogAction == nil || restored.CatalogAction.InvocationID != task.CatalogAction.InvocationID {
		t.Fatalf("restored catalog todo = %#v", restored)
	}
	return restored.CatalogAction
}

func TestResumeInterruptedCatalogActions(t *testing.T) {
	tests := []struct {
		name       string
		action     string
		status     TaskStatus
		wantRuns   int
		wantStatus TaskStatus
	}{
		{name: "pending retry reruns the same invocation", action: "collect-debug-bundle", status: TaskPending, wantRuns: 1, wantStatus: TaskDone},
		{name: "started retry reruns the same invocation", action: "collect-debug-bundle", status: TaskInProgress, wantRuns: 1, wantStatus: TaskDone},
		{name: "pending manual blocks for a human", action: "restart-service", status: TaskPending, wantStatus: TaskBlocked},
		{name: "started manual blocks for a human", action: "restart-service", status: TaskInProgress, wantStatus: TaskBlocked},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, provider := dispatchTestCoordinator(t, "", nil)
			binding := admitInterruptedCatalogTask(t, c, tt.action, tt.status)
			if _, err := c.ResumeInterruptedTasks(context.Background()); err != nil && tt.wantRuns > 0 {
				t.Fatalf("ResumeInterruptedTasks: %v", err)
			}
			if provider.executed != tt.wantRuns {
				t.Fatalf("provider executions = %d, want %d", provider.executed, tt.wantRuns)
			}
			if tt.wantRuns > 0 && provider.env.CatalogInvocationID != binding.InvocationID {
				t.Fatalf("resumed invocation ID = %q, want %q", provider.env.CatalogInvocationID, binding.InvocationID)
			}
			var item *TodoItem
			for _, candidate := range c.taskTracker.TodoList().Items() {
				if candidate.CatalogAction != nil {
					item = candidate
				}
			}
			if item == nil || item.Status != tt.wantStatus {
				t.Fatalf("resumed catalog todo = %#v, want status %s", item, tt.wantStatus)
			}
		})
	}
}
