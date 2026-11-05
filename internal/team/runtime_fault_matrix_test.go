package team

import (
	"context"
	"encoding/json/v2"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/kjelly/hufu/internal/agent"
)

func TestRuntimeFaultMatrixCancellationDrainsBufferedCompletionWithoutLaunchingDependent(t *testing.T) {
	workspace := t.TempDir()
	c := &Coordinator{
		session: &TeamSession{
			Workspace: workspace,
			Config:    agent.TeamConfig{Name: "fault-matrix-cancellation"},
			Agents: map[string]*agent.AgentDef{
				"worker": {Name: "worker", Role: "worker", Generation: agent.GenerationParams{Model: "test"}},
			},
		},
		taskTracker:   NewTaskTracker(),
		reportStatus:  func(StatusEvent) {},
		maxConcurrent: 1,
		taskCache:     newDefaultTaskCache(taskCacheDependencies{}),
	}
	c.SetSessionData(NewSession())
	c.initEventStore()
	if c.eventStore == nil {
		t.Fatal("event store was not initialized")
	}
	t.Cleanup(func() { _ = c.eventStore.Close() })

	ids := c.taskTracker.TodoList().ReserveIDs(2)
	items, err := c.CommitTaskCreationResolved(t.Context(), []TodoSpec{
		{Agent: "worker", Desc: "completed root", Goal: "completed root"},
		{Agent: "worker", Desc: "dependent must not start", Goal: "dependent must not start", DependsOn: []string{ids[0]}},
	}, ids)
	if err != nil {
		t.Fatalf("CommitTaskCreationResolved: %v", err)
	}
	if err := c.commitTaskTransitionFromCurrent(t.Context(), ids[0], TaskInProgress, "worker started", "", nil); err != nil {
		t.Fatalf("commit root start: %v", err)
	}

	tasks := []TaskDef{
		{Agent: "worker", Goal: "completed root"},
		{Agent: "worker", Goal: "dependent must not start", DependsOn: []int{0}},
	}
	var calls int
	c.workerAgentOverride = &countingEmptyAgent{calls: &calls}
	scheduler := mustNewDAGScheduler(t, c, tasks, items, nil)
	scheduler.states[0] = TaskInProgress
	scheduler.inProgress = 1

	if err := c.commitTaskTransitionFromCurrent(t.Context(), ids[0], TaskDone, "worker completed", "root output", nil); err != nil {
		t.Fatalf("commit root completion: %v", err)
	}
	scheduler.eventCh <- agentTaskResult{agentName: "worker", todoID: ids[0], idx: 0, output: "root output"}

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := scheduler.run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled scheduler error = %v, want context.Canceled", err)
	}
	if calls != 0 {
		t.Fatalf("cancelled scheduler launched dependent worker %d time(s)", calls)
	}
	if scheduler.inProgress != 0 || scheduler.states[1] != TaskPending {
		t.Fatalf("cancelled scheduler state = states=%v in_progress=%d, want dependent pending and no in-flight work", scheduler.states, scheduler.inProgress)
	}

	checkpoint, exists, err := LoadSessionReadOnly(workspace)
	if err != nil || !exists {
		t.Fatalf("LoadSessionReadOnly = (%#v, %t, %v), want durable checkpoint", checkpoint, exists, err)
	}
	assertFaultMatrixTaskStatuses(t, checkpoint.Tasks, ids[0], TaskDone, ids[1], TaskPending)

	events, err := c.eventStore.ReadEvents()
	if err != nil {
		t.Fatalf("ReadEvents: %v", err)
	}
	if got := countFaultMatrixTaskEvent(events, ids[0], EventTaskCompleted); got != 1 {
		t.Fatalf("root task_completed events = %d, want exactly 1", got)
	}
	if got := countFaultMatrixTaskEvent(events, ids[1], EventTaskStarted); got != 0 {
		t.Fatalf("dependent task_started events = %d, want 0", got)
	}
	for _, terminal := range []EventType{EventTaskCompleted, EventTaskFailed, EventTaskBlocked, EventTaskSkipped, EventTaskCancelled} {
		if got := countFaultMatrixTaskEvent(events, ids[1], terminal); got != 0 {
			t.Fatalf("dependent %s events = %d, want 0", terminal, got)
		}
	}
	assertFaultMatrixTaskStatuses(t, ReduceToTodoList(events), ids[0], TaskDone, ids[1], TaskPending)
}

func TestRuntimeFaultMatrixRetryReopensSameOccurrenceAndPersistsProjections(t *testing.T) {
	worker := scriptFailThenSucceed()
	c := newTestCoordinator(t, withWorker(worker), withMaxRetries(2))
	c.executionRunID = "run-fault-matrix-retry"
	c.SetSessionData(NewSession())
	c.initEventStore()
	if c.eventStore == nil {
		t.Fatal("event store was not initialized")
	}
	c.initTaskJournal()
	c.taskCache = newDefaultTaskCache(taskCacheDependenciesFor(c))

	ids := c.taskTracker.TodoList().ReserveIDs(1)
	spec := TodoSpec{
		Agent:      "worker",
		Desc:       "retryable task",
		Goal:       "retryable task",
		Model:      "test",
		SideEffect: SideEffectNone,
		Recovery:   RecoveryRetry,
		MaxRetries: 2,
	}
	projection, err := taskOccurrenceProjectionFromSpec(spec, ids[0])
	if err != nil {
		t.Fatalf("taskOccurrenceProjectionFromSpec: %v", err)
	}
	if _, err := c.admitTaskOccurrence(t.Context(), projection, ids[0], 1); err != nil {
		t.Fatalf("admit initial occurrence: %v", err)
	}
	items, err := c.CommitTaskCreationResolved(t.Context(), []TodoSpec{spec}, ids)
	if err != nil {
		t.Fatalf("CommitTaskCreationResolved: %v", err)
	}

	scheduler := mustNewDAGScheduler(t, c, []TaskDef{taskDefFromTodoItem(items[0])}, items, nil)
	results, err := scheduler.run(t.Context())
	if err != nil {
		t.Fatalf("scheduler retry: %v", err)
	}
	out := results[0].output
	if out != "succeeded on retry" || worker.Calls() != 2 {
		t.Fatalf("retry result = %q after %d calls, want success after 2 calls", out, worker.Calls())
	}
	current := todoItemByID(c.taskTracker.TodoList().Items(), ids[0])
	// start(1) -> failure(2) -> retry start(3) -> done(4). Activating
	// attempt two must retain its committed revision instead of resetting it.
	const terminalRevision = 4
	if current == nil || current.Status != TaskDone || current.OccurrenceRevision != terminalRevision {
		t.Fatalf("retried occurrence = %#v, want same ID at revision %d and done", current, terminalRevision)
	}
	if len(current.ContextManifests) != 2 || current.ContextManifests[0].Attempt != 1 || current.ContextManifests[1].Attempt != 2 {
		t.Fatalf("retry context manifests = %#v, want attempts 1 and 2", current.ContextManifests)
	}
	if len(current.ExecutionReceipts) < 2 {
		t.Fatalf("retry execution receipts = %#v, want both attempts", current.ExecutionReceipts)
	}

	checkpoint, exists, err := LoadSessionReadOnly(c.session.Workspace)
	if err != nil || !exists {
		t.Fatalf("LoadSessionReadOnly = (%#v, %t, %v), want durable checkpoint", checkpoint, exists, err)
	}
	checkpointed := todoItemByID(checkpoint.Tasks, ids[0])
	if checkpointed == nil || checkpointed.Status != TaskDone || checkpointed.OccurrenceRevision != terminalRevision {
		t.Fatalf("checkpointed retry occurrence = %#v", checkpointed)
	}

	events, err := c.eventStore.ReadEvents()
	if err != nil {
		t.Fatalf("ReadEvents: %v", err)
	}
	if got := countFaultMatrixTaskEvent(events, ids[0], EventTaskCreated); got != 1 {
		t.Fatalf("task_created events = %d, want one stable Todo occurrence across attempts", got)
	}
	if got := countFaultMatrixTaskEvent(events, ids[0], EventTaskStarted); got < 2 {
		t.Fatalf("task_started events = %d, want durable evidence for both attempts", got)
	}
	if got := countFaultMatrixTaskEvent(events, ids[0], EventTaskCompleted); got != 1 {
		t.Fatalf("task_completed events = %d, want exactly 1", got)
	}
	var attempts []int
	lastRevision := 0
	for _, event := range events {
		if event.TaskID != ids[0] {
			continue
		}
		if attempt := eventDispatchAttempt(event); attempt > 0 {
			attempts = append(attempts, attempt)
		}
		var payload struct {
			Revision int `json:"occurrence_revision"`
		}
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			t.Fatal(err)
		}
		if payload.Revision > 0 {
			if payload.Revision < lastRevision || payload.Revision > lastRevision+1 {
				t.Fatalf("durable occurrence revision changed from %d to %d", lastRevision, payload.Revision)
			}
			lastRevision = payload.Revision
		}
	}
	if !slices.Equal(attempts, []int{1, 2}) || lastRevision != terminalRevision {
		t.Fatalf("durable retry identity: attempts=%v revision=%d", attempts, lastRevision)
	}
	replayed := todoItemByID(ReduceToTodoList(events), ids[0])
	if replayed == nil || replayed.Status != TaskDone || replayed.OccurrenceRevision != terminalRevision {
		t.Fatalf("event-replayed retry occurrence = %#v", replayed)
	}

	if c.journal == nil {
		t.Fatal("task journal was not initialized")
	}
	// Finish background writers before explicitly reopening the task journal.
	c.drainAsyncTasks()
	if err := c.journal.Close(); err != nil {
		t.Fatalf("close task journal: %v", err)
	}
	c.journal = nil
	journalEntries, err := loadTaskJournal(c.session.Workspace, time.Now(), 0, 10)
	if err != nil {
		t.Fatalf("loadTaskJournal: %v", err)
	}
	if got := journalEntries["worker"]; len(got) != 1 || got[0].output != "succeeded on retry" {
		t.Fatalf("task journal worker entries = %#v, want one successful terminal result", got)
	}
}

func countFaultMatrixTaskEvent(events []RunEvent, taskID string, eventType EventType) int {
	count := 0
	for _, event := range events {
		if event.TaskID == taskID && event.Type == string(eventType) {
			count++
		}
	}
	return count
}

func assertFaultMatrixTaskStatuses(t *testing.T, items []*TodoItem, firstID string, firstStatus TaskStatus, secondID string, secondStatus TaskStatus) {
	t.Helper()
	first := todoItemByID(items, firstID)
	second := todoItemByID(items, secondID)
	if first == nil || first.Status != firstStatus || second == nil || second.Status != secondStatus {
		t.Fatalf("task statuses = first %#v second %#v, want %s and %s", first, second, firstStatus, secondStatus)
	}
}
