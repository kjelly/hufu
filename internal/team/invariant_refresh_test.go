package team

import (
	"encoding/json"
	"testing"

	"github.com/kjelly/hufu/internal/agent"
)

func TestRefreshStaleGateVerifiersResetsWithoutRetryBudget(t *testing.T) {
	stale := invariantCompletionTodo("run-old", "gate", InvariantVerificationGate, TaskDone, []InvariantAssessment{completionAssessment("safe", InvariantSeverityError, InvariantPreserved)})
	stale.Retries = 2
	stale.ExecutionReceipt = &ExecutionReceipt{RunID: "run-old", TaskID: stale.ID, Attempt: 1}
	stale.FailureEvent = &FailureEventPayload{FailureClass: FailureExecution}
	stale.Resolution = &TaskResolution{}
	report := invariantCompletionTodo("run-old", "report", InvariantVerificationReport, TaskDone, []InvariantAssessment{completionAssessment("safe", InvariantSeverityError, InvariantViolated)})
	tracker := NewTaskTracker()
	tracker.TodoList().Restore([]*TodoItem{stale, report})
	c := &Coordinator{
		executionRunID: "run-current", taskTracker: tracker,
		session:     &TeamSession{Config: agent.TeamConfig{Name: "team"}},
		taskResults: map[string]*TaskResult{stale.ID: cloneTaskResult(stale.TypedResult)},
	}

	count, err := c.RefreshStaleGateVerifiers(t.Context())
	if err != nil || count != 1 {
		t.Fatalf("RefreshStaleGateVerifiers = %d, %v", count, err)
	}
	items := tracker.TodoList().Items()
	refreshed := todoItemByID(items, stale.ID)
	if refreshed.Status != TaskPending || refreshed.Retries != 2 || refreshed.TypedResult != nil || refreshed.ExecutionReceipt != nil || refreshed.FailureEvent != nil || refreshed.Resolution != nil {
		t.Fatalf("refreshed gate = %#v", refreshed)
	}
	if len(refreshed.ContextManifests) != 1 || refreshed.ContextManifests[0].RunID != "run-old" {
		t.Fatalf("historical manifests were not preserved: %#v", refreshed.ContextManifests)
	}
	if c.GetTaskResult(stale.ID) != nil {
		t.Fatal("process-local result latch survived stale gate reset")
	}
	if got := todoItemByID(items, report.ID); got.Status != TaskDone || got.TypedResult == nil {
		t.Fatalf("report verifier was refreshed: %#v", got)
	}
}

func TestCommitStaleGateVerifierResetIsDurableAndReplayable(t *testing.T) {
	stale := invariantCompletionTodo("run-old", "gate", InvariantVerificationGate, TaskDone, []InvariantAssessment{completionAssessment("safe", InvariantSeverityError, InvariantPreserved)})
	tracker := NewTaskTracker()
	tracker.TodoList().Restore([]*TodoItem{stale})
	store, err := NewEventStore(t.TempDir(), "run-current", "session-current")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	c := &Coordinator{executionRunID: "run-current", taskTracker: tracker, session: &TeamSession{Config: agent.TeamConfig{Name: "team"}}}
	c.SetEventJournal(eventStoreJournal{store: store})

	if err := c.CommitStaleGateVerifierReset(t.Context(), stale.ID); err != nil {
		t.Fatal(err)
	}
	events, err := store.ReadEvents()
	if err != nil || len(events) != 1 {
		t.Fatalf("events = %#v, %v", events, err)
	}
	var payload map[string]any
	if err := json.Unmarshal(events[0].Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if payload["reset_reason"] != "stale_gate_verification" || events[0].Type != string(EventTaskCreated) {
		t.Fatalf("reset event = %#v payload=%#v", events[0], payload)
	}
	replayed, err := ReplayTodoList(events)
	if err != nil || len(replayed) != 1 {
		t.Fatalf("replayed = %#v, %v", replayed, err)
	}
	if replayed[0].Status != TaskPending || replayed[0].TypedResult != nil || len(replayed[0].ContextManifests) != 1 {
		t.Fatalf("replayed stale reset = %#v", replayed[0])
	}
}

func TestRefreshStaleGateVerifiersKeepsCurrentAndInterruptedTasks(t *testing.T) {
	current := invariantCompletionTodo("run-current", "current", InvariantVerificationGate, TaskDone, []InvariantAssessment{completionAssessment("safe", InvariantSeverityError, InvariantPreserved)})
	pending := invariantCompletionTodo("run-old", "pending", InvariantVerificationGate, TaskPending, []InvariantAssessment{completionAssessment("safe", InvariantSeverityError, InvariantPreserved)})
	tracker := NewTaskTracker()
	tracker.TodoList().Restore([]*TodoItem{current, pending})
	c := &Coordinator{executionRunID: "run-current", taskTracker: tracker, session: &TeamSession{Config: agent.TeamConfig{Name: "team"}}}
	count, err := c.RefreshStaleGateVerifiers(t.Context())
	if err != nil || count != 0 {
		t.Fatalf("RefreshStaleGateVerifiers = %d, %v", count, err)
	}
	items := tracker.TodoList().Items()
	if todoItemByID(items, current.ID).Status != TaskDone || todoItemByID(items, pending.ID).TypedResult == nil {
		t.Fatalf("current/interrupted tasks changed: %#v", items)
	}
}

func TestRuntimeAttestedInvariantRejectionCanonicalizesFailureClass(t *testing.T) {
	item := invariantCompletionTodo("run-current", "gate", InvariantVerificationGate, TaskError, []InvariantAssessment{completionAssessment("safe", InvariantSeverityError, InvariantViolated)})
	item.FailureEvent = &FailureEventPayload{FailureClass: FailureExecution}
	if got := effectiveFailureClassForTodo(item); got != FailureSemanticRejection {
		t.Fatalf("effective failure class = %q", got)
	}
	item.InvariantVerification = InvariantVerificationReport
	if got := effectiveFailureClassForTodo(item); got == FailureSemanticRejection {
		t.Fatalf("report-mode violation canonicalized as semantic rejection")
	}
}

func TestInvariantVerifierTasksBypassDuplicateSuppression(t *testing.T) {
	c := &Coordinator{taskTracker: NewTaskTracker(), delegatedTasks: map[string]int{}}
	tasks := []TaskDef{
		{Agent: "reviewer", Goal: "verify invariants", InvariantVerification: InvariantVerificationReport},
		{Agent: "reviewer", Goal: "verify invariants", InvariantVerification: InvariantVerificationGate},
	}
	warnings, duplicates, suppressed := c.checkDuplicateTasks(t.Context(), tasks)
	if len(warnings) != 0 || len(duplicates) != 0 || len(suppressed) != 0 || len(c.delegatedTasks) != 0 {
		t.Fatalf("invariant duplicate suppression result warnings=%v duplicates=%v suppressed=%v delegated=%v", warnings, duplicates, suppressed, c.delegatedTasks)
	}
}
