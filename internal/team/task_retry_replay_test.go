package team

import (
	"encoding/json/v2"
	"testing"
)

func TestWorkerRetryLifecycleMatchesReplay(t *testing.T) {
	c := newTestCoordinator(t)
	c.executionRunID = ""
	store, err := NewEventStore(c.session.Workspace, "retry-run", "retry-session")
	if err != nil {
		t.Fatal(err)
	}
	c.eventStore = store
	items, err := c.CommitTaskCreation(t.Context(), []TodoSpec{{Agent: "worker", Desc: "retry a failed worker"}})
	if err != nil {
		t.Fatal(err)
	}
	id := items[0].ID
	for _, step := range []struct {
		status  TaskStatus
		attempt int
	}{{TaskInProgress, 1}, {TaskError, 0}, {TaskInProgress, 2}} {
		var metadata map[string]any
		if step.attempt > 0 {
			metadata = attemptStartMetadata(step.attempt)
		}
		if err := c.commitTaskTransitionFromCurrent(t.Context(), id, step.status, "", "", metadata); err != nil {
			t.Fatal(err)
		}
		events, err := store.ReadEvents()
		if err != nil {
			t.Fatal(err)
		}
		live := todoItemByID(c.TaskTracker().TodoList().Items(), id)
		replayed := todoItemByID(ReduceToTodoList(events), id)
		if replayed == nil || live.Status != replayed.Status || live.OccurrenceRevision != replayed.OccurrenceRevision || live.DispatchID != replayed.DispatchID {
			t.Fatalf("lifecycle diverged after %s: live=%s/%d/%s replay=%+v", step.status, live.Status, live.OccurrenceRevision, live.DispatchID, replayed)
		}
		if step.attempt > 0 {
			c.setCurrentTaskAttempt(id, step.attempt)
			active, ok := c.activeTaskResultOccurrence(id)
			if !ok || active.OccurrenceRevision != live.OccurrenceRevision || active.Attempt != step.attempt {
				t.Fatal("attempt activation replaced the committed revision")
			}
		}
	}
}

func TestTaskTransitionKeySeparatesOccurrenceLeases(t *testing.T) {
	item := &TodoItem{ID: "task", Status: TaskInProgress, OccurrenceRevision: 1, DispatchID: "old"}
	first := taskTransitionEventKey(item)
	item.OccurrenceRevision++
	second := taskTransitionEventKey(item)
	item.DispatchID = "new"
	third := taskTransitionEventKey(item)
	if first == second || second == third || first == third || third != taskTransitionEventKey(item) {
		t.Fatal("transition idempotency did not bind the exact occurrence lease")
	}
}

func TestReplayRetryStartRequiresNewAttemptLease(t *testing.T) {
	for _, test := range []struct {
		name     string
		prior    TaskStatus
		status   TaskStatus
		attempt  int
		revision int
		dispatch string
		want     bool
	}{
		{"retry", TaskError, TaskInProgress, 2, 4, "", true},
		{"late_checkpoint", TaskError, TaskInProgress, 0, 4, "", false},
		{"first_attempt", TaskError, TaskInProgress, 1, 4, "", false},
		{"stale_revision", TaskError, TaskInProgress, 2, 3, "", false},
		{"stale_dispatch", TaskError, TaskInProgress, 2, 4, "old", false},
		{"wrong_status", TaskError, TaskDone, 2, 4, "", false},
		{"completed", TaskDone, TaskInProgress, 2, 4, "", false},
		{"skipped", TaskSkipped, TaskInProgress, 2, 4, "", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			payload, err := json.Marshal(map[string]any{"status": test.status, "dispatch_attempt": test.attempt})
			if err != nil {
				t.Fatal(err)
			}
			event := RunEvent{Type: string(EventTaskStarted), Payload: payload}
			item := &TodoItem{Status: test.prior, OccurrenceRevision: 3}
			if got := isReplayRetryStart(item, event, string(test.status), test.revision, test.dispatch); got != test.want {
				t.Fatalf("retry start = %v, want %v", got, test.want)
			}
		})
	}
}
