package team

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"charm.land/fantasy"
)

func TestReconcileTaskErrorsNameTheWrongField(t *testing.T) {
	const finishHint = "call finish with acknowledge_failed_tasks:true"
	tests := []struct {
		name       string
		taskID     string // "" means the blocked task
		resolvedBy string // "" with resolver true means the pending task
		resolver   bool
		want       []string
		wantNot    []string
	}{
		{
			name:       "resolved_by is a label rather than a task",
			resolvedBy: "user_decision_skip",
			want:       []string{`resolved_by "user_decision_skip" is not a task in the todo list`, finishHint},
		},
		{
			name: "resolved_by is empty",
			want: []string{`resolution status "superseded" requires resolved_by`, finishHint},
		},
		{
			name:       "task_id is not a listed ID",
			taskID:     "task-5",
			resolvedBy: "user_decision_skip",
			want:       []string{`task_id "task-5" is not in the todo list`, "Failed or blocked tasks: 1."},
			wantNot:    []string{finishHint},
		},
		{
			name:     "resolver exists but is not done",
			resolver: true,
			want:     []string{"must be done"},
			wantNot:  []string{finishHint, "Failed or blocked tasks"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newBudgetCoordinator(t)
			c.taskTracker.TodoList().AddBatch([]TodoSpec{
				{Agent: "executor", Desc: "blocked step"},
				{Agent: "executor", Desc: "pending step"},
			})
			items := c.taskTracker.TodoList().Items()
			c.taskTracker.TodoList().UpdateStatusAndOutput(items[0].ID, TaskBlocked, "step-risk=system-change", "")
			taskID := tt.taskID
			if taskID == "" {
				taskID = items[0].ID
			}
			resolvedBy := tt.resolvedBy
			if tt.resolver {
				resolvedBy = items[1].ID
			}
			input, err := json.Marshal(map[string]string{
				"task_id": taskID, "status": "superseded", "resolved_by": resolvedBy, "reason": "user chose skip",
			})
			if err != nil {
				t.Fatal(err)
			}
			resp, err := (&reconcileTaskTool{coordinator: c}).Run(context.Background(), fantasy.ToolCall{Input: string(input)})
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if !resp.IsError {
				t.Fatalf("reconcile_task succeeded: %s", resp.Content)
			}
			for _, w := range tt.want {
				if !strings.Contains(resp.Content, w) {
					t.Errorf("response missing %q:\n%s", w, resp.Content)
				}
			}
			for _, w := range tt.wantNot {
				if strings.Contains(resp.Content, w) {
					t.Errorf("response unexpectedly contains %q:\n%s", w, resp.Content)
				}
			}
			if got := failedTodoItems(c.taskTracker.TodoList().Items()); len(got) != 1 {
				t.Errorf("unresolved failed tasks = %d, want 1", len(got))
			}
		})
	}
}
