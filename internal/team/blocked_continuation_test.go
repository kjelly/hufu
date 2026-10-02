package team

import (
	"context"
	"testing"

	"charm.land/fantasy"

	"github.com/kjelly/hufu/internal/tools"
)

// recordedBlockingWorker reports every task blocked through the real
// submit_result tool. Its transcript lists the given tool calls, or nothing
// at all when recordSteps is false.
type recordedBlockingWorker struct {
	c           *Coordinator
	calls       []fantasy.ToolCallContent
	recordSteps bool
	refused     []string // call IDs a gate refused before they ran
	runs        int
}

func (w *recordedBlockingWorker) Generate(ctx context.Context, _ fantasy.AgentCall) (*fantasy.AgentResult, error) {
	return w.run(ctx)
}

func (w *recordedBlockingWorker) Stream(ctx context.Context, _ fantasy.AgentStreamCall) (*fantasy.AgentResult, error) {
	return w.run(ctx)
}

func (w *recordedBlockingWorker) run(ctx context.Context) (*fantasy.AgentResult, error) {
	w.runs++
	todoID, _ := ctx.Value(todoIDKey{}).(string)
	if item := w.c.todoItemByID(todoID); item != nil && item.Goal != "blocked step" {
		input := `{"status":"success","summary":"done"}`
		if _, err := (&submitResultTool{coordinator: w.c, todoID: todoID}).Run(ctx, fantasy.ToolCall{Name: submitResultToolName, Input: input}); err != nil {
			return nil, err
		}
		return &fantasy.AgentResult{Response: fantasy.Response{Content: fantasy.ResponseContent{fantasy.TextContent{Text: "done"}}}}, nil
	}
	for _, id := range w.refused {
		tools.ReportToolExecutionDisposition(ctx, tools.ToolExecutionDisposition{Kind: "guard_denied", ToolCallID: id, Executed: false})
	}
	submit := `{"status":"blocked","summary":"BLOCKED: step-risk=system-change; needs explicit user confirmation"}`
	if _, err := (&submitResultTool{coordinator: w.c, todoID: todoID}).Run(ctx, fantasy.ToolCall{Name: submitResultToolName, Input: submit}); err != nil {
		return nil, err
	}
	result := &fantasy.AgentResult{Response: fantasy.Response{Content: fantasy.ResponseContent{fantasy.TextContent{Text: "blocked"}}}}
	if w.recordSteps {
		var content fantasy.ResponseContent
		for _, call := range w.calls {
			content = append(content, call)
		}
		content = append(content, fantasy.ToolCallContent{ToolCallID: "submit", ToolName: submitResultToolName, Input: submit})
		result.Steps = []fantasy.StepResult{{Response: fantasy.Response{Content: content}}}
	}
	return result, nil
}

// TestWorkerReportedBlockStopsTheRunOnlyAfterAChange checks that a worker
// that reported its task blocked is never replayed, whatever retry budget is
// left, and that the run stops delegating only when the task may have changed
// state. The blocked step itself can never be dispatched again.
func TestWorkerReportedBlockStopsTheRunOnlyAfterAChange(t *testing.T) {
	riskCheck := fantasy.ToolCallContent{ToolCallID: "risk", ToolName: decisionPrimitiveToolName, Input: `{"name":"step-risk","context":{"step":"install"}}`}
	tests := []struct {
		name        string
		retries     int
		calls       []fantasy.ToolCallContent
		recordSteps bool
		refused     []string
		wantWrapUp  bool
	}{
		{name: "read-only attempt", retries: 1, calls: []fantasy.ToolCallContent{riskCheck, {ToolCallID: "view", ToolName: "view", Input: `{"file_path":"go.mod"}`}}, recordSteps: true},
		{name: "no retry budget", retries: 0, calls: []fantasy.ToolCallContent{riskCheck}, recordSteps: true},
		{name: "after a write", retries: 1, calls: []fantasy.ToolCallContent{riskCheck, {ToolCallID: "touch", ToolName: "bash", Input: `{"command":"touch out.txt"}`}}, recordSteps: true, wantWrapUp: true},
		{name: "write refused before it ran", retries: 1, calls: []fantasy.ToolCallContent{riskCheck, {ToolCallID: "rm", ToolName: "bash", Input: `{"command":"rm -rf /var/tmp/old"}`}}, recordSteps: true, refused: []string{"rm"}},
		{name: "nothing recorded", retries: 1, wantWrapUp: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := newResourceOrderCoordinator(t, 2, test.retries)
			worker := &recordedBlockingWorker{c: c, calls: test.calls, recordSteps: test.recordSteps, refused: test.refused}
			c.workerAgentOverride = worker
			_, _ = c.ExecuteTasks(context.Background(), []TaskDef{{Agent: "worker", Goal: "blocked step", SideEffect: SideEffectNone}})
			items := c.taskTracker.TodoList().Items()
			if len(items) != 1 || items[0].Status != TaskBlocked || worker.runs != 1 {
				t.Fatalf("task = %s after %d worker runs, want blocked after exactly 1", items[0].Status, worker.runs)
			}
			if c.IsWrapUp() != test.wantWrapUp {
				t.Fatalf("wrap-up = %v, want %v", c.IsWrapUp(), test.wantWrapUp)
			}
			if test.wantWrapUp {
				return
			}
			// The same step is not dispatched again; other work still runs.
			_, _ = c.ExecuteTasks(context.Background(), []TaskDef{
				{Agent: "worker", Goal: "blocked step", SideEffect: SideEffectNone},
				{Agent: "worker", Goal: "independent step", SideEffect: SideEffectNone},
			})
			items = c.taskTracker.TodoList().Items()
			if worker.runs != 2 || items[0].Status != TaskBlocked || items[len(items)-1].Status != TaskDone || items[len(items)-1].Goal != "independent step" {
				t.Fatalf("after redispatch: %d worker runs, tasks %+v; want only the independent step to run", worker.runs, todoStatuses(items))
			}
		})
	}
}

func todoStatuses(items []*TodoItem) []string {
	out := make([]string, 0, len(items))
	for _, item := range items {
		out = append(out, item.ID+":"+item.Goal+":"+string(item.Status))
	}
	return out
}

// TestBlockedStepDuplicateLastsForTheInvocation checks that a step its worker
// reported blocked cannot be dispatched again in the same invocation, while a
// later invocation, which carries the user's answer, may dispatch it.
func TestBlockedStepDuplicateLastsForTheInvocation(t *testing.T) {
	c := newResourceOrderCoordinator(t, 2, 1)
	readOnly := []fantasy.ToolCallContent{{ToolCallID: "risk", ToolName: decisionPrimitiveToolName, Input: `{"name":"step-risk","context":{"step":"install"}}`}}
	c.workerAgentOverride = &recordedBlockingWorker{c: c, calls: readOnly, recordSteps: true}
	_, _ = c.ExecuteTasks(context.Background(), []TaskDef{{Agent: "worker", Goal: "blocked step", SideEffect: SideEffectNone}})
	blocked := c.taskTracker.TodoList().Items()[0]
	if blocked.Status != TaskBlocked {
		t.Fatalf("task status = %s, want blocked", blocked.Status)
	}
	if match := c.findExistingTodoDuplicate(context.Background(), "worker", blocked.Desc, nil, "", ""); match == nil || match.Item.ID != blocked.ID {
		t.Fatalf("duplicate in the same invocation = %+v, want task %s", match, blocked.ID)
	}
	c.resetRoundState()
	if match := c.findExistingTodoDuplicate(context.Background(), "worker", blocked.Desc, nil, "", ""); match != nil {
		t.Fatalf("duplicate after a new invocation = %+v, want none", match)
	}
}
