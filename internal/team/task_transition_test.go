package team

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"
)

func TestConcurrentCurrentTaskTerminalTransitions(t *testing.T) {
	c := &Coordinator{taskTracker: NewTaskTracker()}
	item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "cancel concurrently"}})[0]
	// AddBatch returns a live item; projection commits replace that object.
	// Goroutines must share the immutable ID value, not read the live pointer.
	todoID := item.ID
	if err := c.CommitTaskTransition(t.Context(), todoID, TaskPending, TaskInProgress, "", "", nil); err != nil {
		t.Fatal(err)
	}
	const callers = 20
	errors := make(chan error, callers)
	var workers sync.WaitGroup
	c.taskProjectionMu.Lock()
	for range callers {
		workers.Go(func() {
			errors <- c.commitTaskTransitionFromCurrent(t.Context(), todoID, TaskError, "cancelled", "", nil)
		})
	}
	c.taskProjectionMu.Unlock()
	workers.Wait()
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatalf("concurrent cancellation failed: %v", err)
		}
	}
	if err := c.CommitTaskTransition(t.Context(), todoID, TaskInProgress, TaskDone, "", "", nil); err == nil || !strings.Contains(err.Error(), "expected in_progress, got error") {
		t.Fatalf("explicit stale transition was not rejected: %v", err)
	}
	if err := c.commitTaskTransitionFromCurrent(t.Context(), todoID, TaskDone, "", "", nil); err == nil {
		t.Fatal("current-state selection bypassed the terminal state machine")
	}
}

// Only in_progress commits that begin an attempt carry dispatch_attempt; a
// same-status re-commit also emits task_started but does not start an
// attempt.
func TestTaskStartedMarksOnlyAttemptStarts(t *testing.T) {
	c := &Coordinator{taskTracker: NewTaskTracker()}
	item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "attempt starts"}})[0]
	journal := &uniqueRecordingJournal{}
	c.SetEventJournal(journal)
	if err := c.CommitTaskTransition(t.Context(), item.ID, TaskPending, TaskInProgress, "", "", attemptStartMetadata(1)); err != nil {
		t.Fatal(err)
	}
	if err := c.commitTaskTransitionFromCurrent(t.Context(), item.ID, TaskInProgress, "resumed after child", "", nil); err != nil {
		t.Fatal(err)
	}
	if err := c.commitTaskTransitionFromCurrent(t.Context(), item.ID, TaskInProgress, "attempt 2/2", "", attemptStartMetadata(2)); err != nil {
		t.Fatal(err)
	}
	want := []any{float64(1), nil, float64(2)}
	if len(journal.events) != len(want) {
		t.Fatalf("events = %d, want %d", len(journal.events), len(want))
	}
	for i, event := range journal.events {
		if event.Type != string(EventTaskStarted) {
			t.Fatalf("event %d type = %q, want task_started", i, event.Type)
		}
		var payload map[string]any
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			t.Fatal(err)
		}
		if got := payload[taskStartedDispatchAttemptKey]; got != want[i] {
			t.Fatalf("event %d dispatch_attempt = %v, want %v", i, got, want[i])
		}
	}
}

// Lifecycle payloads carry the occurrence attempt (Retries+1), not the
// in-dispatch attempt counter that receipts use.
func TestLifecycleAttemptIsOccurrenceAttempt(t *testing.T) {
	for retries, want := range map[int]int{0: 1, 2: 3} {
		item := &TodoItem{ID: "1", Agent: "worker", Retries: retries, DispatchID: "dispatch-7"}
		if got := taskTransitionPayloadWithCoordinator(item, nil)["attempt"]; got != want {
			t.Fatalf("retries=%d: payload attempt = %v, want %d", retries, got, want)
		}
	}
	c := &Coordinator{taskTracker: NewTaskTracker()}
	item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "attempt"}})[0]
	if got := c.taskAttempt(item.ID); got != 1 {
		t.Fatalf("taskAttempt for a fresh occurrence = %d, want 1", got)
	}
}

// The DispatchID is a process-local counter, so it cannot be a durable
// identity input for world IDs or attempt keys.
func TestDispatchIDIsProcessLocal(t *testing.T) {
	first, second := newTaskDispatchID(), newTaskDispatchID()
	if first == second || !strings.HasPrefix(first, "dispatch-") || !strings.HasPrefix(second, "dispatch-") {
		t.Fatalf("dispatch IDs = %q, %q, want distinct process-local counters", first, second)
	}
}
