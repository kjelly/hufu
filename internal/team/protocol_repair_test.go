package team

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/anomalyco/hufu/internal/agent"
)

type mockWorkerTextAgent struct {
	text string
}

func (m *mockWorkerTextAgent) Generate(ctx context.Context, call fantasy.AgentCall) (*fantasy.AgentResult, error) {
	return &fantasy.AgentResult{
		Response: fantasy.Response{
			Content: fantasy.ResponseContent{
				fantasy.TextContent{Text: m.text},
			},
		},
	}, nil
}

func (m *mockWorkerTextAgent) Stream(ctx context.Context, call fantasy.AgentStreamCall) (*fantasy.AgentResult, error) {
	return &fantasy.AgentResult{
		Response: fantasy.Response{
			Content: fantasy.ResponseContent{
				fantasy.TextContent{Text: m.text},
			},
		},
	}, nil
}

type mockRepairAgent struct {
	onSubmit func()
}

type replayableWorkerAgent struct {
	calls    *int
	onSecond func()
}

func (m *replayableWorkerAgent) response() *fantasy.AgentResult {
	(*m.calls)++
	text := "attempt-one-original"
	if *m.calls == 2 {
		text = "attempt-two-original"
		if m.onSecond != nil {
			m.onSecond()
		}
	}
	return &fantasy.AgentResult{Response: fantasy.Response{Content: fantasy.ResponseContent{
		fantasy.TextContent{Text: text},
	}}}
}

func (m *replayableWorkerAgent) Generate(context.Context, fantasy.AgentCall) (*fantasy.AgentResult, error) {
	return m.response(), nil
}

func (m *replayableWorkerAgent) Stream(context.Context, fantasy.AgentStreamCall) (*fantasy.AgentResult, error) {
	return m.response(), nil
}

func (m *mockRepairAgent) Generate(ctx context.Context, call fantasy.AgentCall) (*fantasy.AgentResult, error) {
	if m.onSubmit != nil {
		m.onSubmit()
	}
	return &fantasy.AgentResult{
		Response: fantasy.Response{
			Content: fantasy.ResponseContent{
				fantasy.TextContent{Text: "Repaired typed result submitted."},
			},
		},
	}, nil
}

func (m *mockRepairAgent) Stream(ctx context.Context, call fantasy.AgentStreamCall) (*fantasy.AgentResult, error) {
	if m.onSubmit != nil {
		m.onSubmit()
	}
	return &fantasy.AgentResult{
		Response: fantasy.Response{
			Content: fantasy.ResponseContent{
				fantasy.TextContent{Text: "Repaired typed result submitted."},
			},
		},
	}, nil
}

func TestProtocolRepair_SuccessAndReceipt(t *testing.T) {
	workspace := t.TempDir()
	t.Cleanup(func() { time.Sleep(100 * time.Millisecond) })

	c := &Coordinator{
		session: &TeamSession{
			Workspace: workspace,
			Config:    agent.TeamConfig{Name: "protocol-repair", Timeout: 30, MaxRetries: 3},
			Agents: map[string]*agent.AgentDef{
				"worker": {Name: "worker", Role: "worker", Generation: agent.GenerationParams{Model: "test"}},
			},
		},
		sessionTime:     time.Now(),
		taskTracker:     NewTaskTracker(),
		reportStatus:    func(StatusEvent) {},
		taskResultCache: make(map[string][]cachedTaskEntry),
		executionRunID:  "run-protocol-101",
	}

	items := c.taskTracker.TodoList().AddBatch([]TodoSpec{{
		Agent: "worker",
		Desc:  "data processing task",
	}})
	todoID := items[0].ID

	// First agent run produces output but omits submit_result.
	c.workerAgentOverride = &mockWorkerTextAgent{text: "Processed input data successfully."}

	// We inject a mock repair agent that calls submit_result when invoked during repair.
	c.repairAgentOverride = &mockRepairAgent{
		onSubmit: func() {
			c.storeSubmittedTaskResult(todoID, &TaskResult{
				TaskID:   todoID,
				Agent:    "worker",
				Status:   "success",
				Summary:  "repaired structured result",
				Source:   "submitted",
				Evidence: nil,
			})
		},
	}

	out, err := c.executeTask(context.Background(), TaskDef{
		Agent: "worker",
		Goal:  "data processing task",
		Execution: ExecutionContract{
			RequiresResult: true,
		},
	}, todoID)

	if err != nil {
		t.Fatalf("expected protocol repair to succeed, got error: %v", err)
	}
	if out == "" {
		t.Error("expected non-empty task output")
	}
	if strings.Contains(out, "VERBATIM TRANSCRIPT CAPTURED") {
		t.Fatalf("summary-mode task must preserve worker output, got verbatim manifest: %q", out)
	}
	if !strings.Contains(out, "Processed input data successfully") {
		t.Fatalf("summary-mode task output lost worker response: %q", out)
	}

	item := c.taskTracker.TodoList().Items()[0]
	if item.Status != TaskDone {
		t.Fatalf("task status = %s, want done", item.Status)
	}

	// Verify ExecutionReceipt is preserved on TodoItem
	if item.ExecutionReceipt == nil {
		t.Fatal("expected ExecutionReceipt to be preserved on TodoItem")
	}
	if item.ExecutionReceipt.RunID != "run-protocol-101" {
		t.Errorf("Receipt RunID = %q, want run-protocol-101", item.ExecutionReceipt.RunID)
	}
	if item.ExecutionReceipt.TaskID != todoID {
		t.Errorf("Receipt TaskID = %q, want %s", item.ExecutionReceipt.TaskID, todoID)
	}
	if item.ExecutionReceipt.Attempt != 1 {
		t.Errorf("Receipt Attempt = %d, want 1", item.ExecutionReceipt.Attempt)
	}
	if item.ExecutionReceipt.ProducerID != "worker" {
		t.Errorf("Receipt ProducerID = %q, want worker", item.ExecutionReceipt.ProducerID)
	}
	if item.ExecutionReceipt.TranscriptRef == "" {
		t.Error("expected ExecutionReceipt.TranscriptRef to be populated in production execution path")
	}
	transcript, readErr := os.ReadFile(item.ExecutionReceipt.TranscriptRef)
	if readErr != nil {
		t.Fatalf("read original transcript %q: %v", item.ExecutionReceipt.TranscriptRef, readErr)
	}
	if !strings.Contains(string(transcript), `"event":"assistant_output"`) {
		t.Fatalf("original transcript does not contain worker output: %s", transcript)
	}
	if strings.Contains(string(transcript), "Repaired typed result submitted") {
		t.Fatal("repair output must not be appended to the original transcript")
	}
	if item.ExecutionReceipt.RepairProvenance == nil || !item.ExecutionReceipt.RepairProvenance.Success {
		t.Error("expected repair provenance to record success=true")
	}
	if len(item.ExecutionReceipts) != 1 {
		t.Fatalf("execution receipt history length = %d, want one receipt for attempt 1", len(item.ExecutionReceipts))
	}
}

func TestProtocolRepair_ReplayableRetryPreservesPerAttemptProvenance(t *testing.T) {
	workspace := t.TempDir()
	t.Cleanup(func() { time.Sleep(100 * time.Millisecond) })
	workerCalls := 0

	c := &Coordinator{
		session: &TeamSession{
			Workspace: workspace,
			Config:    agent.TeamConfig{Name: "protocol-retry", Timeout: 30, MaxRetries: 2},
			Agents: map[string]*agent.AgentDef{
				"worker": {Name: "worker", Role: "worker", MaxRetries: 2, Generation: agent.GenerationParams{Model: "test"}},
			},
		},
		sessionTime:     time.Now(),
		taskTracker:     NewTaskTracker(),
		reportStatus:    func(StatusEvent) {},
		taskResultCache: make(map[string][]cachedTaskEntry),
		executionRunID:  "run-protocol-retry",
	}
	item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "replayable protocol task"}})[0]

	c.workerAgentOverride = &replayableWorkerAgent{
		calls: &workerCalls,
		onSecond: func() {
			c.storeSubmittedTaskResult(item.ID, &TaskResult{
				TaskID: item.ID, Agent: "worker", Status: "success",
				Summary: "second attempt submitted result", Source: "submitted",
			})
		},
	}
	// Attempt 1 repair fails, so the replayable task must run the worker again.
	c.repairAgentOverride = &mockWorkerTextAgent{text: "repair-output-must-not-be-in-attempt-1"}

	if _, err := c.executeTask(context.Background(), TaskDef{
		Agent: "worker", Goal: "replayable protocol task",
		Execution: ExecutionContract{RequiresResult: true},
	}, item.ID); err != nil {
		t.Fatalf("expected replayable retry to succeed, got: %v", err)
	}
	if workerCalls != 2 {
		t.Fatalf("worker invocation count = %d, want 2", workerCalls)
	}

	got := c.taskTracker.TodoList().Items()[0]
	if got.Status != TaskDone {
		t.Fatalf("task status = %s, want done", got.Status)
	}
	if len(got.ExecutionReceipts) != 2 {
		t.Fatalf("receipt history length = %d, want 2", len(got.ExecutionReceipts))
	}
	first, second := got.ExecutionReceipts[0], got.ExecutionReceipts[1]
	if first.Attempt != 1 || second.Attempt != 2 || first.TranscriptRef == second.TranscriptRef {
		t.Fatalf("expected distinct attempt receipts, got first=%#v second=%#v", first, second)
	}
	if first.RepairProvenance == nil || first.RepairProvenance.Success {
		t.Fatalf("attempt 1 should retain failed repair provenance: %#v", first.RepairProvenance)
	}
	if second.RepairProvenance != nil {
		t.Fatalf("attempt 2 should not have repair provenance: %#v", second.RepairProvenance)
	}
	firstData, err := os.ReadFile(first.TranscriptRef)
	if err != nil {
		t.Fatalf("read attempt 1 transcript: %v", err)
	}
	secondData, err := os.ReadFile(second.TranscriptRef)
	if err != nil {
		t.Fatalf("read attempt 2 transcript: %v", err)
	}
	if !strings.Contains(string(firstData), "attempt-one-original") || strings.Contains(string(firstData), "attempt-two-original") || strings.Contains(string(firstData), "repair-output-must-not") {
		t.Fatalf("attempt 1 transcript was not immutable/original: %s", firstData)
	}
	if !strings.Contains(string(secondData), "attempt-two-original") {
		t.Fatalf("attempt 2 transcript missing second worker execution: %s", secondData)
	}

	es, err := NewEventStore(workspace, "run-protocol-retry", "protocol-retry")
	if err != nil {
		t.Fatalf("create event store: %v", err)
	}
	payload, err := json.Marshal(map[string]interface{}{
		"id":                 got.ID,
		"desc":               got.Desc,
		"status":             string(got.Status),
		"agent":              got.Agent,
		"execution_receipts": got.ExecutionReceipts,
	})
	if err != nil {
		_ = es.Close()
		t.Fatalf("marshal receipt event: %v", err)
	}
	if err := es.Append(RunEvent{Type: "task_completed", TaskID: got.ID, Payload: payload}); err != nil {
		_ = es.Close()
		t.Fatalf("append receipt event: %v", err)
	}
	events, err := es.ReadEvents()
	_ = es.Close()
	if err != nil {
		t.Fatalf("read event store: %v", err)
	}
	reduced := ReduceToTodoList(events)
	if len(reduced) != 1 || len(reduced[0].ExecutionReceipts) != 2 {
		t.Fatalf("event-store reduction lost receipt history: %#v", reduced)
	}
}

func TestProtocolRepair_NonReplayableTaskBlocksOnRepairFailure(t *testing.T) {
	workspace := t.TempDir()
	t.Cleanup(func() { time.Sleep(100 * time.Millisecond) })
	workerCalls := 0

	c := &Coordinator{
		session: &TeamSession{
			Workspace: workspace,
			Config:    agent.TeamConfig{Name: "protocol-block", Timeout: 30, MaxRetries: 3},
			Agents: map[string]*agent.AgentDef{
				"worker": {Name: "worker", Role: "worker", Generation: agent.GenerationParams{Model: "test"}},
			},
		},
		sessionTime:     time.Now(),
		taskTracker:     NewTaskTracker(),
		reportStatus:    func(StatusEvent) {},
		taskResultCache: make(map[string][]cachedTaskEntry),
		executionRunID:  "run-protocol-102",
	}

	items := c.taskTracker.TodoList().AddBatch([]TodoSpec{{
		Agent:      "worker",
		Desc:       "mutate external system",
		SideEffect: SideEffectExternalWrite,
	}})
	todoID := items[0].ID

	// Counting agent that omits submit_result in both initial run and repair step
	c.workerAgentOverride = &countingEmptyAgent{calls: &workerCalls}

	_, err := c.executeTask(context.Background(), TaskDef{
		Agent:      "worker",
		Goal:       "mutate external system",
		SideEffect: SideEffectExternalWrite,
		Execution: ExecutionContract{
			RequiresResult: true,
		},
	}, todoID)

	if err == nil {
		t.Fatal("expected protocol failure when repair fails")
	}

	// Worker tools should only be called ONCE (no retries for non-replayable protocol failure)
	if workerCalls != 1 {
		t.Fatalf("worker invocation count = %d, want exactly 1 (worker tools run once, no replay)", workerCalls)
	}

	item := c.taskTracker.TodoList().Items()[0]
	if item.Status != TaskBlocked {
		t.Fatalf("task status = %s, want blocked", item.Status)
	}
	if !strings.Contains(item.Detail, "protocol") {
		t.Fatalf("blocked detail should contain protocol failure explanation, got: %q", item.Detail)
	}
	if item.ExecutionReceipt == nil {
		t.Error("ExecutionReceipt should be preserved even when task blocks")
	}
	if item.ExecutionReceipt != nil {
		transcript, readErr := os.ReadFile(item.ExecutionReceipt.TranscriptRef)
		if readErr != nil {
			t.Fatalf("read failed-task original transcript %q: %v", item.ExecutionReceipt.TranscriptRef, readErr)
		}
		if !strings.Contains(string(transcript), "Processed") && !strings.Contains(string(transcript), "assistant_output") {
			t.Fatalf("failed-task transcript does not preserve original execution: %s", transcript)
		}
		if strings.Contains(string(transcript), "Repaired typed result submitted") {
			t.Fatal("repair output must not be appended to failed-task original transcript")
		}
	}
}

func TestProtocolRepair_FreeTextOutputCannotBypassRepairFailure(t *testing.T) {
	// Finding 1 test: Non-empty worker output + repair fails + successful verifier ("true") MUST NOT turn task to done.
	workspace := t.TempDir()
	t.Cleanup(func() { time.Sleep(100 * time.Millisecond) })

	c := &Coordinator{
		session: &TeamSession{
			Workspace: workspace,
			Config:    agent.TeamConfig{Name: "protocol-bypass", Timeout: 30, MaxRetries: 1},
			Agents: map[string]*agent.AgentDef{
				"worker": {Name: "worker", Role: "worker", Generation: agent.GenerationParams{Model: "test"}},
			},
		},
		sessionTime:     time.Now(),
		taskTracker:     NewTaskTracker(),
		reportStatus:    func(StatusEvent) {},
		taskResultCache: make(map[string][]cachedTaskEntry),
		executionRunID:  "run-protocol-bypass",
	}

	items := c.taskTracker.TodoList().AddBatch([]TodoSpec{{
		Agent:      "worker",
		Desc:       "critical task requiring result",
		SideEffect: SideEffectExternalWrite,
	}})
	todoID := items[0].ID

	// Worker returns non-empty output text but omits submit_result
	c.workerAgentOverride = &mockWorkerTextAgent{text: "completed work text without submit_result"}
	// Repair agent runs but fails to call submit_result
	c.repairAgentOverride = &mockWorkerTextAgent{text: "repair failed to submit"}

	_, err := c.executeTask(context.Background(), TaskDef{
		Agent:      "worker",
		Goal:       "critical task requiring result",
		Verify:     "true",
		SideEffect: SideEffectExternalWrite,
		Execution: ExecutionContract{
			RequiresResult: true,
		},
	}, todoID)

	if err == nil {
		t.Fatal("expected execution error when protocol repair fails to produce submit_result")
	}

	item := c.taskTracker.TodoList().Items()[0]
	if item.Status == TaskDone {
		t.Fatal("task MUST NOT be marked done when protocol repair fails, even if free-text output exists and verifier passes")
	}
	if item.Status != TaskBlocked {
		t.Fatalf("task status = %s, want blocked", item.Status)
	}
}

func TestProtocolRepair_AllowsReplayFalseBlocksWorkerReplay(t *testing.T) {
	// Finding 2 test: SideEffectNone + explicit AllowsReplay = false MUST block retries and run worker exactly once.
	workspace := t.TempDir()
	t.Cleanup(func() { time.Sleep(100 * time.Millisecond) })
	workerCalls := 0

	c := &Coordinator{
		session: &TeamSession{
			Workspace: workspace,
			Config:    agent.TeamConfig{Name: "replay-false", Timeout: 30, MaxRetries: 3},
			Agents: map[string]*agent.AgentDef{
				"worker": {Name: "worker", Role: "worker", Generation: agent.GenerationParams{Model: "test"}},
			},
		},
		sessionTime:     time.Now(),
		taskTracker:     NewTaskTracker(),
		reportStatus:    func(StatusEvent) {},
		taskResultCache: make(map[string][]cachedTaskEntry),
		executionRunID:  "run-replay-false",
	}

	items := c.taskTracker.TodoList().AddBatch([]TodoSpec{{
		Agent:      "worker",
		Desc:       "inline non-replayable task",
		SideEffect: SideEffectNone,
	}})
	todoID := items[0].ID

	c.workerAgentOverride = &countingEmptyAgent{calls: &workerCalls}
	noReplay := false

	_, err := c.executeTask(context.Background(), TaskDef{
		Agent:      "worker",
		Goal:       "inline non-replayable task",
		SideEffect: SideEffectNone,
		Execution: ExecutionContract{
			RequiresResult: true,
			AllowsReplay:   &noReplay,
		},
	}, todoID)

	if err == nil {
		t.Fatal("expected protocol failure when repair fails")
	}

	if workerCalls != 1 {
		t.Fatalf("worker invocation count = %d, want exactly 1 (AllowsReplay=false prohibits re-running worker)", workerCalls)
	}

	item := c.taskTracker.TodoList().Items()[0]
	if item.Status != TaskBlocked {
		t.Fatalf("task status = %s, want blocked", item.Status)
	}
}

func TestProtocolRepair_RecoveryPolicyBlocksWorkerReplay(t *testing.T) {
	workspace := t.TempDir()
	t.Cleanup(func() { time.Sleep(100 * time.Millisecond) })
	workerCalls := 0

	c := &Coordinator{
		session: &TeamSession{
			Workspace: workspace,
			Config:    agent.TeamConfig{Name: "recovery-policy", Timeout: 30, MaxRetries: 3},
			Agents: map[string]*agent.AgentDef{
				"worker": {Name: "worker", Role: "worker", Generation: agent.GenerationParams{Model: "test"}},
			},
		},
		sessionTime:     time.Now(),
		taskTracker:     NewTaskTracker(),
		reportStatus:    func(StatusEvent) {},
		taskResultCache: make(map[string][]cachedTaskEntry),
		executionRunID:  "run-protocol-policy",
	}
	item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "reconcile before retry"}})[0]
	c.workerAgentOverride = &countingEmptyAgent{calls: &workerCalls}

	_, err := c.executeTask(context.Background(), TaskDef{
		Agent:      "worker",
		Goal:       "reconcile before retry",
		Recovery:   RecoveryReconcile,
		SideEffect: SideEffectNone,
		Execution:  ExecutionContract{RequiresResult: true},
	}, item.ID)
	if err == nil {
		t.Fatal("expected protocol failure when recovery policy disallows replay")
	}
	if workerCalls != 1 {
		t.Fatalf("worker invocation count = %d, want exactly 1", workerCalls)
	}
	if got := c.taskTracker.TodoList().Items()[0].Status; got != TaskBlocked {
		t.Fatalf("task status = %s, want blocked", got)
	}
}

func TestExecutionReceipt_ProvenanceAndEventStoreReplay(t *testing.T) {
	// Finding 3 test: Multi-attempt receipts and repair provenance survive event store reduction.
	receipt1 := ExecutionReceipt{
		RunID:         "run-1",
		TaskID:        "todo-1",
		Attempt:       1,
		StartedAt:     time.Now().Add(-10 * time.Second),
		FinishedAt:    time.Now().Add(-5 * time.Second),
		ProducerID:    "worker",
		TranscriptRef: "task-log-1",
		RepairProvenance: &RepairProvenance{
			Attempted: true,
			Success:   false,
			Error:     "missing submit_result",
		},
	}
	receipt2 := ExecutionReceipt{
		RunID:         "run-1",
		TaskID:        "todo-1",
		Attempt:       2,
		StartedAt:     time.Now().Add(-4 * time.Second),
		FinishedAt:    time.Now(),
		ProducerID:    "worker",
		TranscriptRef: "task-log-2",
		RepairProvenance: &RepairProvenance{
			Attempted: true,
			Success:   true,
			SubmittedResult: &TaskResult{
				TaskID:  "todo-1",
				Agent:   "worker",
				Status:  "success",
				Summary: "repaired typed result",
				Source:  "submitted",
			},
		},
	}

	payloadData, _ := json.Marshal(map[string]interface{}{
		"id":                 "todo-1",
		"desc":               "test receipt provenance",
		"status":             "done",
		"agent":              "worker",
		"execution_receipt":  receipt2,
		"execution_receipts": []ExecutionReceipt{receipt1, receipt2},
	})

	events := []RunEvent{
		{Type: "task_created", TaskID: "todo-1", Payload: payloadData},
		{Type: "task_completed", TaskID: "todo-1", Payload: payloadData},
	}

	reduced := ReduceToTodoList(events)
	if len(reduced) != 1 {
		t.Fatalf("reduced items count = %d, want 1", len(reduced))
	}
	item := reduced[0]
	if len(item.ExecutionReceipts) != 2 {
		t.Fatalf("reduced ExecutionReceipts length = %d, want 2", len(item.ExecutionReceipts))
	}
	if item.ExecutionReceipts[0].RepairProvenance == nil || item.ExecutionReceipts[0].RepairProvenance.Success {
		t.Errorf("attempt 1 repair provenance should show success=false")
	}
	if item.ExecutionReceipts[1].RepairProvenance == nil || !item.ExecutionReceipts[1].RepairProvenance.Success {
		t.Errorf("attempt 2 repair provenance should show success=true")
	}
}

func TestProtocolIncomplete_ConvergenceAndFinalization(t *testing.T) {
	// Finding 4 test: TaskProtocolIncomplete convergence on unexpected termination and interrupted status check.
	c := &Coordinator{
		taskTracker:  NewTaskTracker(),
		reportStatus: func(StatusEvent) {},
	}
	c.taskTracker.TodoList().AddBatch([]TodoSpec{{
		Agent: "worker",
		Desc:  "incomplete protocol task",
	}})
	items := c.taskTracker.TodoList().Items()
	_ = c.taskTracker.TodoList().TryUpdateStatusAndOutput(items[0].ID, TaskProtocolIncomplete, "waiting repair", "")

	if !isInterruptedStatus(TaskProtocolIncomplete) {
		t.Error("isInterruptedStatus(TaskProtocolIncomplete) should return true")
	}

	c.finalizeRemainingTasks()

	itemsAfter := c.taskTracker.TodoList().Items()
	if itemsAfter[0].Status != TaskError {
		t.Fatalf("finalizeRemainingTasks should transition TaskProtocolIncomplete to TaskError, got: %s", itemsAfter[0].Status)
	}
}
