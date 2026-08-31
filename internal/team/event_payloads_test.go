package team

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/kjelly/hufu/internal/agent"
)

func TestValidateEventPayloadSchemaV2FailsClosedForMalformedTerminalTask(t *testing.T) {
	err := ValidateEventPayload(RunEvent{
		SchemaVersion: eventStoreSchemaVersion + 1,
		ID:            "evt-1",
		RunID:         "run-1",
		SessionID:     "session-1",
		TaskID:        "task-1",
		Actor:         "worker",
		Type:          string(EventTaskCompleted),
		Timestamp:     "2026-01-01T00:00:00Z",
		Payload:       []byte(`{"id":"task-1"}`),
	})
	if err == nil {
		t.Fatal("malformed terminal task payload was accepted")
	}
}

func TestValidateEventPayloadAllowsLegacyAndUnknownEvents(t *testing.T) {
	legacy := RunEvent{SchemaVersion: 1, Type: "task_started", Payload: []byte(`{}`)}
	if err := ValidateEventPayload(legacy); err != nil {
		t.Fatalf("legacy event rejected: %v", err)
	}
	unknown := RunEvent{SchemaVersion: 2, ID: "evt-1", RunID: "run-1", SessionID: "session-1", Actor: "newer-runtime", Type: "future_event", Timestamp: "2026-01-01T00:00:00Z", Payload: []byte(`{"v":1}`)}
	if err := ValidateEventPayload(unknown); err != nil {
		t.Fatalf("forward-compatible event rejected: %v", err)
	}
}

func TestValidateEventPayloadAcceptsTaskRemovedAndResolution(t *testing.T) {
	for _, tc := range []struct {
		eventType string
		payload   string
	}{
		{string(EventTaskCancelled), `{"id":"task-1","status":"error","failure_class":"cancelled","summary":"cancelled"}`},
		{string(EventTaskRemoved), `{"id":"task-1","status":"pending","desc":"suppressed"}`},
		{string(EventTaskResolution), `{"id":"task-1","status":"error","resolution":{"status":"reconciled","resolved_by":"task-2"}}`},
	} {
		event := RunEvent{
			SchemaVersion: eventStoreSchemaVersion,
			ID:            "evt-1",
			RunID:         "run-1",
			SessionID:     "session-1",
			TaskID:        "task-1",
			Actor:         "worker",
			Type:          tc.eventType,
			Timestamp:     "2026-01-01T00:00:00Z",
			Payload:       []byte(tc.payload),
		}
		if err := ValidateEventPayload(event); err != nil {
			t.Fatalf("%s rejected: %v", tc.eventType, err)
		}
	}
}

func TestEventStoreRedactsBeforeHashAndPersistence(t *testing.T) {
	store, err := NewEventStore(t.TempDir(), "run-1", "session-1")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	durable, err := store.AppendPersisted(RunEvent{Type: string(EventUserMessageAdded), Actor: "user", Payload: []byte(`{"role":"user","content":"api_key=top-secret"}`)})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(durable.Payload), "top-secret") {
		t.Fatalf("durable payload leaked secret: %s", durable.Payload)
	}
	if got := ComputeEventHash(durable.PreviousHash, durable.ID, durable.Type, durable.Timestamp, durable.Payload); got != durable.Hash {
		t.Fatalf("hash = %s, want hash over redacted persisted payload %s", durable.Hash, got)
	}
}

func TestEventStorePreservesRunFinishedContextTelemetryTypes(t *testing.T) {
	store, err := NewEventStore(t.TempDir(), "run-finished-telemetry", "session-1")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	payload, err := json.Marshal(RunFinishedEventPayload{
		Outcome: RunOutcomeFailed,
		Metrics: &RunMetrics{ContextWindowTelemetry: ContextWindowTelemetrySummary{
			LastRequestedTokens: 94029,
			LastAvailableTokens: 93232,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	durable, err := store.AppendPersisted(RunEvent{
		Type:    string(EventRunFinished),
		Actor:   "coordinator",
		Payload: payload,
	})
	if err != nil {
		t.Fatalf("append run_finished: %v", err)
	}

	var got RunFinishedEventPayload
	if err := json.Unmarshal(durable.Payload, &got); err != nil {
		t.Fatalf("decode durable run_finished: %v", err)
	}
	if got.Metrics == nil || got.Metrics.ContextWindowTelemetry.LastRequestedTokens != 94029 || got.Metrics.ContextWindowTelemetry.LastAvailableTokens != 93232 {
		t.Fatalf("context telemetry = %#v, want numeric values preserved", got.Metrics)
	}
}

func TestEventStoreIdempotencyReturnsOriginalDurableEvent(t *testing.T) {
	store, err := NewEventStore(t.TempDir(), "run-1", "session-1")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	first, err := store.AppendPersisted(RunEvent{Type: string(EventUserMessageAdded), Actor: "user", IdempotencyKey: "message-1", Payload: []byte(`{"role":"user","content":"first"}`)})
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.AppendPersisted(RunEvent{Type: string(EventUserMessageAdded), Actor: "user", IdempotencyKey: "message-1", Payload: []byte(`{"role":"user","content":"retry"}`)})
	if err != nil {
		t.Fatal(err)
	}
	if second.ID != first.ID || second.Hash != first.Hash || string(second.Payload) != string(first.Payload) {
		t.Fatalf("idempotent append returned non-durable event: first=%#v second=%#v", first, second)
	}
}

func TestAppendPersistedWritesCurrentStrictSchema(t *testing.T) {
	store, err := NewEventStore(t.TempDir(), "run-1", "session-1")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	event, err := store.AppendPersisted(RunEvent{Type: string(EventTaskStarted), Actor: "worker", TaskID: "task-1", Payload: []byte(`{"id":"task-1","status":"in_progress"}`)})
	if err != nil {
		t.Fatal(err)
	}
	if event.SchemaVersion != eventStoreSchemaVersion {
		t.Fatalf("event schema = %d, want current %d", event.SchemaVersion, eventStoreSchemaVersion)
	}
	if err := ValidateEventPayload(event); err != nil {
		t.Fatalf("current durable event failed strict validation: %v", err)
	}
}

type failingJournal struct{ err error }

func (j failingJournal) Append(context.Context, RunEvent) (RunEvent, error) { return RunEvent{}, j.err }
func (j failingJournal) ReadEvents(context.Context) ([]RunEvent, error)     { return nil, j.err }
func (j failingJournal) VerifyHashChain(context.Context) error              { return j.err }

func TestCoordinatorSessionMessageAppendFailureDoesNotAdvanceProjection(t *testing.T) {
	c := &Coordinator{sessionData: NewSession()}
	c.SetEventJournal(failingJournal{err: errors.New("disk unavailable")})
	c.addSessionUserMessage("do not persist this")
	if len(c.sessionData.Entries) != 0 {
		t.Fatalf("session advanced after append failure: %#v", c.sessionData.Entries)
	}
}

type recordingJournal struct{ events []RunEvent }

func (j *recordingJournal) Append(_ context.Context, event RunEvent) (RunEvent, error) {
	event.ID = "evt-test"
	event.Timestamp = "2026-01-01T00:00:00Z"
	j.events = append(j.events, event)
	return event, nil
}
func (j *recordingJournal) ReadEvents(context.Context) ([]RunEvent, error) { return j.events, nil }
func (j *recordingJournal) VerifyHashChain(context.Context) error          { return nil }

// uniqueRecordingJournal behaves like recordingJournal but assigns a distinct
// event ID per append. normalizeReplayEvents dedups by event.ID, so a journal
// that stamps every event with the same ID would collapse a multi-event batch
// to a single event on replay and hide exactly the DAG-edge loss this suite
// pins.
type uniqueRecordingJournal struct{ events []RunEvent }

func (j *uniqueRecordingJournal) Append(_ context.Context, event RunEvent) (RunEvent, error) {
	event.ID = fmt.Sprintf("evt-%d", len(j.events)+1)
	event.Timestamp = "2026-01-01T00:00:00Z"
	j.events = append(j.events, event)
	return event, nil
}
func (j *uniqueRecordingJournal) ReadEvents(context.Context) ([]RunEvent, error) {
	return j.events, nil
}
func (j *uniqueRecordingJournal) VerifyHashChain(context.Context) error { return nil }

func TestCommitTaskTransitionAppendsBeforeMutatingTodo(t *testing.T) {
	c := &Coordinator{taskTracker: NewTaskTracker()}
	item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "write report"}})[0]
	c.SetEventJournal(failingJournal{err: errors.New("sync failed")})
	if err := c.CommitTaskTransition(context.Background(), item.ID, TaskPending, TaskInProgress, "", "", nil); err == nil {
		t.Fatal("transition append failure was accepted")
	}
	if got := todoItemByID(c.taskTracker.TodoList().Items(), item.ID).Status; got != TaskPending {
		t.Fatalf("todo mutated before append: %s", got)
	}

	journal := &recordingJournal{}
	c.SetEventJournal(journal)
	if err := c.CommitTaskTransition(context.Background(), item.ID, TaskPending, TaskInProgress, "working", "", map[string]interface{}{"attempt": 1}); err != nil {
		t.Fatal(err)
	}
	if len(journal.events) != 1 || journal.events[0].Type != string(EventTaskStarted) {
		t.Fatalf("durable transition = %#v", journal.events)
	}
	if got := todoItemByID(c.taskTracker.TodoList().Items(), item.ID).Status; got != TaskInProgress {
		t.Fatalf("todo did not advance after append: %s", got)
	}
}

func TestCommitTaskTransition_FailingJournalDoesNotMutateFailureEvidenceOrCheckpoint(t *testing.T) {
	c := &Coordinator{
		taskTracker: NewTaskTracker(),
		sessionData: NewSession(),
	}
	item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "run command"}})[0]
	if err := c.taskTracker.TodoList().TryUpdateStatusAndOutput(item.ID, TaskInProgress, "in progress", "initial output"); err != nil {
		t.Fatal(err)
	}

	c.SetEventJournal(failingJournal{err: errors.New("event store write failed")})
	failureEvent := &FailureEventPayload{
		TaskID:       item.ID,
		Phase:        "execution",
		FailureClass: FailureExecution,
		Summary:      "uncommitted failure",
	}
	metadata := map[string]interface{}{
		"failure_event":  failureEvent,
		"failure_output": "uncommitted failure output",
	}

	err := c.CommitTaskTransition(context.Background(), item.ID, TaskInProgress, TaskError, "failed", "uncommitted failure output", metadata)
	if err == nil {
		t.Fatal("expected CommitTaskTransition to fail on failing journal")
	}

	current := todoItemByID(c.taskTracker.TodoList().Items(), item.ID)
	if current.Status != TaskInProgress {
		t.Fatalf("expected status to remain TaskInProgress, got %s", current.Status)
	}
	if current.Output != "initial output" {
		t.Fatalf("expected output to remain 'initial output', got %q", current.Output)
	}
	if current.FailureEvent != nil {
		t.Fatalf("expected failure event to remain nil, got %#v", current.FailureEvent)
	}
}

type failOnAttemptAgent struct {
	calls int
}

func (a *failOnAttemptAgent) Generate(ctx context.Context, call fantasy.AgentCall) (*fantasy.AgentResult, error) {
	return a.Stream(ctx, fantasy.AgentStreamCall{Prompt: call.Prompt, Messages: call.Messages})
}

func (a *failOnAttemptAgent) Stream(ctx context.Context, call fantasy.AgentStreamCall) (*fantasy.AgentResult, error) {
	a.calls++
	return &fantasy.AgentResult{
		Response: fantasy.Response{Content: fantasy.ResponseContent{
			fantasy.TextContent{Text: "first attempt produced partial output"},
		}},
	}, errors.New("worker failed: first attempt error")
}

type conditionalFailingJournal struct {
	err    error
	events []RunEvent
}

func (j *conditionalFailingJournal) Append(_ context.Context, event RunEvent) (RunEvent, error) {
	if event.Type == string(EventTaskStarted) && event.Payload != nil && strings.Contains(string(event.Payload), "retry") {
		return RunEvent{}, j.err
	}
	event.ID = "evt-test"
	event.Timestamp = "2026-01-01T00:00:00Z"
	j.events = append(j.events, event)
	return event, nil
}
func (j *conditionalFailingJournal) ReadEvents(context.Context) ([]RunEvent, error) {
	return j.events, nil
}
func (j *conditionalFailingJournal) VerifyHashChain(context.Context) error { return nil }

// nthFailingJournal records appends like recordingJournal but fails on the
// Nth append (1-based), simulating a journal that dies partway through a
// multi-event batch commit.
type nthFailingJournal struct {
	failOn int
	events []RunEvent
}

func (j *nthFailingJournal) Append(_ context.Context, event RunEvent) (RunEvent, error) {
	if len(j.events)+1 == j.failOn {
		return RunEvent{}, errors.New("journal append failed")
	}
	event.ID = "evt-test"
	event.Timestamp = "2026-01-01T00:00:00Z"
	j.events = append(j.events, event)
	return event, nil
}
func (j *nthFailingJournal) ReadEvents(context.Context) ([]RunEvent, error) { return j.events, nil }
func (j *nthFailingJournal) VerifyHashChain(context.Context) error          { return nil }

func TestExecuteTask_FailingJournalRetryDoesNotInvokeModelAndPreservesErrorStatus(t *testing.T) {
	workspace := t.TempDir()
	worker := &failOnAttemptAgent{}
	journal := &conditionalFailingJournal{err: errors.New("journal append failed")}

	c := &Coordinator{
		session: &TeamSession{
			Workspace: workspace,
			Config: agent.TeamConfig{
				Name:       "retry-test",
				Timeout:    30,
				MaxRetries: 2,
			},
			Agents: map[string]*agent.AgentDef{
				"worker": {Name: "worker", Role: "worker", Generation: agent.GenerationParams{Model: "test"}, MaxRetries: 2},
			},
		},
		sessionTime:     time.Now(),
		taskTracker:     NewTaskTracker(),
		reportStatus:    func(StatusEvent) {},
		taskResultCache: make(map[string][]cachedTaskEntry),
		executionRunID:  "run-retry",
	}
	c.workerAgentOverride = worker
	c.SetEventJournal(journal)

	items := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "flaky task", MaxRetries: 2}})
	todoID := items[0].ID

	taskDef := TaskDef{Agent: "worker", Goal: "flaky task", MaxRetries: 2, Recovery: RecoveryRetry}
	_, err := c.executeTask(context.Background(), taskDef, todoID)
	if err == nil {
		t.Fatal("expected executeTask to fail on retry journal append failure")
	}
	if !strings.Contains(err.Error(), "mark retry task started") {
		t.Fatalf("unexpected error message: %v", err)
	}

	// Model was called exactly once on attempt 1; attempt 2 was aborted before invoking model.
	if worker.calls != 1 {
		t.Fatalf("expected worker calls = 1, got %d", worker.calls)
	}

	current := todoItemByID(c.taskTracker.TodoList().Items(), todoID)
	if current.Status != TaskError {
		t.Fatalf("expected task status to remain TaskError, got %s", current.Status)
	}
}

func TestCommitTaskResetForRetry_FailingJournalPreservesTodoListAndCheckpoint(t *testing.T) {
	c := &Coordinator{
		taskTracker: NewTaskTracker(),
		sessionData: NewSession(),
	}
	item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "task to reset"}})[0]
	if err := c.taskTracker.TodoList().TryUpdateStatusAndOutput(item.ID, TaskError, "failed attempt 1", "error output"); err != nil {
		t.Fatal(err)
	}

	c.SetEventJournal(failingJournal{err: errors.New("event store reset append failed")})
	err := c.CommitTaskResetForRetry(context.Background(), item.ID, "resumed after interruption")
	if err == nil {
		t.Fatal("expected CommitTaskResetForRetry to fail on failing journal")
	}

	current := todoItemByID(c.taskTracker.TodoList().Items(), item.ID)
	if current.Status != TaskError {
		t.Fatalf("expected status to remain TaskError, got %s", current.Status)
	}
	if current.Output != "error output" {
		t.Fatalf("expected output to remain 'error output', got %q", current.Output)
	}
	if current.Retries != 0 {
		t.Fatalf("expected Retries to remain 0, got %d", current.Retries)
	}
}

func TestDAGScheduler_CacheHitFailingJournalDoesNotAdvanceCheckpointOrReleaseDependents(t *testing.T) {
	workspace := t.TempDir()
	c := &Coordinator{
		session: &TeamSession{
			Workspace: workspace,
			Config: agent.TeamConfig{
				Name: "cache-hit-test",
			},
			Agents: map[string]*agent.AgentDef{
				"worker": {Name: "worker", Role: "worker"},
			},
		},
		sessionTime:     time.Now(),
		taskTracker:     NewTaskTracker(),
		reportStatus:    func(StatusEvent) {},
		taskResultCache: make(map[string][]cachedTaskEntry),
	}
	c.workerAgentOverride = &failOnAttemptAgent{}
	c.storeTaskCacheWithTypedVerification("worker", "cached task", nil, "", "", "cached result")
	c.SetEventJournal(failingJournal{err: errors.New("event store write failed on cache hit")})

	tasks := []TaskDef{
		{Agent: "worker", Goal: "cached task"},
		{Agent: "worker", Goal: "dependent task", DependsOn: []int{0}},
	}
	items := c.taskTracker.TodoList().AddBatch([]TodoSpec{
		{Agent: "worker", Desc: "cached task"},
		{Agent: "worker", Desc: "dependent task"},
	})

	s := newDAGScheduler(c, tasks, items, nil)
	_, _ = s.run(context.Background())

	cachedItem := todoItemByID(c.taskTracker.TodoList().Items(), items[0].ID)
	if cachedItem.Status == TaskDone {
		t.Fatalf("cached item was marked TaskDone despite event store append failure: status=%s", cachedItem.Status)
	}
	dependentItem := todoItemByID(c.taskTracker.TodoList().Items(), items[1].ID)
	if dependentItem.Status != TaskBlocked && dependentItem.Status != TaskPending {
		t.Fatalf("dependent item should not be executed: status=%s", dependentItem.Status)
	}
}

func TestDAGScheduler_ResetWaveFailingJournalDoesNotResetStateOrLaunchWorker(t *testing.T) {
	workspace := t.TempDir()
	worker := &failOnAttemptAgent{}
	journal := &conditionalFailingJournal{err: errors.New("reset append failed")}

	c := &Coordinator{
		session: &TeamSession{
			Workspace: workspace,
			Config: agent.TeamConfig{
				Name: "dag-reset-test",
			},
			Agents: map[string]*agent.AgentDef{
				"worker": {Name: "worker", Role: "worker"},
			},
		},
		sessionTime:     time.Now(),
		taskTracker:     NewTaskTracker(),
		reportStatus:    func(StatusEvent) {},
		taskResultCache: make(map[string][]cachedTaskEntry),
	}
	c.workerAgentOverride = worker
	c.SetEventJournal(journal)

	task0 := 0
	tasks := []TaskDef{
		{Agent: "worker", Goal: "task 1"},
		{Agent: "worker", Goal: "task 2", OnFailure: &task0, MaxRetries: 1},
	}
	items := c.taskTracker.TodoList().AddBatch([]TodoSpec{
		{Agent: "worker", Desc: "task 1"},
		{Agent: "worker", Desc: "task 2", MaxRetries: 1},
	})

	s := newDAGScheduler(c, tasks, items, nil)
	s.states[0] = TaskDone
	s.states[1] = TaskError
	_ = c.taskTracker.TodoList().TryUpdateStatusAndOutput(items[0].ID, TaskDone, "done 1", "out 1")
	_ = c.taskTracker.TodoList().TryUpdateStatusAndOutput(items[1].ID, TaskError, "error 2", "err 2")

	// Failing journal rejects reset append
	c.SetEventJournal(failingJournal{err: errors.New("reset append failed")})
	err := s.resetWave(context.Background(), 0)
	if err == nil {
		t.Fatal("expected resetWave to return error on failing journal")
	}

	item0 := todoItemByID(c.taskTracker.TodoList().Items(), items[0].ID)
	if item0.Status != TaskDone {
		t.Fatalf("expected item 0 status to remain TaskDone, got %s", item0.Status)
	}
	if item0.Output != "out 1" {
		t.Fatalf("expected item 0 output to remain 'out 1', got %q", item0.Output)
	}
	if item0.Retries != 0 {
		t.Fatalf("expected item 0 Retries to remain 0, got %d", item0.Retries)
	}
}

func TestResumeInterruptedTasks_FailingJournalAbortDoesNotResetOrLaunch(t *testing.T) {
	workspace := t.TempDir()
	worker := &failOnAttemptAgent{}
	c := &Coordinator{
		session: &TeamSession{
			Workspace: workspace,
			Config: agent.TeamConfig{
				Name: "resume-fail-test",
			},
			Agents: map[string]*agent.AgentDef{
				"worker": {Name: "worker", Role: "worker"},
			},
		},
		sessionTime:     time.Now(),
		taskTracker:     NewTaskTracker(),
		reportStatus:    func(StatusEvent) {},
		taskResultCache: make(map[string][]cachedTaskEntry),
	}
	c.workerAgentOverride = worker
	c.SetEventJournal(failingJournal{err: errors.New("event store write failed on resume reset")})

	item := c.taskTracker.TodoList().AddBatch([]TodoSpec{
		{Agent: "worker", Desc: "interrupted task", Recovery: RecoveryRetry},
	})[0]
	_ = c.taskTracker.TodoList().TryUpdateStatusAndOutput(item.ID, TaskInProgress, "interrupted detail", "interrupted output")

	count, err := c.ResumeInterruptedTasks(context.Background())
	if err == nil {
		t.Fatal("expected ResumeInterruptedTasks to return error on failing journal")
	}
	if count != 0 {
		t.Fatalf("expected count = 0, got %d", count)
	}

	current := todoItemByID(c.taskTracker.TodoList().Items(), item.ID)
	if current.Status != TaskInProgress {
		t.Fatalf("expected status to remain TaskInProgress, got %s", current.Status)
	}
	if current.Output != "interrupted output" {
		t.Fatalf("expected output to remain 'interrupted output', got %q", current.Output)
	}
	if worker.calls != 0 {
		t.Fatalf("worker was dispatched despite journal append failure: calls=%d", worker.calls)
	}
}

func TestReduceToTodoList_TaskResetForRetry(t *testing.T) {
	events := []RunEvent{
		{
			Type:    string(EventTaskCreated),
			TaskID:  "1",
			Actor:   "worker",
			Payload: []byte(`{"id":"1","status":"pending","desc":"do work"}`),
		},
		{
			Type:    string(EventTaskStarted),
			TaskID:  "1",
			Actor:   "worker",
			Payload: []byte(`{"id":"1","status":"in_progress"}`),
		},
		{
			Type:    string(EventTaskFailed),
			TaskID:  "1",
			Actor:   "worker",
			Payload: []byte(`{"id":"1","status":"error","output":"failed on attempt 1"}`),
		},
		{
			Type:    string(EventTaskCreated),
			TaskID:  "1",
			Actor:   "worker",
			Payload: []byte(`{"id":"1","status":"pending","reset_for_retry":true,"retries":1}`),
		},
	}
	items := ReduceToTodoList(events)
	if len(items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(items))
	}
	if items[0].Status != TaskPending {
		t.Fatalf("expected status TaskPending, got %s", items[0].Status)
	}
	if items[0].Output != "" {
		t.Fatalf("expected output cleared, got %q", items[0].Output)
	}
	if items[0].Retries != 1 {
		t.Fatalf("expected retries 1, got %d", items[0].Retries)
	}
}

func TestCommitTaskCreation_FailingJournalDoesNotAdvanceProjectionOrCheckpoint(t *testing.T) {
	c := &Coordinator{
		taskTracker: NewTaskTracker(),
		sessionData: NewSession(),
	}
	checkpointCalls := 0
	c.taskTracker.TodoList().onChange = func() { checkpointCalls++ }

	c.SetEventJournal(failingJournal{err: errors.New("event store write failed")})
	_, err := c.CommitTaskCreation(context.Background(), []TodoSpec{{Agent: "worker", Desc: "do not persist"}})
	if err == nil {
		t.Fatal("expected CommitTaskCreation to fail on failing journal")
	}
	if len(c.taskTracker.TodoList().Items()) != 0 {
		t.Fatalf("task advanced after append failure: %#v", c.taskTracker.TodoList().Items())
	}
	if checkpointCalls != 0 {
		t.Fatalf("checkpoint advanced after append failure: %d calls", checkpointCalls)
	}

	// A healthy journal appends task_created before the projection advances.
	journal := &recordingJournal{}
	c.SetEventJournal(journal)
	items, err := c.CommitTaskCreation(context.Background(), []TodoSpec{{Agent: "worker", Desc: "persist me"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || len(journal.events) != 1 || journal.events[0].Type != string(EventTaskCreated) {
		t.Fatalf("durable creation = %#v events=%#v", items, journal.events)
	}
	if len(c.taskTracker.TodoList().Items()) != 1 {
		t.Fatalf("task did not advance after append: %#v", c.taskTracker.TodoList().Items())
	}
	if checkpointCalls != 1 {
		t.Fatalf("checkpoint did not advance after successful append: %d calls", checkpointCalls)
	}
}

func TestCommitTaskRemoval_AppendsBeforeDeletingAndReplayRemoves(t *testing.T) {
	c := &Coordinator{taskTracker: NewTaskTracker()}
	items := c.taskTracker.TodoList().AddBatch([]TodoSpec{
		{Agent: "worker", Desc: "keep"},
		{Agent: "worker", Desc: "suppressed duplicate"},
	})
	journal := &recordingJournal{}
	c.SetEventJournal(journal)
	if err := c.CommitTaskRemoval(context.Background(), items[1].ID); err != nil {
		t.Fatal(err)
	}
	if len(journal.events) != 1 || journal.events[0].Type != string(EventTaskRemoved) {
		t.Fatalf("durable removal = %#v", journal.events)
	}
	if len(c.taskTracker.TodoList().Items()) != 1 {
		t.Fatalf("removed item still in projection: %#v", c.taskTracker.TodoList().Items())
	}

	// Replay: task_created for both, then task_removed for the duplicate.
	events := []RunEvent{
		{Type: string(EventTaskCreated), TaskID: items[0].ID, Actor: "worker", Payload: []byte(`{"id":"` + items[0].ID + `","status":"pending","desc":"keep"}`)},
		{Type: string(EventTaskCreated), TaskID: items[1].ID, Actor: "worker", Payload: []byte(`{"id":"` + items[1].ID + `","status":"pending","desc":"suppressed duplicate"}`)},
		{Type: string(EventTaskRemoved), TaskID: items[1].ID, Actor: "worker", Payload: []byte(`{"id":"` + items[1].ID + `","status":"pending"}`)},
	}
	replayed := ReduceToTodoList(events)
	if len(replayed) != 1 || replayed[0].ID != items[0].ID {
		t.Fatalf("replay restored suppressed duplicate: %#v", replayed)
	}
}

func TestCommitTaskRemoval_FailingJournalDoesNotDelete(t *testing.T) {
	c := &Coordinator{taskTracker: NewTaskTracker()}
	items := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "keep me"}})
	c.SetEventJournal(failingJournal{err: errors.New("event store write failed")})
	if err := c.CommitTaskRemoval(context.Background(), items[0].ID); err == nil {
		t.Fatal("expected CommitTaskRemoval to fail on failing journal")
	}
	if len(c.taskTracker.TodoList().Items()) != 1 {
		t.Fatalf("item deleted despite append failure: %#v", c.taskTracker.TodoList().Items())
	}
}

func TestCommitTaskResolution_AppendsBeforeMutatingAndReplayRestores(t *testing.T) {
	c := &Coordinator{taskTracker: NewTaskTracker()}
	item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "failed task"}})[0]
	if err := c.taskTracker.TodoList().TryUpdateStatusAndOutput(item.ID, TaskError, "failed", "error output"); err != nil {
		t.Fatal(err)
	}
	resolver := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "resolver"}})[0]
	if err := c.taskTracker.TodoList().TryUpdateStatusAndOutput(resolver.ID, TaskDone, "done", "out"); err != nil {
		t.Fatal(err)
	}
	if err := c.taskTracker.TodoList().SetVerificationResult(resolver.ID, &VerificationResult{ExitCode: 0}); err != nil {
		t.Fatal(err)
	}

	res := &TaskResolution{Status: "reconciled", ResolvedBy: resolver.ID, Reason: "fixed by resolver"}
	journal := &recordingJournal{}
	c.SetEventJournal(journal)
	if err := c.CommitTaskResolution(context.Background(), item.ID, res); err != nil {
		t.Fatal(err)
	}
	if len(journal.events) != 1 || journal.events[0].Type != string(EventTaskResolution) {
		t.Fatalf("durable resolution = %#v", journal.events)
	}
	current := todoItemByID(c.taskTracker.TodoList().Items(), item.ID)
	if current.Resolution == nil || current.Resolution.Status != "reconciled" {
		t.Fatalf("resolution not applied: %#v", current.Resolution)
	}

	// Replay: task_created, task_failed, then task_resolution restores it.
	events := []RunEvent{
		{Type: string(EventTaskCreated), TaskID: item.ID, Actor: "worker", Payload: []byte(`{"id":"` + item.ID + `","status":"pending","desc":"failed task"}`)},
		{Type: string(EventTaskFailed), TaskID: item.ID, Actor: "worker", Payload: []byte(`{"id":"` + item.ID + `","status":"error","output":"error output"}`)},
		{Type: string(EventTaskResolution), TaskID: item.ID, Actor: "worker", Payload: []byte(`{"id":"` + item.ID + `","status":"error","resolution":{"status":"reconciled","resolved_by":"` + resolver.ID + `","reason":"fixed by resolver"}}`)},
	}
	replayed := ReduceToTodoList(events)
	if len(replayed) != 1 {
		t.Fatalf("replay = %#v", replayed)
	}
	if replayed[0].Resolution == nil || replayed[0].Resolution.Status != "reconciled" {
		t.Fatalf("resolution lost on replay: %#v", replayed[0].Resolution)
	}
}

func TestCommitTaskResolution_FailingJournalDoesNotMutate(t *testing.T) {
	c := &Coordinator{taskTracker: NewTaskTracker()}
	item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "failed task"}})[0]
	_ = c.taskTracker.TodoList().TryUpdateStatusAndOutput(item.ID, TaskError, "failed", "error output")
	resolver := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "resolver"}})[0]
	_ = c.taskTracker.TodoList().TryUpdateStatusAndOutput(resolver.ID, TaskDone, "done", "out")
	_ = c.taskTracker.TodoList().SetVerificationResult(resolver.ID, &VerificationResult{ExitCode: 0})

	c.SetEventJournal(failingJournal{err: errors.New("event store write failed")})
	res := &TaskResolution{Status: "reconciled", ResolvedBy: resolver.ID, Reason: "fixed"}
	if err := c.CommitTaskResolution(context.Background(), item.ID, res); err == nil {
		t.Fatal("expected CommitTaskResolution to fail on failing journal")
	}
	current := todoItemByID(c.taskTracker.TodoList().Items(), item.ID)
	if current.Resolution != nil {
		t.Fatalf("resolution mutated despite append failure: %#v", current.Resolution)
	}
}

func TestCommitTaskCreationResolved_AppendsDAGEdgesAndReplayRestores(t *testing.T) {
	c := &Coordinator{taskTracker: NewTaskTracker()}
	journal := &uniqueRecordingJournal{}
	c.SetEventJournal(journal)

	// Reserve IDs first (as ExecuteTasks does) so index-based DAG/recovery
	// edges can be resolved to real todo IDs before the durable payload is
	// serialized.
	ids := c.taskTracker.TodoList().ReserveIDs(3)
	specs := []TodoSpec{
		{Agent: "worker", Desc: "task 0"},
		{Agent: "worker", Desc: "task 1", DependsOn: []string{ids[0]}},
		{Agent: "worker", Desc: "task 2", OnFailure: ids[0]},
	}
	items, err := c.CommitTaskCreationResolved(context.Background(), specs, ids)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 3 {
		t.Fatalf("created %d items, want 3", len(items))
	}
	if len(journal.events) != 3 {
		t.Fatalf("durable events = %d, want 3", len(journal.events))
	}
	// The initial task_created payload must carry the complete execution
	// contract (PR-05): a dependent task must not be replayed as independent
	// pending work, and an on_failure loop must not be lost.
	for i, ev := range journal.events {
		if ev.Type != string(EventTaskCreated) {
			t.Fatalf("event %d type = %s, want task_created", i, ev.Type)
		}
		var payload map[string]interface{}
		if err := json.Unmarshal(ev.Payload, &payload); err != nil {
			t.Fatalf("event %d payload: %v", i, err)
		}
		if i == 1 {
			deps, _ := payload["depends_on"].([]interface{})
			if len(deps) != 1 || deps[0] != ids[0] {
				t.Fatalf("task 1 depends_on = %#v, want [%s]", deps, ids[0])
			}
		}
		if i == 2 {
			if onFailure, _ := payload["on_failure"].(string); onFailure != ids[0] {
				t.Fatalf("task 2 on_failure = %q, want %s", onFailure, ids[0])
			}
		}
	}

	// Crash/restart replay: reconstruct the projection from the durable events
	// alone and prove both edges survive.
	replayed := ReduceToTodoList(journal.events)
	if len(replayed) != 3 {
		t.Fatalf("replay = %d items, want 3", len(replayed))
	}
	byID := make(map[string]*TodoItem, len(replayed))
	for _, item := range replayed {
		byID[item.ID] = item
	}
	if got := byID[ids[1]].DependsOn; len(got) != 1 || got[0] != ids[0] {
		t.Fatalf("replayed task 1 depends_on = %#v, want [%s]", got, ids[0])
	}
	if got := byID[ids[2]].OnFailure; got != ids[0] {
		t.Fatalf("replayed task 2 on_failure = %q, want %s", got, ids[0])
	}
}

func TestCommitTaskCreation_PartialAppendFailureLeavesCommittedTasksVisible(t *testing.T) {
	c := &Coordinator{taskTracker: NewTaskTracker(), sessionData: NewSession()}
	checkpointCalls := 0
	c.taskTracker.TodoList().onChange = func() { checkpointCalls++ }

	// Fail on the second append: the first task_created is durable and must be
	// visible in the projection, never an orphan that replay resurrects later.
	journal := &nthFailingJournal{failOn: 2}
	c.SetEventJournal(journal)
	items, err := c.CommitTaskCreation(context.Background(), []TodoSpec{
		{Agent: "worker", Desc: "committed"},
		{Agent: "worker", Desc: "never committed"},
	})
	if err == nil {
		t.Fatal("expected CommitTaskCreation to fail on the second append")
	}
	if len(items) != 1 || items[0].Desc != "committed" {
		t.Fatalf("partial result = %#v, want the first task only", items)
	}
	// The committed task is durable AND visible: no divergence between the
	// event log and the live projection.
	if len(journal.events) != 1 || journal.events[0].Type != string(EventTaskCreated) {
		t.Fatalf("durable events = %#v, want one task_created", journal.events)
	}
	if got := len(c.taskTracker.TodoList().Items()); got != 1 {
		t.Fatalf("projection = %d items, want 1 (committed task visible)", got)
	}
	if checkpointCalls != 1 {
		t.Fatalf("checkpoint calls = %d, want 1 (committed task checkpointed)", checkpointCalls)
	}

	// Restart replay: the committed task is reconstructed, and the uncommitted
	// task never appears.
	replayed := ReduceToTodoList(journal.events)
	if len(replayed) != 1 || replayed[0].Desc != "committed" {
		t.Fatalf("replay = %#v, want only the committed task", replayed)
	}
}

func TestExecuteTasks_AppendsDAGEdgesInTaskCreatedPayload(t *testing.T) {
	workspace := t.TempDir()
	journal := &uniqueRecordingJournal{}
	c := &Coordinator{
		session: &TeamSession{
			Workspace: workspace,
			Config:    agent.TeamConfig{Name: "dag-test", MaxRounds: 5},
			Agents: map[string]*agent.AgentDef{
				"build": {Name: "build", Role: "worker"},
				"test":  {Name: "test", Role: "worker"},
			},
		},
		projectDir:      workspace,
		taskTracker:     NewTaskTracker(),
		reportStatus:    func(StatusEvent) {},
		delegatedTasks:  make(map[string]int),
		taskResultCache: make(map[string][]cachedTaskEntry),
	}
	c.SetEventJournal(journal)
	// The DAG scheduler dispatches each task to a worker. Submit a completed
	// result for whichever todo is currently in progress so both tasks finish
	// without a real model call.
	c.workerAgentOverride = &submittingWorkerAgent{onSubmit: func() {
		for _, item := range c.taskTracker.TodoList().Items() {
			if item.Status == TaskInProgress {
				c.storeSubmittedTaskResult(item.ID, &TaskResult{
					TaskID: item.ID, Agent: item.Agent, Status: TaskResultStatusSuccess, Source: "submitted",
					Summary: "done",
				})
			}
		}
	}}

	// task 1 depends on task 0; task 1's failure loops back to task 0.
	_, err := c.ExecuteTasks(context.Background(), []TaskDef{
		{Agent: "build", Goal: "compile"},
		{Agent: "test", Goal: "verify", DependsOn: []int{0}, OnFailure: intPtr(0)},
	})
	if err != nil {
		t.Fatalf("ExecuteTasks: %v", err)
	}
	// The scheduler appends started/completed transitions after creation, so
	// isolate the durable task_created events that must carry the edges.
	var createdEvents []RunEvent
	for _, ev := range journal.events {
		if ev.Type == string(EventTaskCreated) {
			createdEvents = append(createdEvents, ev)
		}
	}
	if len(createdEvents) != 2 {
		t.Fatalf("task_created events = %d, want 2", len(createdEvents))
	}
	items := c.taskTracker.TodoList().Items()
	if len(items) != 2 {
		t.Fatalf("projection = %d items, want 2", len(items))
	}
	// The live projection carries the resolved edges.
	if got := items[1].DependsOn; len(got) != 1 || got[0] != items[0].ID {
		t.Fatalf("task 1 depends_on = %#v, want [%s]", got, items[0].ID)
	}
	if got := items[1].OnFailure; got != items[0].ID {
		t.Fatalf("task 1 on_failure = %q, want %s", got, items[0].ID)
	}
	// The durable payload carries the same edges (no post-creation mutation).
	var payload map[string]interface{}
	if err := json.Unmarshal(createdEvents[1].Payload, &payload); err != nil {
		t.Fatal(err)
	}
	deps, _ := payload["depends_on"].([]interface{})
	if len(deps) != 1 || deps[0] != items[0].ID {
		t.Fatalf("durable task 1 depends_on = %#v, want [%s]", deps, items[0].ID)
	}
	if onFailure, _ := payload["on_failure"].(string); onFailure != items[0].ID {
		t.Fatalf("durable task 1 on_failure = %q, want %s", onFailure, items[0].ID)
	}
	// Replay from the durable events alone restores both edges.
	replayed := ReduceToTodoList(journal.events)
	if len(replayed) != 2 {
		t.Fatalf("replay = %d items, want 2", len(replayed))
	}
	if got := replayed[1].DependsOn; len(got) != 1 || got[0] != replayed[0].ID {
		t.Fatalf("replayed task 1 depends_on = %#v, want [%s]", got, replayed[0].ID)
	}
	if got := replayed[1].OnFailure; got != replayed[0].ID {
		t.Fatalf("replayed task 1 on_failure = %q, want %s", got, replayed[0].ID)
	}
}
