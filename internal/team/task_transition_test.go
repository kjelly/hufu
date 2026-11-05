package team

import (
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
)

func TestPendingMetadataDoesNotRecreateCanonicalTask(t *testing.T) {
	for _, restart := range []bool{false, true} {
		name := "live"
		if restart {
			name = "rehydrated"
		}
		t.Run(name, func(t *testing.T) {
			c := newTestCoordinator(t)
			// This fixture exercises canonical creation/checkpoint persistence,
			// without a model or execution-policy admission to bypass.
			c.executionRunID = ""
			store, err := NewEventStore(c.session.Workspace, "checkpoint-run", "checkpoint-session")
			if err != nil {
				t.Fatal(err)
			}
			c.eventStore = store
			items, err := c.CommitTaskCreation(t.Context(), []TodoSpec{{Agent: "worker", Desc: "preflight failure"}})
			if err != nil {
				t.Fatal(err)
			}
			id := items[0].ID
			if restart {
				c.emittedTaskTransitions = nil
				c.hydrateEmittedEventKeys()
			}
			fingerprint := NewFailureFingerprint("", "worker", "preflight", FailurePolicy, "fixture denied")
			if err := c.taskTracker.TodoList().AppendFailureFingerprint(id, fingerprint); err != nil {
				t.Fatal(err)
			}
			if err := c.emitTaskEventsFromCheckpoint(c.taskTracker.TodoList().Items()); err != nil {
				t.Fatal(err)
			}
			if err := c.CommitTaskTransition(t.Context(), id, TaskPending, TaskBlocked, "fixture denied", "", nil); err != nil {
				t.Fatal(err)
			}
			events, err := store.ReadEvents()
			if err != nil {
				t.Fatal(err)
			}
			created := 0
			for _, event := range events {
				if event.Type == string(EventTaskCreated) && event.TaskID == id {
					created++
				}
			}
			item := todoItemByID(ReduceToTodoList(events), id)
			if created != 1 || item == nil || item.Status != TaskBlocked || len(item.FailureFingerprints) != 1 || item.FailureFingerprints[0].Digest != fingerprint.Digest {
				t.Fatal("pending metadata recreated its occurrence or lost failure evidence")
			}
		})
	}
}

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

// Recovery must issue a fresh lease even when it is a process's first dispatch.
func TestDispatchIDsDoNotRepeatAcrossProcesses(t *testing.T) {
	const childKey = "HUFU_DISPATCH_ID_CHILD"
	if os.Getenv(childKey) == "1" {
		for range 2 {
			if _, err := os.Stdout.WriteString(newTaskDispatchID() + "\n"); err != nil {
				t.Fatal(err)
			}
		}
		return
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	seen := make(map[string]bool)
	for range 2 {
		cmd := exec.CommandContext(t.Context(), executable, "-test.run=^TestDispatchIDsDoNotRepeatAcrossProcesses$")
		cmd.Env = append(os.Environ(), childKey+"=1")
		output, err := cmd.Output()
		if err != nil {
			t.Fatalf("dispatch subprocess: %v", err)
		}
		remaining := string(output)
		for range 2 {
			var id string
			id, remaining, _ = strings.Cut(remaining, "\n")
			if !strings.HasPrefix(id, "dispatch-") || seen[id] {
				t.Fatalf("dispatch lease repeated across processes or is invalid: %q", id)
			}
			seen[id] = true
		}
	}
}
