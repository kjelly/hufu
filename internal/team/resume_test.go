package team

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"charm.land/fantasy"
	"github.com/kjelly/hufu/internal/agent"
)

func TestIsInterruptedStatus(t *testing.T) {
	interrupted := []TaskStatus{TaskInProgress, TaskVerifying, TaskPaused, TaskPlanned, TaskPending, TaskProtocolIncomplete}
	for _, s := range interrupted {
		if !isInterruptedStatus(s) {
			t.Errorf("%q should be treated as interrupted", s)
		}
	}
	terminal := []TaskStatus{TaskDone, TaskSkipped, TaskError, TaskBlocked}
	for _, s := range terminal {
		if isInterruptedStatus(s) {
			t.Errorf("%q should NOT be treated as interrupted", s)
		}
	}
}

// TestResumeInterruptedTasks_ProtocolCheckpointUsesResultOnlyRepair proves
// the crash-resume boundary: once protocol_incomplete and worker output are
// checkpointed, resume invokes only the submit_result repair agent. The
// original worker override must not be called and the checkpoint must finish
// with the repaired typed result.
func TestResumeInterruptedTasks_ProtocolCheckpointUsesResultOnlyRepair(t *testing.T) {
	workspace := t.TempDir()
	config := agent.TeamConfig{Name: "protocol-resume", Timeout: 30, MaxRetries: 1}
	agents := map[string]*agent.AgentDef{
		"worker": {Name: "worker", Role: "worker", Generation: agent.GenerationParams{Model: "test"}},
	}

	first := &Coordinator{
		session:         &TeamSession{Workspace: workspace, Config: config, Agents: agents},
		sessionData:     NewSession(),
		projectDir:      workspace,
		taskTracker:     NewTaskTracker(),
		reportStatus:    func(StatusEvent) {},
		taskResultCache: make(map[string][]cachedTaskEntry),
		executionRunID:  "run-before-crash",
	}
	first.taskTracker.TodoList().onChange = func() { first.saveCheckpoint() }
	item := first.taskTracker.TodoList().AddBatch([]TodoSpec{{
		Agent: "worker", Desc: "complete the already-run task",
		Execution: ExecutionContract{RequiresResult: true},
	}})[0]
	workerOutput := "checkpointed worker output"
	if err := first.taskTracker.TodoList().SetFailureEventAndOutput(item.ID, &FailureEventPayload{
		TaskID: item.ID, Phase: "protocol", FailureClass: FailureProtocol,
		RetryDisposition: ReconcileOnly, Summary: "missing submit_result",
	}, workerOutput); err != nil {
		t.Fatal(err)
	}
	if err := first.taskTracker.TodoList().TryUpdateStatusAndOutput(item.ID, TaskProtocolIncomplete, "missing submit_result", ""); err != nil {
		t.Fatal(err)
	}

	checkpoint := LoadSession(workspace)
	if checkpoint == nil || len(checkpoint.Tasks) != 1 || checkpoint.Tasks[0].Status != TaskProtocolIncomplete || checkpoint.Tasks[0].Output != workerOutput {
		t.Fatalf("protocol checkpoint lost status/output: %#v", checkpoint)
	}

	workerCalls := 0
	repairCalls := 0
	if err := os.WriteFile(filepath.Join(workspace, "resume-repair-artifact.txt"), []byte("repair evidence"), 0o644); err != nil {
		t.Fatal(err)
	}
	repairArtifactRejected := false
	second := &Coordinator{
		session:             &TeamSession{Workspace: workspace, Config: config, Agents: agents},
		sessionData:         NewSession(),
		projectDir:          workspace,
		taskTracker:         NewTaskTracker(),
		reportStatus:        func(StatusEvent) {},
		taskResultCache:     make(map[string][]cachedTaskEntry),
		executionRunID:      "run-after-crash",
		workerAgentOverride: &countingTextAgent{calls: &workerCalls, text: "UNSAFE WORKER REPLAY"},
	}
	second.SetSessionData(checkpoint)
	second.repairAgentOverride = &scriptedRepairAgent{
		calls: &repairCalls,
		onContext: func(ctx context.Context) {
			response, runErr := (&submitResultTool{coordinator: second, todoID: item.ID}).Run(ctx, fantasy.ToolCall{
				Name:  submitResultToolName,
				Input: `{"status":"success","summary":"repair","artifacts":[{"path":"resume-repair-artifact.txt"}]}`,
			})
			repairArtifactRejected = runErr == nil && response.IsError && strings.Contains(response.Content, "cannot add artifact evidence")
		},
		onCall: func(int) {
			second.storeSubmittedTaskResult(item.ID, &TaskResult{
				TaskID: item.ID, Agent: "worker", Status: "success",
				Summary: "result-only repair accepted", Source: "submitted",
			})
		},
	}

	resumed, err := second.ResumeInterruptedTasks(context.Background())
	if err != nil {
		t.Fatalf("protocol resume failed: %v", err)
	}
	if resumed != 1 {
		t.Fatalf("resumed count = %d, want 1", resumed)
	}
	if workerCalls != 0 {
		t.Fatalf("worker calls after protocol checkpoint = %d, want 0", workerCalls)
	}
	if repairCalls != 1 {
		t.Fatalf("result-only repair calls = %d, want 1", repairCalls)
	}
	if !repairArtifactRejected {
		t.Fatal("resumed result-only repair accepted artifact evidence")
	}
	updated := second.taskTracker.TodoList().Items()[0]
	if updated.Status != TaskDone || updated.TypedResult == nil || updated.TypedResult.Source != "submitted" {
		t.Fatalf("repaired task = %#v, want done with submitted result", updated)
	}
	if updated.Output != workerOutput {
		t.Fatalf("worker evidence changed during result-only repair: %q", updated.Output)
	}
	if persisted := LoadSession(workspace); persisted == nil || persisted.Tasks[0].Status != TaskDone {
		t.Fatalf("repaired terminal state was not checkpointed: %#v", persisted)
	}
}

// TestResumeInterruptedTasks_ProtocolCheckpointedSubmittedResultFinalizesLocally
// covers the crash boundary after submit_result checkpoints its TypedResult but
// before protocol repair writes its execution receipt or terminal status.
func TestResumeInterruptedTasks_ProtocolCheckpointedSubmittedResultFinalizesLocally(t *testing.T) {
	workspace := t.TempDir()
	config := agent.TeamConfig{Name: "protocol-submitted-resume", Timeout: 30, MaxRetries: 1}
	agents := map[string]*agent.AgentDef{
		"worker": {Name: "worker", Role: "worker", Generation: agent.GenerationParams{Model: "test"}},
	}

	first := &Coordinator{
		session:         &TeamSession{Workspace: workspace, Config: config, Agents: agents},
		sessionData:     NewSession(),
		projectDir:      workspace,
		taskTracker:     NewTaskTracker(),
		reportStatus:    func(StatusEvent) {},
		taskResultCache: make(map[string][]cachedTaskEntry),
		executionRunID:  "run-before-submitted-crash",
	}
	first.taskTracker.TodoList().onChange = func() { first.saveCheckpoint() }
	item := first.taskTracker.TodoList().AddBatch([]TodoSpec{{
		Agent: "worker", Desc: "finalize checkpointed submitted result",
		Execution: ExecutionContract{RequiresResult: true},
	}})[0]
	workerOutput := "durable worker output before submit_result crash"
	if err := first.taskTracker.TodoList().SetFailureEventAndOutput(item.ID, &FailureEventPayload{
		TaskID: item.ID, Phase: "protocol", FailureClass: FailureProtocol,
		RetryDisposition: ReconcileOnly, Summary: "missing submit_result",
	}, workerOutput); err != nil {
		t.Fatal(err)
	}
	if err := first.taskTracker.TodoList().TryUpdateStatusAndOutput(item.ID, TaskProtocolIncomplete, "missing submit_result", ""); err != nil {
		t.Fatal(err)
	}
	first.storeSubmittedTaskResult(item.ID, &TaskResult{
		TaskID: item.ID, Agent: "worker", Status: "success",
		Summary: "submitted result survived crash", Source: "submitted",
	})

	checkpoint := LoadSession(workspace)
	if checkpoint == nil || len(checkpoint.Tasks) != 1 || checkpoint.Tasks[0].TypedResult == nil || checkpoint.Tasks[0].ExecutionReceipt != nil {
		t.Fatalf("expected submitted-result-only checkpoint, got %#v", checkpoint)
	}

	workerCalls := 0
	repairCalls := 0
	second := &Coordinator{
		session:             &TeamSession{Workspace: workspace, Config: config, Agents: agents},
		sessionData:         NewSession(),
		projectDir:          workspace,
		taskTracker:         NewTaskTracker(),
		reportStatus:        func(StatusEvent) {},
		taskResultCache:     make(map[string][]cachedTaskEntry),
		executionRunID:      "run-after-submitted-crash",
		workerAgentOverride: &countingTextAgent{calls: &workerCalls, text: "UNSAFE WORKER REPLAY"},
		repairAgentOverride: &countingTextAgent{calls: &repairCalls, text: "UNSAFE REPAIR REPLAY"},
	}
	second.SetSessionData(checkpoint)

	resumed, err := second.ResumeInterruptedTasks(context.Background())
	if err != nil {
		t.Fatalf("submitted-result resume failed: %v", err)
	}
	if resumed != 1 || workerCalls != 0 || repairCalls != 0 {
		t.Fatalf("submitted-result resume = resumed %d, worker %d, repair %d; want 1/0/0", resumed, workerCalls, repairCalls)
	}
	updated := second.taskTracker.TodoList().Items()[0]
	if updated.Status != TaskDone || updated.TypedResult == nil || updated.TypedResult.Summary != "submitted result survived crash" || updated.Output != workerOutput {
		t.Fatalf("locally finalized task = %#v", updated)
	}
	if updated.ExecutionReceipt == nil || updated.ExecutionReceipt.RepairProvenance == nil || !updated.ExecutionReceipt.RepairProvenance.Success || updated.ExecutionReceipt.RepairProvenance.SubmittedResult == nil {
		t.Fatalf("missing materialized repair provenance: %#v", updated.ExecutionReceipt)
	}
	if len(updated.ExecutionReceipt.RepairProvenance.History) != 1 || !updated.ExecutionReceipt.RepairProvenance.History[0].Success {
		t.Fatalf("materialized repair history = %#v", updated.ExecutionReceipt.RepairProvenance.History)
	}
	if persisted := LoadSession(workspace); persisted == nil || persisted.Tasks[0].Status != TaskDone || persisted.Tasks[0].ExecutionReceipt == nil {
		t.Fatalf("local finalization was not checkpointed: %#v", persisted)
	}
}

func TestResumeInterruptedTasks_ProtocolSchemaRetryPreservesPriorHistory(t *testing.T) {
	workspace := t.TempDir()
	config := agent.TeamConfig{Name: "protocol-history-resume", Timeout: 30, MaxRetries: 1}
	agents := map[string]*agent.AgentDef{
		"worker": {Name: "worker", Role: "worker", Generation: agent.GenerationParams{Model: "test"}},
	}

	first := &Coordinator{
		session:         &TeamSession{Workspace: workspace, Config: config, Agents: agents},
		sessionData:     NewSession(),
		projectDir:      workspace,
		taskTracker:     NewTaskTracker(),
		reportStatus:    func(StatusEvent) {},
		taskResultCache: make(map[string][]cachedTaskEntry),
		executionRunID:  "run-before-schema-restart",
	}
	first.taskTracker.TodoList().onChange = func() { first.saveCheckpoint() }
	item := first.taskTracker.TodoList().AddBatch([]TodoSpec{{
		Agent: "worker", Desc: "preserve schema repair history",
		Execution: ExecutionContract{RequiresResult: true},
	}})[0]
	if err := first.taskTracker.TodoList().SetFailureEventAndOutput(item.ID, &FailureEventPayload{
		TaskID: item.ID, Phase: "protocol", FailureClass: FailureProtocol,
		RetryDisposition: ReconcileOnly, Summary: "missing submit_result",
	}, "worker output for schema repair"); err != nil {
		t.Fatal(err)
	}
	if err := first.taskTracker.TodoList().TryUpdateStatusAndOutput(item.ID, TaskProtocolIncomplete, "invalid schema", ""); err != nil {
		t.Fatal(err)
	}
	if err := first.taskTracker.TodoList().SetExecutionReceipt(item.ID, &ExecutionReceipt{
		RunID: "run-before-schema-restart", TaskID: item.ID, Attempt: 1, ProducerID: "worker",
		RepairProvenance: &RepairProvenance{
			Attempted: true, FailureReason: RepairFailureInvalidSchema, RepairAttempts: 1,
			History: []RepairAttemptProvenance{{Attempt: 1, FailureReason: RepairFailureInvalidSchema, Prompt: "first schema attempt"}},
		},
	}); err != nil {
		t.Fatal(err)
	}

	checkpoint := LoadSession(workspace)
	if checkpoint == nil {
		t.Fatal("missing schema-repair checkpoint")
	}
	repairCalls := 0
	second := &Coordinator{
		session:             &TeamSession{Workspace: workspace, Config: config, Agents: agents},
		sessionData:         NewSession(),
		projectDir:          workspace,
		taskTracker:         NewTaskTracker(),
		reportStatus:        func(StatusEvent) {},
		taskResultCache:     make(map[string][]cachedTaskEntry),
		executionRunID:      "run-after-schema-restart",
		workerAgentOverride: &countingTextAgent{calls: new(int), text: "UNSAFE WORKER REPLAY"},
	}
	second.SetSessionData(checkpoint)
	second.repairAgentOverride = &scriptedRepairAgent{
		calls: &repairCalls,
		onCall: func(int) {
			second.storeSubmittedTaskResult(item.ID, &TaskResult{TaskID: item.ID, Agent: "worker", Status: "success", Summary: "schema retry accepted", Source: "submitted"})
		},
	}

	if _, err := second.ResumeInterruptedTasks(context.Background()); err != nil {
		t.Fatalf("schema-retry resume failed: %v", err)
	}
	if repairCalls != 1 {
		t.Fatalf("schema-retry repair calls = %d, want 1", repairCalls)
	}
	updated := second.taskTracker.TodoList().Items()[0]
	if updated.ExecutionReceipt == nil || updated.ExecutionReceipt.RepairProvenance == nil {
		t.Fatalf("missing resumed repair provenance: %#v", updated.ExecutionReceipt)
	}
	provenance := updated.ExecutionReceipt.RepairProvenance
	if provenance.RepairAttempts != 2 || len(provenance.History) != 2 || provenance.History[0].FailureReason != RepairFailureInvalidSchema || !provenance.History[1].Success {
		t.Fatalf("schema-retry history was not preserved: %#v", provenance)
	}
}

// TestResumeInterruptedTasks_ProtocolCheckpointWithoutEvidenceBlocks verifies
// that a malformed/incomplete checkpoint cannot fall back to ordinary worker
// retry when there is no execution output to feed to result-only repair.
func TestResumeInterruptedTasks_ProtocolCheckpointWithoutEvidenceBlocks(t *testing.T) {
	workspace := t.TempDir()
	c := &Coordinator{
		session: &TeamSession{Workspace: workspace, Config: agent.TeamConfig{Name: "protocol-resume"}, Agents: map[string]*agent.AgentDef{
			"worker": {Name: "worker", Role: "worker"},
		}},
		sessionData: NewSession(), taskTracker: NewTaskTracker(),
		reportStatus: func(StatusEvent) {}, taskResultCache: make(map[string][]cachedTaskEntry),
	}
	c.taskTracker.TodoList().Restore([]*TodoItem{{
		ID: "1", Agent: "worker", Desc: "missing evidence", Status: TaskProtocolIncomplete,
		Execution: ExecutionContract{RequiresResult: true},
	}})
	workerCalls := 0
	c.workerAgentOverride = &countingTextAgent{calls: &workerCalls, text: "UNSAFE WORKER REPLAY"}
	c.repairAgentOverride = &mockWorkerTextAgent{text: "repair without submit_result"}

	if _, err := c.ResumeInterruptedTasks(context.Background()); err == nil {
		t.Fatal("expected missing protocol evidence to fail closed")
	}
	updated := c.taskTracker.TodoList().Items()[0]
	if updated.Status != TaskBlocked {
		t.Fatalf("missing-evidence status = %s, want blocked", updated.Status)
	}
	if workerCalls != 0 {
		t.Fatalf("worker calls for missing protocol evidence = %d, want 0", workerCalls)
	}
	if updated.FailureEvent == nil || updated.FailureEvent.FailureClass != FailureProtocol || updated.FailureEvent.RetryDisposition != NeedsHuman {
		t.Fatalf("missing-evidence failure event = %#v", updated.FailureEvent)
	}
}

// TestExecuteTask_ProtocolIncompleteUsesResultOnlyRepair covers the second
// boundary: callers that enter executeTask directly cannot bypass the resume
// gate and replay the worker either.
func TestExecuteTask_ProtocolIncompleteUsesResultOnlyRepair(t *testing.T) {
	workspace := t.TempDir()
	c := &Coordinator{
		session: &TeamSession{Workspace: workspace, Config: agent.TeamConfig{Name: "protocol-direct"}, Agents: map[string]*agent.AgentDef{
			"worker": {Name: "worker", Role: "worker"},
		}},
		sessionData: NewSession(), taskTracker: NewTaskTracker(), projectDir: workspace,
		reportStatus: func(StatusEvent) {}, taskResultCache: make(map[string][]cachedTaskEntry),
	}
	item := &TodoItem{
		ID: "1", Agent: "worker", Desc: "direct protocol repair", Status: TaskProtocolIncomplete,
		Output: "original execution evidence", Execution: ExecutionContract{RequiresResult: true},
	}
	c.taskTracker.TodoList().Restore([]*TodoItem{item})
	workerCalls := 0
	repairCalls := 0
	c.workerAgentOverride = &countingTextAgent{calls: &workerCalls, text: "UNSAFE WORKER REPLAY"}
	c.repairAgentOverride = &scriptedRepairAgent{
		calls: &repairCalls,
		onCall: func(int) {
			c.storeSubmittedTaskResult(item.ID, &TaskResult{TaskID: item.ID, Agent: "worker", Status: "success", Summary: "direct repair", Source: "submitted"})
		},
	}

	if _, err := c.executeTask(context.Background(), taskDefFromTodoItem(item), item.ID); err != nil {
		t.Fatalf("direct protocol repair failed: %v", err)
	}
	if workerCalls != 0 || repairCalls != 1 {
		t.Fatalf("direct protocol calls: worker=%d repair=%d, want worker=0 repair=1", workerCalls, repairCalls)
	}
	if got := c.taskTracker.TodoList().Items()[0].Status; got != TaskDone {
		t.Fatalf("direct protocol status = %s, want done", got)
	}
}

func TestTodoIDLess(t *testing.T) {
	if !todoIDLess("2", "10") {
		t.Error("numeric IDs must order numerically: 2 < 10")
	}
	if todoIDLess("10", "2") {
		t.Error("10 should not be < 2")
	}
	if !todoIDLess("a", "b") {
		t.Error("non-numeric IDs fall back to string order")
	}
}

func TestGetInterruptedTasks_Selection(t *testing.T) {
	c := newBudgetCoordinator(t)
	c.taskTracker.TodoList().Restore([]*TodoItem{
		{ID: "1", Agent: "a", Desc: "done task", Status: TaskDone, Output: "out"},
		{ID: "2", Agent: "a", Desc: "in-flight task", Status: TaskInProgress},
		{ID: "2v", Agent: "a", Desc: "verifying task", Status: TaskVerifying},
		{ID: "3", Agent: "b", Desc: "never started", Status: TaskPending},
		{ID: "4", Agent: "b", Desc: "failed task", Status: TaskError},
		{ID: "5", Agent: "c", Desc: "planned task", Status: TaskPlanned},
		{ID: "6", Agent: "c", Desc: "skipped task", Status: TaskSkipped},
	})

	got := c.getInterruptedTasks()

	// Only in-flight / verifying / pending / planned are selected (2,2v,3,5);
	// done/error/skipped excluded. Selection must not mutate status.
	wantIDs := map[string]bool{"2": true, "2v": true, "3": true, "5": true}
	if len(got) != len(wantIDs) {
		t.Fatalf("expected %d interrupted tasks, got %d", len(wantIDs), len(got))
	}
	for _, it := range got {
		if !wantIDs[it.ID] {
			t.Errorf("unexpected task selected for resume: %s (%s)", it.ID, it.Desc)
		}
	}

	// Ascending-ID order (deps run first).
	if got[0].ID != "2" || got[1].ID != "2v" || got[2].ID != "3" || got[3].ID != "5" {
		t.Errorf("interrupted tasks not in ascending ID order: %s,%s,%s,%s", got[0].ID, got[1].ID, got[2].ID, got[3].ID)
	}

	// Selection is read-only; no status changes.
	byID := map[string]*TodoItem{}
	for _, it := range c.taskTracker.TodoList().Items() {
		byID[it.ID] = it
	}
	if byID["1"].Status != TaskDone {
		t.Errorf("done task must remain done, got %s", byID["1"].Status)
	}
	if byID["2"].Status != TaskInProgress {
		t.Errorf("in-flight task must remain in_progress, got %s", byID["2"].Status)
	}
	if byID["4"].Status != TaskError {
		t.Errorf("error task must remain error, got %s", byID["4"].Status)
	}
	if byID["6"].Status != TaskSkipped {
		t.Errorf("skipped task must remain skipped, got %s", byID["6"].Status)
	}
}

func TestResumeInterruptedTasks_NoopWhenNothingInterrupted(t *testing.T) {
	c := newBudgetCoordinator(t)
	c.taskTracker.TodoList().Restore([]*TodoItem{
		{ID: "1", Agent: "a", Desc: "done", Status: TaskDone, Output: "x"},
		{ID: "2", Agent: "a", Desc: "skipped", Status: TaskSkipped},
	})
	n, err := c.ResumeInterruptedTasks(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n != 0 {
		t.Errorf("expected 0 tasks re-driven, got %d", n)
	}
}

func TestResumeInterruptedTasks_EmptyTodoList(t *testing.T) {
	c := newBudgetCoordinator(t)
	n, err := c.ResumeInterruptedTasks(context.Background())
	if err != nil || n != 0 {
		t.Errorf("fresh run should be a no-op, got n=%d err=%v", n, err)
	}
}
