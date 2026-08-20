package team

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kjelly/hufu/internal/agent"
)

func TestCompareCanonicalProjectionMatchesCurrentTaskEvent(t *testing.T) {
	item := &TodoItem{ID: "task-1", Agent: "worker", Desc: "write report", Status: TaskDone, Detail: "done", Output: "report", Model: "model", Execution: ExecutionContract{Kind: ExecutionKindExternal}}
	payload, err := taskTransitionPayloadJSON(item)
	if err != nil {
		t.Fatal(err)
	}
	live := NewSession()
	live.Tasks = []*TodoItem{item}
	events := []RunEvent{{SchemaVersion: eventStoreSchemaVersion, Type: string(EventTaskCompleted), TaskID: item.ID, Payload: payload}}
	if err := CompareCanonicalProjection(live, events); err != nil {
		t.Fatalf("matching projection rejected: %v", err)
	}
}

func TestCompareCanonicalProjectionReportsMismatch(t *testing.T) {
	live := NewSession()
	live.Tasks = []*TodoItem{{ID: "task-1", Agent: "worker", Desc: "checkpoint", Status: TaskDone}}
	events := []RunEvent{{SchemaVersion: eventStoreSchemaVersion, Type: string(EventTaskCompleted), TaskID: "task-1", Payload: []byte(`{"id":"task-1","status":"done","agent":"worker","desc":"different"}`)}}
	if err := CompareCanonicalProjection(live, events); err == nil {
		t.Fatal("projection mismatch was accepted")
	}
}

func TestInitEventStoreMarksCanonicalProjectionMismatchForRecovery(t *testing.T) {
	workspace := t.TempDir()
	store, err := NewEventStore(workspace, "run-1", "session-1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.AppendPersisted(RunEvent{
		Type: string(EventTaskCompleted), Actor: "worker", TaskID: "task-1",
		Payload: []byte(`{"id":"task-1","status":"done","agent":"worker","desc":"event-store"}`),
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	c := &Coordinator{
		session:     &TeamSession{Workspace: workspace},
		sessionData: NewSession(),
		taskTracker: NewTaskTracker(),
	}
	c.sessionData.Tasks = []*TodoItem{{ID: "task-1", Agent: "worker", Desc: "checkpoint", Status: TaskDone}}
	c.initEventStore()
	defer c.eventStore.Close()
	if !c.sessionData.RecoveryRequired || c.sessionData.RecoveryReason == "" {
		t.Fatalf("projection mismatch was not marked for recovery: %#v", c.sessionData)
	}
	if saved := LoadSession(workspace); saved == nil || !saved.RecoveryRequired {
		t.Fatalf("recovery-required projection marker was not persisted: %#v", saved)
	}
}

func taskTransitionPayloadJSON(item *TodoItem) ([]byte, error) {
	return json.Marshal(taskTransitionPayload(item))
}

func TestInitEventStoreMarksCorruptedHashChainForRecovery(t *testing.T) {
	workspace := t.TempDir()
	store, err := NewEventStore(workspace, "run-1", "session-1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.AppendPersisted(RunEvent{
		Type: string(EventTaskCompleted), Actor: "worker", TaskID: "task-1",
		Payload: []byte(`{"id":"task-1","status":"done","agent":"worker","desc":"event-store"}`),
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	// Corrupt the hash chain in event_store.jsonl
	storePath := filepath.Join(workspace, logsDir, eventStoreFile)
	data, err := os.ReadFile(storePath)
	if err != nil {
		t.Fatal(err)
	}
	corrupted := strings.Replace(string(data), `"id":"task-1"`, `"id":"task-corrupted"`, 1)
	if err := os.WriteFile(storePath, []byte(corrupted), 0o644); err != nil {
		t.Fatal(err)
	}

	c := &Coordinator{
		session:     &TeamSession{Workspace: workspace},
		sessionData: NewSession(),
		taskTracker: NewTaskTracker(),
	}
	c.initEventStore()
	if !c.sessionData.RecoveryRequired || c.sessionData.RecoveryReason == "" {
		t.Fatalf("corrupted event store did not mark recovery-required: %#v", c.sessionData)
	}
	saved := LoadSession(workspace)
	if saved == nil || !saved.RecoveryRequired {
		t.Fatalf("recovery marker was not persisted: %#v", saved)
	}
}

func TestInitEventStoreRebuildsEmptyCheckpointFromEventStore(t *testing.T) {
	workspace := t.TempDir()
	store, err := NewEventStore(workspace, "run-1", "session-1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.AppendPersisted(RunEvent{
		Type: string(EventTaskCreated), Actor: "worker", TaskID: "task-1",
		Payload: []byte(`{"id":"task-1","status":"pending","agent":"worker","desc":"build"}`),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AppendPersisted(RunEvent{
		Type: string(EventTaskCompleted), Actor: "worker", TaskID: "task-1",
		Payload: []byte(`{"id":"task-1","status":"done","agent":"worker","desc":"build","output":"built"}`),
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	c := &Coordinator{
		session:     &TeamSession{Workspace: workspace},
		sessionData: NewSession(),
		taskTracker: NewTaskTracker(),
	}
	c.initEventStore()
	defer c.eventStore.Close()

	if c.sessionData.RecoveryRequired {
		t.Fatalf("rebuilt empty projection was unexpectedly marked for recovery: %s", c.sessionData.RecoveryReason)
	}
	if len(c.sessionData.Tasks) != 1 || c.sessionData.Tasks[0].Status != TaskDone || c.sessionData.Tasks[0].Output != "built" {
		t.Fatalf("rebuilt tasks = %#v, want 1 completed task", c.sessionData.Tasks)
	}
	saved := LoadSession(workspace)
	if saved == nil || len(saved.Tasks) != 1 || saved.Tasks[0].Status != TaskDone {
		t.Fatalf("rebuilt session was not persisted: %#v", saved)
	}
}

func TestInitEventStoreRepairsStaleCheckpointAfterCrash(t *testing.T) {
	workspace := t.TempDir()
	store, err := NewEventStore(workspace, "run-1", "session-1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.AppendPersisted(RunEvent{
		Type: string(EventUserMessageAdded), Actor: "user",
		Payload: []byte(`{"role":"user","content":"hello"}`),
	}); err != nil {
		t.Fatal(err)
	}
	// Initial session saved with 1 message
	initialSD := NewSession()
	initialSD.AddEntry("user", "hello")
	if err := SaveSession(workspace, initialSD); err != nil {
		t.Fatal(err)
	}
	// Second event appended to event store, but crash before session.json saved
	if _, err := store.AppendPersisted(RunEvent{
		Type: string(EventAssistantMessageAdded), Actor: "assistant",
		Payload: []byte(`{"role":"assistant","content":"world"}`),
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	c := &Coordinator{
		session:     &TeamSession{Workspace: workspace},
		sessionData: LoadSession(workspace),
		taskTracker: NewTaskTracker(),
	}
	c.initEventStore()
	defer c.eventStore.Close()

	if c.sessionData.RecoveryRequired {
		t.Fatalf("crash-repaired projection was marked for recovery: %s", c.sessionData.RecoveryReason)
	}
	if len(c.sessionData.Entries) != 2 || c.sessionData.Entries[1].Content != "world" {
		t.Fatalf("repaired session entries = %#v, want 2 entries", c.sessionData.Entries)
	}
	saved := LoadSession(workspace)
	if saved == nil || len(saved.Entries) != 2 {
		t.Fatalf("repaired session was not persisted: %#v", saved)
	}
}

func TestInitEventStoreBranchLineageReplay(t *testing.T) {
	workspace := t.TempDir()
	st := NewSessionTree()
	st.Branches["feature"] = &SessionBranch{
		ID:        "feature",
		Name:      "feature",
		CreatedAt: "2026-01-01T00:00:00Z",
	}
	st.ActiveBranch = "feature"
	if err := SaveSessionTree(workspace, st); err != nil {
		t.Fatal(err)
	}

	store, err := NewEventStore(workspace, "run-1", "session-1")
	if err != nil {
		t.Fatal(err)
	}
	// Event for main branch
	store.SetBranchID("main")
	if _, err := store.AppendPersisted(RunEvent{
		Type: string(EventTaskCreated), Actor: "worker", TaskID: "task-main",
		Payload: []byte(`{"id":"task-main","status":"pending","agent":"worker","desc":"main task"}`),
	}); err != nil {
		t.Fatal(err)
	}
	// Event for feature branch
	store.SetBranchID("feature")
	if _, err := store.AppendPersisted(RunEvent{
		Type: string(EventTaskCreated), Actor: "worker", TaskID: "task-feat",
		Payload: []byte(`{"id":"task-feat","status":"pending","agent":"worker","desc":"feature task"}`),
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	c := &Coordinator{
		session:     &TeamSession{Workspace: workspace},
		sessionData: NewSession(),
		taskTracker: NewTaskTracker(),
	}
	c.initEventStore()
	defer c.eventStore.Close()

	if len(c.sessionData.Tasks) != 1 || c.sessionData.Tasks[0].ID != "task-feat" {
		t.Fatalf("feature branch replayed tasks = %#v, want only task-feat", c.sessionData.Tasks)
	}
}

func TestInitEventStore_MalformedSessionTreeMarksRecoveryRequired(t *testing.T) {
	workspace := t.TempDir()
	store, err := NewEventStore(workspace, "run-1", "session-1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.AppendPersisted(RunEvent{
		Type: string(EventTaskCreated), Actor: "worker", TaskID: "task-1",
		Payload: []byte(`{"id":"task-1","status":"pending","agent":"worker","desc":"build"}`),
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	// Write malformed session_tree.json
	treePath := filepath.Join(workspace, sessionTreeFile)
	if err := os.WriteFile(treePath, []byte(`{ invalid json`), 0o644); err != nil {
		t.Fatal(err)
	}

	c := &Coordinator{
		session:     &TeamSession{Workspace: workspace},
		sessionData: NewSession(),
		taskTracker: NewTaskTracker(),
	}
	c.initEventStore()
	if !c.sessionData.RecoveryRequired || !strings.Contains(c.sessionData.RecoveryReason, "session-tree load failed") {
		t.Fatalf("malformed session tree did not mark recovery required: %#v", c.sessionData)
	}
	saved := LoadSession(workspace)
	if saved == nil || !saved.RecoveryRequired {
		t.Fatalf("recovery required not persisted: %#v", saved)
	}
}

func TestRunAndRunDirectAgent_RecoveryRequiredDeniesAdmissionAndMakesNoCalls(t *testing.T) {
	workspace := t.TempDir()
	store, err := NewEventStore(workspace, "run-1", "session-1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.AppendPersisted(RunEvent{
		Type: string(EventTaskCompleted), Actor: "worker", TaskID: "task-1",
		Payload: []byte(`{"id":"task-1","status":"done","agent":"worker","desc":"event-store"}`),
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	// Corrupt event store log
	storePath := filepath.Join(workspace, logsDir, eventStoreFile)
	data, err := os.ReadFile(storePath)
	if err != nil {
		t.Fatal(err)
	}
	corrupted := strings.Replace(string(data), `"id":"task-1"`, `"id":"task-corrupted"`, 1)
	if err := os.WriteFile(storePath, []byte(corrupted), 0o644); err != nil {
		t.Fatal(err)
	}

	c := &Coordinator{
		session:     &TeamSession{Workspace: workspace},
		sessionData: NewSession(),
		taskTracker: NewTaskTracker(),
	}

	// Direct agent call should fail admission without executing
	directRes, directErr := c.RunDirectAgent(t.Context(), "helper", "some direct task")
	if directErr == nil || directRes != nil {
		t.Fatalf("RunDirectAgent succeeded or returned non-nil on corrupted store: res=%#v, err=%v", directRes, directErr)
	}
	if !strings.Contains(directErr.Error(), "recovery required") {
		t.Fatalf("RunDirectAgent error = %v, want recovery required", directErr)
	}
	lastResult := c.LastRunResult()
	if lastResult == nil || lastResult.Outcome != RunOutcomeBlocked || lastResult.GoalSatisfied {
		t.Fatalf("RunDirectAgent did not record blocked terminal outcome: %#v", lastResult)
	}

	// Run call should fail admission without executing
	runRes, runErr := c.Run(t.Context(), "some coordinator task")
	if runErr == nil || runRes != "" {
		t.Fatalf("Run succeeded or returned non-empty on corrupted store: res=%q, err=%v", runRes, runErr)
	}
	if !strings.Contains(runErr.Error(), "recovery required") {
		t.Fatalf("Run error = %v, want recovery required", runErr)
	}
	lastResult = c.LastRunResult()
	if lastResult == nil || lastResult.Outcome != RunOutcomeBlocked || lastResult.GoalSatisfied {
		t.Fatalf("Run did not record blocked terminal outcome: %#v", lastResult)
	}
}

func TestInitEventStore_NonPrefixTaskStateRejectsRecovery(t *testing.T) {
	workspace := t.TempDir()
	store, err := NewEventStore(workspace, "run-1", "session-1")
	if err != nil {
		t.Fatal(err)
	}
	// Event store only has task-1 in_progress
	if _, err := store.AppendPersisted(RunEvent{
		Type: string(EventTaskStarted), Actor: "worker", TaskID: "task-1",
		Payload: []byte(`{"id":"task-1","status":"in_progress","agent":"worker","desc":"build"}`),
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	// Checkpoint has task-1 done with execution receipt (crash after side effect)
	liveSD := NewSession()
	liveSD.Tasks = []*TodoItem{{
		ID:     "task-1",
		Agent:  "worker",
		Desc:   "build",
		Status: TaskDone,
		Output: "finished",
		ExecutionReceipt: &ExecutionReceipt{
			TaskID:     "task-1",
			ProducerID: "worker",
			Attempt:    1,
		},
	}}
	if err := SaveSession(workspace, liveSD); err != nil {
		t.Fatal(err)
	}

	c := &Coordinator{
		session:     &TeamSession{Workspace: workspace},
		sessionData: LoadSession(workspace),
		taskTracker: NewTaskTracker(),
	}
	c.initEventStore()
	defer c.eventStore.Close()

	if !c.sessionData.RecoveryRequired || !strings.Contains(c.sessionData.RecoveryReason, "mismatch") {
		t.Fatalf("divergent non-prefix task state was not marked recovery-required: %#v", c.sessionData)
	}
}

func TestTwoBranchIdempotencyAndMemoryReplayIsolation(t *testing.T) {
	workspace := t.TempDir()
	st := NewSessionTree()
	st.Branches["feature"] = &SessionBranch{
		ID:        "feature",
		Name:      "feature",
		CreatedAt: "2026-01-01T00:00:00Z",
	}
	st.ActiveBranch = "feature"
	if err := SaveSessionTree(workspace, st); err != nil {
		t.Fatal(err)
	}

	store, err := NewEventStore(workspace, "run-1", "session-1")
	if err != nil {
		t.Fatal(err)
	}
	// Main branch has task-1 transition and memory observation
	store.SetBranchID("main")
	if _, err := store.AppendPersisted(RunEvent{
		Type:           string(EventTaskCompleted),
		Actor:          "worker",
		TaskID:         "task-1",
		IdempotencyKey: "task-1:done:1",
		Payload:        []byte(`{"id":"task-1","status":"done","agent":"worker","desc":"main build"}`),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AppendPersisted(RunEvent{
		Type:           "memory_retrieved",
		Actor:          "coordinator",
		TaskID:         "task-1",
		IdempotencyKey: "mem-main-1",
		Payload:        []byte(`{"query":"main query"}`),
	}); err != nil {
		t.Fatal(err)
	}

	// Feature branch has task-1 pending
	store.SetBranchID("feature")
	if _, err := store.AppendPersisted(RunEvent{
		Type:    string(EventTaskCreated),
		Actor:   "worker",
		TaskID:  "task-1",
		Payload: []byte(`{"id":"task-1","status":"pending","agent":"worker","desc":"feature build"}`),
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	c := &Coordinator{
		session:     &TeamSession{Workspace: workspace},
		sessionData: NewSession(),
		taskTracker: NewTaskTracker(),
	}
	c.initEventStore()
	defer c.eventStore.Close()

	// Feature branch should NOT have hydrated main's task-1:done:1 idempotency key
	c.eventOnceMu.Lock()
	mainEmitted := c.emittedTaskTransitions["task-1:done:1"]
	c.eventOnceMu.Unlock()
	if mainEmitted {
		t.Fatal("feature branch hydrated main branch's task-1:done:1 idempotency key")
	}

	// Feature branch must be able to emit its own task-1:done:1 transition
	emitted, err := c.emitEventOnce("task-1:done:1", RunEvent{
		Type:    string(EventTaskCompleted),
		Actor:   "worker",
		TaskID:  "task-1",
		Payload: []byte(`{"id":"task-1","status":"done","agent":"worker","desc":"feature build"}`),
	})
	if err != nil || !emitted {
		t.Fatalf("feature branch could not emit its own transition: emitted=%v, err=%v", emitted, err)
	}
}

func TestContinueWithPrompt_RecoveryRequiredDeniesAdmissionAndMakesNoCalls(t *testing.T) {
	workspace := t.TempDir()
	c := &Coordinator{
		session: &TeamSession{
			Workspace: workspace,
			Config:    agent.TeamConfig{Name: "test-team"},
		},
		sessionData: NewSession(),
		taskTracker: NewTaskTracker(),
	}
	c.sessionData.RecoveryRequired = true
	c.sessionData.RecoveryReason = "corrupted projection test"

	result, err := c.ContinueWithPrompt(context.Background(), "continuation prompt")
	if err == nil {
		t.Fatal("expected ContinueWithPrompt to fail admission on recovery required")
	}
	if !strings.Contains(err.Error(), "recovery required: corrupted projection test") {
		t.Fatalf("unexpected error message: %v", err)
	}
	if result != "" {
		t.Fatalf("expected empty result, got %q", result)
	}
	lastResult := c.LastRunResult()
	if lastResult == nil || lastResult.Outcome != RunOutcomeBlocked || lastResult.GoalSatisfied {
		t.Fatalf("expected blocked terminal outcome, got %#v", lastResult)
	}
}

func TestBeginExecutionRun_InvalidWorkspaceMarksRecoveryRequiredAndDeniesAdmission(t *testing.T) {
	invalidWorkspace := "/dev/null/impossible/workspace"
	c := &Coordinator{
		session: &TeamSession{
			Workspace: invalidWorkspace,
			Config:    agent.TeamConfig{Name: "test-team"},
		},
		sessionData: NewSession(),
		taskTracker: NewTaskTracker(),
	}

	result, err := c.Run(context.Background(), "do something")
	if err == nil {
		t.Fatal("expected Run to fail admission with invalid workspace")
	}
	if !strings.Contains(err.Error(), "recovery required") {
		t.Fatalf("expected recovery required error, got: %v", err)
	}
	if result != "" {
		t.Fatalf("expected empty result, got %q", result)
	}
	if !c.sessionData.RecoveryRequired {
		t.Fatal("expected RecoveryRequired to be set on coordinator sessionData")
	}
	lastResult := c.LastRunResult()
	if lastResult == nil || IsRunOutcomeSuccess(lastResult.Outcome) || lastResult.ExitCode == 0 {
		t.Fatalf("expected non-success result when terminal persistence is unavailable, got %#v", lastResult)
	}
}

func TestCompareCanonicalProjection_RejectsTypedResultMismatch(t *testing.T) {
	item1 := &TodoItem{
		ID:     "task-1",
		Agent:  "worker",
		Desc:   "task with artifact",
		Status: TaskDone,
		TypedResult: &TaskResult{
			Artifacts: []ArtifactRef{{Path: "artifact-1.txt"}},
		},
	}
	item2 := &TodoItem{
		ID:     "task-1",
		Agent:  "worker",
		Desc:   "task with artifact",
		Status: TaskDone,
		TypedResult: &TaskResult{
			Artifacts: []ArtifactRef{{Path: "artifact-2.txt"}},
		},
	}

	err := compareSingleTaskProjection(item1, item2)
	if err == nil {
		t.Fatal("expected compareSingleTaskProjection to reject typed result artifact mismatch")
	}
	if !strings.Contains(err.Error(), "canonical parity mismatch") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestCompareCanonicalProjection_RejectsReceiptProducerMismatch(t *testing.T) {
	item1 := &TodoItem{
		ID:     "task-1",
		Agent:  "worker",
		Desc:   "task with receipt",
		Status: TaskDone,
		ExecutionReceipt: &ExecutionReceipt{
			RunID:      "run-1",
			TaskID:     "task-1",
			Attempt:    1,
			ProducerID: "worker-agent-a",
		},
	}
	item2 := &TodoItem{
		ID:     "task-1",
		Agent:  "worker",
		Desc:   "task with receipt",
		Status: TaskDone,
		ExecutionReceipt: &ExecutionReceipt{
			RunID:      "run-1",
			TaskID:     "task-1",
			Attempt:    1,
			ProducerID: "worker-agent-b",
		},
	}

	err := compareSingleTaskProjection(item1, item2)
	if err == nil {
		t.Fatal("expected compareSingleTaskProjection to reject receipt producer mismatch")
	}
	if !strings.Contains(err.Error(), "canonical parity mismatch") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestInitEventStoreRepairsLiveTodoListAndDelegationPolicy(t *testing.T) {
	workspace := t.TempDir()
	policy := agent.DelegationPolicy{
		InitialBatch:             []string{"worker"},
		RequireExactInitialBatch: true,
	}

	// 1. Initial checkpoint saved with initial_pending and no tasks
	initialSD := NewSession()
	initialSD.DelegationPhase = DelegationPhaseInitialPending
	if err := SaveSession(workspace, initialSD); err != nil {
		t.Fatal(err)
	}

	// 2. An initial task_created event is appended to event store, but crash before checkpoint
	store, err := NewEventStore(workspace, "run-1", "session-1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.AppendPersisted(RunEvent{
		Type:    string(EventTaskCreated),
		Actor:   "worker",
		TaskID:  "1",
		Payload: []byte(`{"id":"1","status":"pending","agent":"worker","desc":"initial task"}`),
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	// 3. Normal resume path: LoadSession -> construct Coordinator -> SetSessionData -> initEventStore
	c := &Coordinator{
		session: &TeamSession{
			Workspace: workspace,
			Config: agent.TeamConfig{
				Name:       "replay-team",
				Delegation: policy,
			},
		},
		taskTracker: NewTaskTracker(),
	}
	loaded := LoadSession(workspace)
	c.SetSessionData(loaded)
	c.initEventStore()
	defer c.eventStore.Close()

	// 4. Assert in this same process:
	// - TodoList has the replayed task
	items := c.taskTracker.TodoList().Items()
	if len(items) != 1 || items[0].ID != "1" {
		t.Fatalf("TodoList items = %#v, want 1 task with ID 1", items)
	}
	// - DelegationPhase is active, not initial_pending
	if got := c.delegationPhase(); got != DelegationPhaseActive {
		t.Fatalf("delegationPhase = %q, want %q", got, DelegationPhaseActive)
	}
	if c.initialDelegationPending() {
		t.Fatal("initialDelegationPending = true, want false")
	}
	// - sessionData.Tasks has the task
	if len(c.sessionData.Tasks) != 1 || c.sessionData.Tasks[0].ID != "1" {
		t.Fatalf("sessionData.Tasks = %#v, want 1 task with ID 1", c.sessionData.Tasks)
	}
	// - getInterruptedTasks (resume/scheduling) sees the task exactly once
	interrupted := c.getInterruptedTasks()
	if len(interrupted) != 1 || interrupted[0].ID != "1" {
		t.Fatalf("interrupted tasks = %#v, want 1 task with ID 1", interrupted)
	}
	// - Session tree branch replay agrees
	st, _ := LoadSessionTree(workspace)
	if st == nil {
		st = NewSessionTree()
	}
	if err := RebuildSessionForBranch(workspace, st, c.eventStore, "main"); err != nil {
		t.Fatalf("RebuildSessionForBranch failed: %v", err)
	}
	rebuiltSD := LoadSession(workspace)
	if rebuiltSD == nil || len(rebuiltSD.Tasks) != 1 || rebuiltSD.Tasks[0].ID != "1" {
		t.Fatalf("rebuiltSD.Tasks = %#v, want 1 task with ID 1", rebuiltSD.Tasks)
	}
}
