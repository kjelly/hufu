package team

import (
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
