package team

import (
	"context"
	"sync"
	"testing"
	"time"
)

type actionProgressTransitionJournal struct {
	EventJournal
	beforeAppend func()
}

type actionProgressTestTransition struct {
	name   string
	status TaskStatus
	commit func(*Coordinator, string) error
}

func actionProgressTestTransitions(t *testing.T) []actionProgressTestTransition {
	t.Helper()
	var transitions []actionProgressTestTransition
	for _, next := range []TaskStatus{TaskDone, TaskError, TaskVerifying} {
		transitions = append(transitions, actionProgressTestTransition{
			name: string(next), status: next,
			commit: func(c *Coordinator, id string) error {
				var metadata map[string]interface{}
				if next == TaskError {
					metadata = map[string]interface{}{"failure_event": c.failureEventForItem(c.todoItemByID(id), FailureExecution, RetryNone, "transition", FailureFingerprint{}, id)}
				}
				return c.commitTaskTransitionFromCurrent(t.Context(), id, next, "transition", "", metadata)
			},
		})
	}
	return append(transitions,
		actionProgressTestTransition{name: "retry", status: TaskPending, commit: func(c *Coordinator, id string) error {
			return c.CommitTaskResetForRetry(t.Context(), id, "retry")
		}},
		actionProgressTestTransition{name: "resume", status: TaskPending, commit: func(c *Coordinator, id string) error {
			return c.prepareInterruptedTaskForResume(t.Context(), id, "resume")
		}},
	)
}

func (j actionProgressTransitionJournal) Append(ctx context.Context, event RunEvent) (RunEvent, error) {
	j.beforeAppend()
	return j.EventJournal.Append(ctx, event)
}

func newActionProgressTransitionCoordinator(t *testing.T) (*Coordinator, TaskDef, *TodoItem, *EventStore) {
	t.Helper()
	session := workflowTestSession(t)
	events, err := NewEventStore(session.Workspace, "run-progress-transition", "session-progress-transition")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = events.Close() })
	c := &Coordinator{session: session, taskTracker: NewTaskTracker(), eventStore: events, executionRunID: "run-progress-transition", phaseWorkflow: executePhaseWorkflow(t, session), projectDir: session.Workspace}
	c.SetEventJournal(eventStoreJournal{store: events})
	task, item := createAdmittedTestTask(t, c, TaskDef{ID: "execute", Agent: "executor", Goal: "apply", Phase: PhaseExecute, Action: &Action{Capability: "structured-actions", Type: "apply"}})
	if err := c.commitTaskTransitionFromCurrent(t.Context(), item.ID, TaskInProgress, "executing", "", attemptStartMetadata(1)); err != nil {
		t.Fatal(err)
	}
	c.setCurrentTaskAttempt(item.ID, 1)
	return c, task, item, events
}

func assertActionProgressTransitionReplay(t *testing.T, c *Coordinator, events *EventStore) {
	t.Helper()
	stored, err := events.ReadEvents()
	if err != nil {
		t.Fatal(err)
	}
	live := c.taskTracker.TodoList().Items()
	replayed := ReduceToTodoList(stored)
	if err := compareTaskProjection(live, replayed); err != nil {
		t.Fatalf("progress/transition canonical parity: %v", err)
	}
	if !isProjectionPrefixOrRecoverable(&SessionData{Tasks: live}, ReduceToSessionData(stored), replayed) {
		t.Fatal("progress/transition mismatch would require recovery on resume")
	}
	if err := events.VerifyHashChain(); err != nil {
		t.Fatal(err)
	}
}

func TestActionProgressWaitsForTaskTransition(t *testing.T) {
	for _, transition := range actionProgressTestTransitions(t) {
		t.Run(transition.name, func(t *testing.T) {
			c, task, item, events := newActionProgressTransitionCoordinator(t)
			todoID := item.ID
			var callbacks sync.WaitGroup
			var once sync.Once
			c.SetEventJournal(actionProgressTransitionJournal{EventJournal: eventStoreJournal{store: events}, beforeAppend: func() {
				// The transition has captured its snapshot. A concurrent
				// observation must wait until that snapshot is published.
				once.Do(func() {
					started := make(chan struct{})
					finished := make(chan struct{})
					callbacks.Go(func() {
						close(started)
						c.recordActionProgress(task, todoID, "action-1", 1, ActionProgress{Version: 1, Kind: "stage", Stage: "deploy", Message: "late observation"})
						close(finished)
					})
					<-started
					select {
					case <-finished:
						t.Error("progress callback escaped a pending lifecycle snapshot")
					case <-time.After(100 * time.Millisecond):
					}
				})
			}})
			if err := transition.commit(c, todoID); err != nil {
				t.Fatal(err)
			}
			callbacks.Wait()
			if got := c.todoItemByID(todoID); got.Status != transition.status || got.ActionProgress != nil {
				t.Fatalf("late progress changed the completed transition: %#v", got)
			}
			assertActionProgressTransitionReplay(t, c, events)
		})
	}
}

func TestTaskTransitionWaitsForActionProgress(t *testing.T) {
	for _, change := range actionProgressTestTransitions(t) {
		t.Run(change.name, func(t *testing.T) {
			c, task, item, events := newActionProgressTransitionCoordinator(t)
			todoID := item.ID
			originalSync := events.syncFile
			var once sync.Once
			transition := make(chan error, 1)
			snapshotPrepared := make(chan struct{})
			var snapshotOnce sync.Once
			c.SetEventJournal(actionProgressTransitionJournal{EventJournal: eventStoreJournal{store: events}, beforeAppend: func() {
				snapshotOnce.Do(func() { close(snapshotPrepared) })
			}})
			events.syncFile = func() error {
				once.Do(func() {
					// Progress is durable before its live projection is
					// installed. A lifecycle snapshot must wait for both.
					started := make(chan struct{})
					go func() {
						close(started)
						transition <- change.commit(c, todoID)
					}()
					<-started
					select {
					case <-snapshotPrepared:
						t.Error("lifecycle snapshot captured before progress was published")
					case <-time.After(100 * time.Millisecond):
					}
				})
				return originalSync()
			}
			c.recordActionProgress(task, todoID, "action-1", 1, ActionProgress{Version: 1, Kind: "stage", Stage: "deploy", Message: "observed"})
			if err := <-transition; err != nil {
				t.Fatal(err)
			}
			if got := c.todoItemByID(todoID); got.Status != change.status || got.ActionProgress == nil || got.ActionProgress.Stage != "deploy" {
				t.Fatalf("transition lost the preceding progress: %#v", got)
			}
			assertActionProgressTransitionReplay(t, c, events)
		})
	}
}
