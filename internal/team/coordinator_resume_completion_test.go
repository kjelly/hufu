package team

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"charm.land/fantasy"
	"github.com/kjelly/hufu/internal/agent"
)

func TestResumePreservesWatchdogCancellation(t *testing.T) {
	t.Parallel()
	c := newTestCoordinator(t)
	session := NewSession()
	session.Tasks = []*TodoItem{
		{ID: "1", Agent: "worker", Desc: "completed source", Status: TaskDone, Output: "accepted evidence"},
		{ID: "2", Agent: "worker", Desc: "pending dependent", Status: TaskPending, Recovery: RecoveryRetry},
	}
	c.SetSessionData(session)
	ctx, cancel := context.WithCancelCause(t.Context())
	cancel(ErrInvocationStalled)
	_, stopped, err := c.resumeInvocationTasks(ctx, "resume", false)
	if !stopped || !errors.Is(err, ErrInvocationStalled) {
		t.Fatalf("resume stopped=%t with %v, want stall cause", stopped, err)
	}
	if result := c.LastRunResult(); result == nil || result.StopReason != StopReasonInvocationStalled || result.GoalSatisfied {
		t.Fatalf("resume lost watchdog outcome: %+v", result)
	}
	if source := todoByID(t, c, "1"); source.Status != TaskDone || source.Output != "accepted evidence" {
		t.Fatalf("resume altered completed evidence: %+v", source)
	}
}

func TestResumeDoesNotLaunchTaskWithUnacceptedDependency(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name       string
		evidence   bool
		wantStatus TaskStatus
	}{
		{name: "declared dependency", wantStatus: TaskBlocked},
		{name: "evidence source", evidence: true, wantStatus: TaskPending},
	} {
		t.Run(test.name, func(t *testing.T) {
			c, producer, _, _ := resultCollectionFixture(t)
			spec := TodoSpec{Agent: producer.Agent, Desc: "audit the accepted result", Model: producer.Model, ExecutionTarget: producer.ExecutionTarget,
				ExecutionTopology: producer.ExecutionTopology, Recovery: RecoveryRetry, SideEffect: SideEffectNone}
			if test.evidence {
				spec.EvidenceFrom = []string{producer.ID}
			} else {
				spec.DependsOn = []string{producer.ID}
			}
			ids := c.taskTracker.TodoList().ReserveIDs(1)
			projection, err := taskOccurrenceProjectionFromSpec(spec, ids[0])
			if err != nil {
				t.Fatal(err)
			}
			if _, err := c.admitTaskOccurrence(t.Context(), projection, ids[0], 1); err != nil {
				t.Fatal(err)
			}
			items, err := c.CommitTaskCreationResolved(t.Context(), []TodoSpec{spec}, ids)
			if err != nil {
				t.Fatal(err)
			}
			c.PersistFailureWithClassAndStatus(producer.Agent, producer.Desc, producer.ID, "producer failed", NeedsHuman, FailureProtocol, TaskBlocked)
			worker := scriptWorker(textTurn("must wait for the producer"))
			c.workerAgentOverride = worker
			resumed, err := c.ResumeInterruptedTasks(t.Context())
			status := c.todoItemByID(items[0].ID).Status
			if resumed != 0 || worker.Calls() != 0 || status != test.wantStatus || (err != nil) != (test.wantStatus == TaskBlocked) {
				t.Fatalf("resume with unaccepted source: resumed=%d calls=%d status=%s, want %s, err=%v", resumed, worker.Calls(), status, test.wantStatus, err)
			}
		})
	}
}

func TestRunStopsAfterRecoveryRequiresOperator(t *testing.T) {
	t.Parallel()
	c := newTestCoordinator(t)
	c.session.Agents["coordinator"] = &agent.AgentDef{Name: "coordinator", Role: "coordinator"}
	session := NewSession()
	session.Tasks = []*TodoItem{
		{ID: "1", Agent: "worker", Desc: "completed research", Status: TaskDone, Output: "retained research evidence"},
		{ID: "2", Agent: "worker", Desc: "manual recovery", Status: TaskInProgress, Recovery: RecoveryManual},
	}
	c.SetSessionData(session)
	c.initEventStore()
	coordinatorCalls := 0
	c.runOrchestratorOverride = func(context.Context, *agent.AgentDef, string) (string, []fantasy.StepResult, error) {
		coordinatorCalls++
		return "must not generate after needs_human", nil, nil
	}
	response, err := c.Run(t.Context(), "resume")
	if !errors.Is(err, ErrTasksUnresolved) || coordinatorCalls != 0 || !strings.Contains(response, "retained research evidence") {
		t.Fatalf("operator-blocked recovery reached coordinator: calls=%d response=%q error=%v", coordinatorCalls, response, err)
	}
	if item := todoByID(t, c, "1"); item.Status != TaskDone || item.Output != "retained research evidence" {
		t.Fatal("recovery changed completed research")
	}
	store, err := OpenEventStoreReadOnly(c.session.Workspace)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	events, err := store.ReadEvents()
	if err != nil {
		t.Fatal(err)
	}
	finished := 0
	for _, event := range events {
		if event.Type == string(EventRunFinished) {
			finished++
		}
	}
	projected := ReduceToSessionData(events)
	if finished != 1 || projected.RunResult == nil || projected.RunResult.GoalSatisfied || len(projected.RunResult.UnresolvedTasks) != 1 || projected.RunResult.UnresolvedTasks[0].RetryDisposition != NeedsHuman {
		t.Fatal("operator stop did not persist one canonical unresolved result")
	}
}

func TestOperatorBlockedResumeCompletion(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"needs_human", "retry", "replan", "reconcile", "pending", "done", "unclassified", "cancelled"} {
		t.Run(mode, func(t *testing.T) {
			c := newTestCoordinator(t)
			c.sessionData = NewSession()
			store, err := NewEventStore(c.session.Workspace, c.executionRunID, "test")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = store.Close() })
			store.SetBranchID("main")
			c.eventStore = store
			c.SetEventJournal(eventStoreJournal{store: store})
			completed := addTodo(t, c, "completed research")
			c.taskTracker.TodoList().UpdateStatusAndOutput(completed, TaskDone, "", "retained accepted research")
			id := addTodo(t, c, "result-only repair")
			disposition := NeedsHuman
			switch mode {
			case "retry":
				disposition = RetryWorker
			case "replan":
				disposition = ReplanRequired
			case "reconcile":
				disposition = ReconcileOnly
			case "unclassified":
				disposition = ""
			}
			if err := c.taskTracker.TodoList().SetFailureEventAndOutput(id, &FailureEventPayload{TaskID: id, FailureClass: FailureProtocol, RetryDisposition: disposition}, "original observations"); err != nil {
				t.Fatal(err)
			}
			status := TaskBlocked
			if mode == "pending" {
				status = TaskPending
			} else if mode == "done" {
				status = TaskDone
			}
			c.taskTracker.TodoList().UpdateStatus(id, status, "")
			before := c.taskTracker.TodoList().Items()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if mode == "cancelled" {
				cancel()
			}
			response, stopped := c.finalizeOperatorBlockedResume(ctx, "resume existing work", false)
			if stopped != (mode == "needs_human") {
				t.Fatalf("completion guard stopped=%t for %s", stopped, mode)
			}
			if !stopped {
				if c.LastRunResult() != nil {
					t.Fatal("retryable work acquired a terminal result")
				}
				return
			}
			result := c.LastRunResult()
			if result == nil || IsRunOutcomeSuccess(result.Outcome) || result.ExitCode == 0 || len(result.UnresolvedTasks) != 1 || result.UnresolvedTasks[0].RetryDisposition != NeedsHuman || result.Acceptance != nil || !strings.Contains(response, "retained accepted research") {
				t.Fatalf("operator stop lost its unresolved result or completed evidence: %#v", result)
			}
			if !reflect.DeepEqual(before, c.taskTracker.TodoList().Items()) {
				t.Fatal("terminal resume changed worker task state")
			}
		})
	}
}
