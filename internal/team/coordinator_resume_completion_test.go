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

func TestRunStopsAfterRecoveryRequiresOperator(t *testing.T) {
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
