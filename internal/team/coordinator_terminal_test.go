package team

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestTerminalHandoffPausesAndResumesTaskRound(t *testing.T) {
	tracker := NewTaskTracker()
	item := tracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "interactive task"}})[0]
	tracker.TodoList().UpdateStatus(item.ID, TaskInProgress, "")
	coord := &Coordinator{session: &TeamSession{}, taskTracker: tracker, reportStatus: func(StatusEvent) {}}

	cancelled := make(chan struct{})
	coord.registerTerminalRound(item.ID, func() { close(cancelled) })
	session := TerminalSession{OwnerTaskID: item.ID}
	coord.pauseTerminalTask(session)
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("takeover did not cancel active model round")
	}
	if got := tracker.TodoList().Items()[0].Status; got != TaskPaused {
		t.Fatalf("status after takeover = %s, want %s", got, TaskPaused)
	}

	resumed := make(chan bool, 1)
	go func() { resumed <- coord.waitForTerminalResume(context.Background(), item.ID) }()
	select {
	case <-resumed:
		t.Fatal("model round resumed before terminal was released")
	case <-time.After(25 * time.Millisecond):
	}

	coord.resumeTerminalTask(session)
	select {
	case ok := <-resumed:
		if !ok {
			t.Fatal("waitForTerminalResume returned false")
		}
	case <-time.After(time.Second):
		t.Fatal("model round did not resume after terminal release")
	}
	if got := tracker.TodoList().Items()[0].Status; got != TaskInProgress {
		t.Fatalf("status after release = %s, want %s", got, TaskInProgress)
	}
}

func TestCoordinatorFinalizeTaskTerminalResourcesContainsLeakAndPreservesTaskError(t *testing.T) {
	manager, err := NewTerminalSessionManager(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	coord := &Coordinator{terminalSessionMgr: manager}
	manager.SetActiveTaskRoundChecker(coord.isTerminalRoundActive)
	owner := WithTerminalTaskID(context.Background(), "task-leak")
	if _, err := manager.Start(owner, TerminalStartRequest{
		RunID: "run-leak", OwnerTaskID: "task-leak", Command: []string{"sh", "-c", "sleep 30"},
	}); err != nil {
		t.Fatal(err)
	}

	original := errors.New("worker failed after starting terminal")
	got, blocked := coord.finalizeTaskTerminalResources(context.Background(), "task-leak", original)
	if blocked {
		t.Fatalf("contained terminal unexpectedly blocked task: %v", got)
	}
	if !errors.Is(got, original) {
		t.Fatalf("finalization error %v did not preserve original %v", got, original)
	}
	if err := manager.RequireTaskClosed("task-leak"); err != nil {
		t.Fatalf("contained terminal remained a retry gate: %v", err)
	}
	sessions, err := manager.List(context.Background(), "run-leak")
	if err != nil || len(sessions) != 1 {
		t.Fatalf("list sessions = %#v, err=%v", sessions, err)
	}
	if sessions[0].CleanupState != TerminalCleanupCompleted {
		t.Fatalf("cleanup state = %q, want completed", sessions[0].CleanupState)
	}
}

func TestCoordinatorCleanupRunTerminalResourcesContainsAllSessions(t *testing.T) {
	manager, err := NewTerminalSessionManager(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	coord := &Coordinator{terminalSessionMgr: manager, executionRunID: "run-stop"}
	manager.SetActiveTaskRoundChecker(coord.isTerminalRoundActive)
	for _, taskID := range []string{"task-one", "task-two"} {
		owner := WithTerminalTaskID(context.Background(), taskID)
		if _, err := manager.Start(owner, TerminalStartRequest{
			RunID: "run-stop", OwnerTaskID: taskID, Command: []string{"sh", "-c", "sleep 30"},
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := coord.cleanupRunTerminalResources(TerminalCleanupRunCancelled); err != nil {
		t.Fatal(err)
	}
	if err := manager.RequireNoLeaks("run-stop"); err != nil {
		t.Fatalf("run cleanup left a terminal leak: %v", err)
	}
}

func TestCoordinatorCleanupRunTerminalResourcesCancelsAndWaitsForActiveRound(t *testing.T) {
	manager, err := NewTerminalSessionManager(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	tracker := NewTaskTracker()
	item := tracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "terminal task"}})[0]
	tracker.TodoList().UpdateStatus(item.ID, TaskInProgress, "")
	coord := &Coordinator{terminalSessionMgr: manager, taskTracker: tracker, executionRunID: "run-active"}
	manager.SetActiveTaskRoundChecker(coord.isTerminalRoundActive)
	owner := WithTerminalTaskID(context.Background(), item.ID)
	if _, err := manager.Start(owner, TerminalStartRequest{
		RunID: "run-active", OwnerTaskID: item.ID, Command: []string{"sh", "-c", "sleep 30"},
	}); err != nil {
		t.Fatal(err)
	}
	roundCancelled := make(chan struct{})
	coord.registerTerminalRound(item.ID, func() { close(roundCancelled) })
	go func() {
		<-roundCancelled
		coord.unregisterTerminalRound(item.ID)
	}()
	if err := coord.cleanupRunTerminalResources(TerminalCleanupRunCancelled); err != nil {
		t.Fatalf("cleanup after cancelling active round: %v", err)
	}
	if coord.isTerminalRoundActive(item.ID) {
		t.Fatal("terminal round remained registered after shutdown cancellation")
	}
	if err := manager.RequireNoLeaks("run-active"); err != nil {
		t.Fatalf("active-round shutdown left a terminal leak: %v", err)
	}
}

func TestCoordinatorCleanupRunTerminalResourcesContainsTerminalAfterRoundTimeout(t *testing.T) {
	manager, err := NewTerminalSessionManager(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	tracker := NewTaskTracker()
	item := tracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "non-cooperative terminal task"}})[0]
	tracker.TodoList().UpdateStatus(item.ID, TaskInProgress, "")
	coord := &Coordinator{
		terminalSessionMgr:           manager,
		taskTracker:                  tracker,
		executionRunID:               "run-timeout",
		terminalRoundShutdownTimeout: 25 * time.Millisecond,
	}
	manager.SetActiveTaskRoundChecker(coord.isTerminalRoundActive)
	owner := WithTerminalTaskID(context.Background(), item.ID)
	if _, err := manager.Start(owner, TerminalStartRequest{
		RunID: "run-timeout", OwnerTaskID: item.ID, Command: []string{"sh", "-c", "sleep 30"},
	}); err != nil {
		t.Fatal(err)
	}
	cancelled := make(chan struct{})
	// Deliberately never unregister: this simulates a cancelled model round
	// that fails to cooperate with shutdown before the bounded wait expires.
	coord.registerTerminalRound(item.ID, func() { close(cancelled) })
	if err := coord.cleanupRunTerminalResources(TerminalCleanupRunCancelled); err != nil {
		t.Fatalf("timeout cleanup did not contain terminal: %v", err)
	}
	select {
	case <-cancelled:
	default:
		t.Fatal("shutdown did not cancel the active terminal round")
	}
	if err := manager.RequireNoLeaks("run-timeout"); err != nil {
		t.Fatalf("non-cooperative round left terminal leak: %v", err)
	}
	if err := manager.Write(owner, managerSessionID(t, manager, "run-timeout"), TerminalInput{OwnerTaskID: item.ID, Data: []byte("late write\n")}); err == nil {
		t.Fatal("late owner terminal write succeeded after timeout cleanup custody transfer")
	}
	if got := tracker.TodoList().Items()[0].Status; got != TaskBlocked {
		t.Fatalf("timeout owner status = %s, want %s", got, TaskBlocked)
	}
	// The simulated model never returns, so clean the test-only round record
	// after containment has asserted the timeout behavior.
	coord.unregisterTerminalRound(item.ID)
}

func managerSessionID(t *testing.T, manager *TerminalSessionManager, runID string) string {
	t.Helper()
	sessions, err := manager.List(context.Background(), runID)
	if err != nil || len(sessions) != 1 {
		t.Fatalf("list terminal sessions = %#v, err=%v", sessions, err)
	}
	return sessions[0].ID
}
