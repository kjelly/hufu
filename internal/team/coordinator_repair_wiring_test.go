package team

import (
	"context"
	"testing"

	"charm.land/fantasy"
)

// repairWiringCaptureAgent captures the fantasy.AgentStreamCall it receives
// so a test can assert what the coordinator actually wires onto it.
type repairWiringCaptureAgent struct {
	captured *fantasy.AgentStreamCall
}

func (a *repairWiringCaptureAgent) Generate(ctx context.Context, call fantasy.AgentCall) (*fantasy.AgentResult, error) {
	return a.Stream(ctx, fantasy.AgentStreamCall{Prompt: call.Prompt, Messages: call.Messages})
}

func (a *repairWiringCaptureAgent) Stream(ctx context.Context, call fantasy.AgentStreamCall) (*fantasy.AgentResult, error) {
	a.captured = &call
	return &fantasy.AgentResult{
		Response: fantasy.Response{Content: fantasy.ResponseContent{
			fantasy.TextContent{Text: "done"},
		}},
	}, nil
}

// TestCoordinatorWiresRepairToolCallOntoStreamingCall guards against a
// regression that would compile cleanly and look correct while silently not
// firing: fantasy 0.41's streaming loop reads AgentStreamCall.RepairToolCall
// directly, not the agent-level default set via fantasy.WithRepairToolCall
// in agent.CreateAgent's NewAgent options. If a future edit to
// coordinator_task_run.go drops the field from the streamCall literal, tool
// calls corrupted by a streaming provider concatenating two parallel tool
// calls' JSON deltas would go unrepaired again with no compiler or test
// signal — this test is that signal.
func TestCoordinatorWiresRepairToolCallOntoStreamingCall(t *testing.T) {
	worker := &repairWiringCaptureAgent{}
	c, _ := newWP08TestCoordinator(t, worker, 0)
	items := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "repair wiring task"}})
	todoID := items[0].ID

	if _, err := c.executeTask(context.Background(), TaskDef{
		Agent: "worker", Goal: "repair wiring task",
	}, todoID); err != nil {
		t.Fatalf("executeTask failed: %v", err)
	}

	if worker.captured == nil {
		t.Fatal("worker.Stream was never called")
	}
	if worker.captured.RepairToolCall == nil {
		t.Fatal("AgentStreamCall.RepairToolCall is nil: the coordinator's streaming call must set this field directly, since fantasy's streaming loop never falls back to the agent-level WithRepairToolCall default")
	}

	// Prove it is actually a working repair function, not merely non-nil,
	// by feeding it the exact corruption shape this was written to recover.
	repaired, err := worker.captured.RepairToolCall(context.Background(), fantasy.ToolCallRepairOptions{
		OriginalToolCall: fantasy.ToolCallContent{
			ToolCallID: "call_1",
			ToolName:   "view",
			Input:      `{"file_path":"a.go"}{"pattern":"b","path":"c"}`,
		},
	})
	if err != nil {
		t.Fatalf("wired RepairToolCall returned an error on a recoverable concatenated payload: %v", err)
	}
	if repaired == nil || repaired.Input != `{"file_path":"a.go"}` {
		t.Fatalf("wired RepairToolCall did not recover the first concatenated JSON value: %+v", repaired)
	}
}
