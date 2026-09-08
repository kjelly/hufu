package team

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestFailureClassAllowedHelpers pins the small, generic gate helpers used by
// dagScheduler.handleEvent before exercising the full scheduler behavior
// below: an empty/unknown class is always allowed (no captured evidence must
// never silently strand a task), and membership is otherwise exact.
func TestFailureClassAllowedHelpers(t *testing.T) {
	if !failureClassAllowed("", []TaskFailureClass{FailureVerify}) {
		t.Fatal("an empty/unknown class must be allowed so missing evidence never silently blocks remediation")
	}
	if !failureClassAllowed(FailureVerify, []TaskFailureClass{FailureVerify, FailureTimeout}) {
		t.Fatal("expected FailureVerify to be allowed by an allowlist containing it")
	}
	if failureClassAllowed(FailureExecution, []TaskFailureClass{FailureVerify}) {
		t.Fatal("expected FailureExecution to be rejected by an allowlist that only contains FailureVerify")
	}
	if got := failureClassForTodo(nil); got != "" {
		t.Fatalf("failureClassForTodo(nil) = %q, want empty", got)
	}
	item := &TodoItem{FailureEvent: &FailureEventPayload{FailureClass: FailureTimeout}}
	if got := failureClassForTodo(item); got != FailureTimeout {
		t.Fatalf("failureClassForTodo() = %q, want %q", got, FailureTimeout)
	}
}

// dagSchedulerOnFailureClassesFixture builds a two-task coder->verifier batch
// (verifier depends on coder and resets it via on_failure) with the verifier
// task's terminal FailureEvent pre-seeded to the given class, mirroring the
// hufu-coding team's verifier/reviewer/final-sa contracts. It mirrors the
// construction pattern used by the existing
// TestDAGSchedulerRoutesSuccessfulNoProgressWithBoundedBudget.
func dagSchedulerOnFailureClassesFixture(t *testing.T, onFailureClasses []TaskFailureClass, terminalClass TaskFailureClass) (*dagScheduler, []*TodoItem) {
	t.Helper()
	coord := &Coordinator{
		taskTracker:     NewTaskTracker(),
		reportStatus:    func(StatusEvent) {},
		taskResultCache: make(map[string][]cachedTaskEntry),
		maxConcurrent:   1,
	}
	tasks := []TaskDef{
		{Agent: "coder"},
		{Agent: "verifier", DependsOn: []int{0}, OnFailure: intPtr(0), MaxRetries: 2, OnFailureClasses: onFailureClasses},
	}
	items := coord.taskTracker.TodoList().AddBatch([]TodoSpec{
		{Agent: "coder", Desc: "implement"},
		{Agent: "verifier", Desc: "verify"},
	})
	coord.taskTracker.TodoList().UpdateStatus(items[0].ID, TaskInProgress, "running")
	coord.taskTracker.TodoList().UpdateStatus(items[0].ID, TaskDone, "done")
	coord.taskTracker.TodoList().UpdateStatus(items[1].ID, TaskInProgress, "running")
	// Production always persists a terminal status together with the
	// classified FailureEvent (PersistFailureWithClass*) before the error
	// ever reaches the scheduler, so terminalizeTaskErrorIfUnresolved's
	// "still in flight" guard (coordinator_failure.go) skips reclassifying
	// it from the bare error text. Match that here: seed the terminal status
	// first, then the FailureEvent, so the pre-seeded class survives.
	coord.taskTracker.TodoList().UpdateStatus(items[1].ID, TaskError, "terminal failure")
	items[1].FailureEvent = &FailureEventPayload{FailureClass: terminalClass}

	s := newDAGScheduler(coord, tasks, items, nil)
	s.states[0], s.states[1], s.inProgress = TaskDone, TaskInProgress, 1
	return s, items
}

// drainRelaunchedTask waits for the goroutine that launchReady starts (as a
// side effect of resetting a task to Pending) to report back on eventCh, so
// the test does not leak it. The context passed to handleEvent must already
// be cancelled so the relaunched task terminates immediately without
// attempting any real work through the mostly-empty fixture Coordinator.
func drainRelaunchedTask(t *testing.T, s *dagScheduler) {
	t.Helper()
	select {
	case <-s.eventCh:
	case <-time.After(time.Second):
		t.Fatal("relaunched task did not terminate after canceled context")
	}
}

// TestDAGSchedulerOnFailureClassesSuppressesNonSemanticReset is the central
// spec.md §10.2 regression: an infrastructure/provider-class failure on a
// task whose contract restricts on_failure to `verification` must retry that
// task in place and must never reset the on_failure ancestor (the coder).
func TestDAGSchedulerOnFailureClassesSuppressesNonSemanticReset(t *testing.T) {
	s, items := dagSchedulerOnFailureClassesFixture(t, []TaskFailureClass{FailureVerify}, FailureExecution)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	s.handleEvent(ctx, agentTaskResult{idx: 1, agentName: "verifier", todoID: items[1].ID, err: errors.New("app-server disconnected")})

	// The ancestor must be completely untouched: still Done, never even
	// transiently reset. launchReady synchronously flips a newly-ready
	// task's state to InProgress before this call returns (it does not wait
	// for the relaunched goroutine), so the verifier itself — reset and
	// immediately relaunched by the self-heal path — is observed InProgress,
	// not Pending.
	if s.states[0] != TaskDone {
		t.Fatalf("infra-class failure incorrectly reset the coder ancestor: states=%v", s.states)
	}
	if s.states[1] != TaskInProgress {
		t.Fatalf("expected the verifier itself to be reset and relaunched in place, got %s", s.states[1])
	}
	if s.retries[1] != 1 {
		t.Fatalf("expected the in-place self-heal retry to consume the task's own retry budget, got retries=%v", s.retries)
	}
	drainRelaunchedTask(t, s)
}

// TestDAGSchedulerOnFailureClassesAllowsSemanticReset proves the same gate
// still allows the on_failure edge for the one class it authorizes.
func TestDAGSchedulerOnFailureClassesAllowsSemanticReset(t *testing.T) {
	s, items := dagSchedulerOnFailureClassesFixture(t, []TaskFailureClass{FailureVerify}, FailureVerify)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	s.handleEvent(ctx, agentTaskResult{idx: 1, agentName: "verifier", todoID: items[1].ID, err: errors.New("required check failed")})

	// resetWave resets the ancestor and launchReady relaunches it
	// synchronously within this same call, so the observed state is
	// InProgress (see the comment in the suppression test above), not Done.
	if s.states[0] != TaskInProgress {
		t.Fatalf("expected a genuine verification-class rejection to reset the coder ancestor, got %s", s.states[0])
	}
	if s.retries[1] != 1 {
		t.Fatalf("expected the on_failure edge to consume the task's retry budget, got retries=%v", s.retries)
	}
	drainRelaunchedTask(t, s)
}

// TestDAGSchedulerOnFailureClassesOmittedPreservesLegacyBehavior pins
// backward compatibility (spec.md §21): a task that does not configure
// OnFailureClasses must reset its on_failure ancestor for any terminal
// failure class, exactly as before this field existed.
func TestDAGSchedulerOnFailureClassesOmittedPreservesLegacyBehavior(t *testing.T) {
	s, items := dagSchedulerOnFailureClassesFixture(t, nil, FailureExecution)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	s.handleEvent(ctx, agentTaskResult{idx: 1, agentName: "verifier", todoID: items[1].ID, err: errors.New("app-server disconnected")})

	if s.states[0] != TaskInProgress {
		t.Fatalf("legacy team (no on-failure-classes) must still reset the ancestor on any failure class, got %s", s.states[0])
	}
	drainRelaunchedTask(t, s)
}
