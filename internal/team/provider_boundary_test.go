package team

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kjelly/hufu/internal/agent"
)

func TestProviderBoundaryFailureDoesNotPoisonNextInvocation(t *testing.T) {
	firstErr := errors.New("synthetic proxy startup failure")
	startCalls := 0
	c := &Coordinator{
		providerManager: &agent.ProviderManager{},
		providerBoundaryStart: func(context.Context, string) error {
			startCalls++
			if startCalls == 1 {
				return firstErr
			}
			return nil
		},
	}

	if err := c.startProviderExecutionBoundary(context.Background()); !errors.Is(err, firstErr) {
		t.Fatalf("first boundary error = %v, want %v", err, firstErr)
	}
	if c.providerBoundaryStarted || c.providerBoundaryErr == nil {
		t.Fatalf("failed boundary state = started=%t err=%v", c.providerBoundaryStarted, c.providerBoundaryErr)
	}
	if err := c.startProviderExecutionBoundary(context.Background()); err != nil {
		t.Fatalf("second boundary start failed with stale error: %v", err)
	}
	if !c.providerBoundaryStarted || c.providerBoundaryErr != nil {
		t.Fatalf("successful boundary state = started=%t err=%v", c.providerBoundaryStarted, c.providerBoundaryErr)
	}
	if err := c.stopProviderExecutionBoundary(); err != nil {
		t.Fatalf("stop boundary: %v", err)
	}
	if c.providerBoundaryStarted || c.providerBoundaryErr != nil {
		t.Fatalf("stopped boundary state = started=%t err=%v", c.providerBoundaryStarted, c.providerBoundaryErr)
	}
}

func TestInvocationParentCancellationAbortsBoundaryExactlyOnce(t *testing.T) {
	started := make(chan struct{})
	aborted := make(chan struct{})
	var abortCalls atomic.Int32
	c := &Coordinator{
		providerManager: &agent.ProviderManager{},
		providerBoundaryStart: func(context.Context, string) error {
			close(started)
			return nil
		},
		providerBoundaryAbort: func() error {
			if abortCalls.Add(1) == 1 {
				close(aborted)
			}
			return nil
		},
		session:      &TeamSession{Workspace: t.TempDir(), Config: agent.TeamConfig{Name: "parent-cancel"}},
		sessionData:  NewSession(),
		taskTracker:  NewTaskTracker(),
		reportStatus: func(StatusEvent) {},
	}
	parent, cancel := context.WithCancel(context.Background())
	ctx, end := c.beginInvocationExecutionRun(parent)
	if err := c.startProviderExecutionBoundary(ctx); err != nil {
		t.Fatal(err)
	}
	<-started
	cancel()
	select {
	case <-aborted:
	case <-time.After(time.Second):
		t.Fatal("parent cancellation did not abort the provider boundary")
	}
	end()
	if got := abortCalls.Load(); got != 1 {
		t.Fatalf("provider abort calls = %d, want exactly one", got)
	}
}

func TestInvocationAbortGateJoinsConcurrentAbortRequests(t *testing.T) {
	abortStarted := make(chan struct{})
	releaseAbort := make(chan struct{})
	var abortCalls atomic.Int32
	c := &Coordinator{
		providerManager:       &agent.ProviderManager{},
		providerBoundaryStart: func(context.Context, string) error { return nil },
		providerBoundaryAbort: func() error {
			if abortCalls.Add(1) == 1 {
				close(abortStarted)
				<-releaseAbort
			}
			return nil
		},
		session:      &TeamSession{Workspace: t.TempDir(), Config: agent.TeamConfig{Name: "abort-race"}},
		sessionData:  NewSession(),
		taskTracker:  NewTaskTracker(),
		reportStatus: func(StatusEvent) {},
	}
	ctx, end := c.beginInvocationExecutionRun(context.Background())
	if err := c.startProviderExecutionBoundary(ctx); err != nil {
		t.Fatal(err)
	}
	owner := c.invocationWatchdog.Load().owner
	parentAbortDone := make(chan struct{})
	go func() {
		owner.abortProviderBoundary()
		close(parentAbortDone)
	}()
	select {
	case <-abortStarted:
	case <-time.After(time.Second):
		t.Fatal("first abort did not reach the provider boundary")
	}
	stallAbortDone := make(chan struct{})
	go func() {
		owner.abortProviderBoundary()
		close(stallAbortDone)
	}()
	close(releaseAbort)
	select {
	case <-parentAbortDone:
	case <-time.After(time.Second):
		t.Fatal("first abort did not join")
	}
	select {
	case <-stallAbortDone:
	case <-time.After(time.Second):
		t.Fatal("second abort did not join")
	}
	end()
	if got := abortCalls.Load(); got != 1 {
		t.Fatalf("provider abort calls = %d, want exactly one", got)
	}
}

func TestPublicInvocationLeaseSerializesProviderBoundaryOwnership(t *testing.T) {
	var startCalls atomic.Int32
	var abortCalls atomic.Int32
	abortStarted := make(chan struct{})
	releaseAbort := make(chan struct{})
	c := &Coordinator{
		providerManager: &agent.ProviderManager{},
		providerBoundaryStart: func(context.Context, string) error {
			startCalls.Add(1)
			return nil
		},
		providerBoundaryAbort: func() error {
			if abortCalls.Add(1) == 1 {
				close(abortStarted)
				<-releaseAbort
			}
			return nil
		},
	}

	firstCtx, endFirst, err := c.beginPublicInvocationExecutionRun(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := c.startProviderExecutionBoundary(firstCtx); err != nil {
		t.Fatal(err)
	}

	secondCtx, cancelSecond := context.WithCancel(context.Background())
	secondDone := make(chan error, 1)
	go func() {
		_, endSecond, err := c.beginPublicInvocationExecutionRun(secondCtx)
		if err == nil {
			endSecond()
		}
		secondDone <- err
	}()
	cancelSecond()
	select {
	case err := <-secondDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("second invocation error = %v, want context cancellation", err)
		}
	case <-time.After(time.Second):
		t.Fatal("second invocation did not stop waiting after cancellation")
	}
	if got := startCalls.Load(); got != 1 {
		t.Fatalf("provider boundary starts = %d, want one", got)
	}

	firstDone := make(chan struct{})
	go func() {
		endFirst()
		close(firstDone)
	}()
	select {
	case <-abortStarted:
	case <-time.After(time.Second):
		t.Fatal("first invocation did not abort its provider boundary")
	}
	close(releaseAbort)
	select {
	case <-firstDone:
	case <-time.After(time.Second):
		t.Fatal("first invocation cleanup did not complete")
	}
	if got := abortCalls.Load(); got != 1 {
		t.Fatalf("provider boundary aborts = %d, want one", got)
	}
}

func TestPreCancelledInvocationDoesNotStartProviderBoundary(t *testing.T) {
	var startCalls atomic.Int32
	c := &Coordinator{
		providerManager: &agent.ProviderManager{},
		providerBoundaryStart: func(context.Context, string) error {
			startCalls.Add(1)
			return nil
		},
		session:      &TeamSession{Workspace: t.TempDir(), Config: agent.TeamConfig{Name: "pre-cancel"}},
		sessionData:  NewSession(),
		taskTracker:  NewTaskTracker(),
		reportStatus: func(StatusEvent) {},
	}
	parent, cancel := context.WithCancel(context.Background())
	cancel()
	ctx, end := c.beginInvocationExecutionRun(parent)
	defer end()
	if err := c.startProviderExecutionBoundary(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("pre-cancelled startup error = %v, want context.Canceled", err)
	}
	if got := startCalls.Load(); got != 0 {
		t.Fatalf("provider startup calls = %d, want zero", got)
	}
}

func TestSidecarFailsClosedBeforeProviderBoundary(t *testing.T) {
	c := &Coordinator{providerManager: &agent.ProviderManager{}, sidecarModel: "model"}
	if got := c.Sidecar(); got != nil {
		t.Fatal("sidecar was constructed before the provider boundary was admitted")
	}
}

func TestContextPreflightAdmitsSidecarAfterProviderBoundary(t *testing.T) {
	calls := 0
	c, err := NewCoordinator(&TeamSession{Workspace: t.TempDir(), Config: agent.TeamConfig{Name: "preflight-sidecar"}}, "", "", nil, nil, nil, RoleModels{Sidecar: "gpt-4o"}, 0, false, false, false, nil, nil, nil, false, "", false, false, nil, false, false)
	if err != nil {
		t.Fatal(err)
	}
	c.providerBoundaryStart = func(context.Context, string) error {
		calls++
		return nil
	}
	defer c.CloseContextPreflight()
	if err := c.PrepareContextPreflight(); err != nil {
		t.Fatal(err)
	}
	if calls != 1 || !c.providerBoundaryStarted {
		t.Fatalf("provider boundary admission = calls:%d started:%t, want one live boundary", calls, c.providerBoundaryStarted)
	}
	if c.Sidecar() == nil {
		t.Fatal("sidecar was not available after provider boundary admission")
	}
}

func TestContextPreflightCancellationAbortsOwnedBoundaryBeforeClose(t *testing.T) {
	c, err := NewCoordinator(&TeamSession{Workspace: t.TempDir(), Config: agent.TeamConfig{Name: "preflight-cancel"}}, "", "", nil, nil, nil, RoleModels{Sidecar: "model"}, 0, false, false, false, nil, nil, nil, false, "", false, false, nil, false, false)
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	aborted := make(chan struct{})
	var abortCalls atomic.Int32
	c.providerBoundaryStart = func(ctx context.Context, _ string) error {
		close(started)
		return nil
	}
	c.providerBoundaryAbort = func() error {
		if abortCalls.Add(1) == 1 {
			close(aborted)
		}
		return nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	if err := c.PrepareContextPreflightContext(ctx); err != nil {
		t.Fatal(err)
	}
	<-started
	cancel()
	select {
	case <-aborted:
	case <-time.After(time.Second):
		t.Fatal("preflight cancellation did not synchronously request provider abort")
	}
	c.CloseContextPreflight()
	if c.invocationWatchdog.Load() != nil {
		t.Fatal("preflight watchdog remained installed after close")
	}
	if got := abortCalls.Load(); got < 1 {
		t.Fatalf("provider abort calls = %d, want at least one", got)
	}
}

func TestProviderBoundaryStartupFailurePublishesCanonicalRunResult(t *testing.T) {
	workspace := t.TempDir()
	c := &Coordinator{
		providerManager: &agent.ProviderManager{},
		providerBoundaryStart: func(context.Context, string) error {
			return errors.New("synthetic provider admission failure")
		},
		session:      &TeamSession{Workspace: workspace, Config: agent.TeamConfig{Name: "startup-failure"}},
		sessionData:  NewSession(),
		taskTracker:  NewTaskTracker(),
		reportStatus: func(StatusEvent) {},
	}

	result, err := c.Run(context.Background(), "startup must fail canonically")
	if err == nil || result != "" {
		t.Fatalf("Run result=%q err=%v, want startup error and empty result", result, err)
	}
	canonical := c.LastRunResult()
	if canonical == nil || canonical.Outcome != RunOutcomeFailed || canonical.StopReason != StopReasonRunFailed || canonical.ExitCode != 1 {
		t.Fatalf("canonical startup result = %#v, want failed/run_failed/1", canonical)
	}
	if c.sessionData.RunResult == nil || c.sessionData.RunResult.Outcome != RunOutcomeFailed {
		t.Fatalf("session startup result = %#v, want failed", c.sessionData.RunResult)
	}

	store, err := OpenEventStore(workspace)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	events, err := store.ReadEvents()
	if err != nil {
		t.Fatal(err)
	}
	var finished RunFinishedEventPayload
	found := false
	for _, event := range events {
		if event.Type != "run_finished" {
			continue
		}
		if err := json.Unmarshal(event.Payload, &finished); err != nil {
			t.Fatal(err)
		}
		found = true
	}
	if !found || finished.Outcome != RunOutcomeFailed {
		t.Fatalf("run_finished outcome = %q, found=%t, want failed", finished.Outcome, found)
	}
	if _, err := os.Stat(workspace); err != nil {
		t.Fatal(err)
	}
}

func TestPublicAdmissionFailuresFinalizeExactlyOnce(t *testing.T) {
	startupErr := errors.New("synthetic provider admission failure")
	tests := []struct {
		name string
		call func(*Coordinator) error
		make func(string) *Coordinator
	}{
		{
			name: "run startup",
			make: func(workspace string) *Coordinator {
				return &Coordinator{
					providerManager: &agent.ProviderManager{},
					providerBoundaryStart: func(context.Context, string) error {
						return startupErr
					},
					session:     &TeamSession{Workspace: workspace, Config: agent.TeamConfig{Name: "run-admission"}},
					sessionData: NewSession(), taskTracker: NewTaskTracker(),
				}
			},
			call: func(c *Coordinator) error { _, err := c.Run(context.Background(), "admission"); return err },
		},
		{
			name: "continuation startup",
			make: func(workspace string) *Coordinator {
				return &Coordinator{
					providerManager: &agent.ProviderManager{},
					providerBoundaryStart: func(context.Context, string) error {
						return startupErr
					},
					session:     &TeamSession{Workspace: workspace, Config: agent.TeamConfig{Name: "continue-admission"}},
					sessionData: NewSession(), taskTracker: NewTaskTracker(),
				}
			},
			call: func(c *Coordinator) error {
				_, err := c.ContinueWithPrompt(context.Background(), "admission")
				return err
			},
		},
		{
			name: "direct agent resolution",
			make: func(workspace string) *Coordinator {
				return &Coordinator{
					session:     &TeamSession{Workspace: workspace, Config: agent.TeamConfig{Name: "direct-admission"}},
					sessionData: NewSession(), taskTracker: NewTaskTracker(),
				}
			},
			call: func(c *Coordinator) error {
				_, err := c.RunDirectAgent(context.Background(), "missing", "admission")
				return err
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			workspace := t.TempDir()
			c := tc.make(workspace)
			if err := tc.call(c); err == nil {
				t.Fatal("public admission unexpectedly succeeded")
			}
			first := c.LastRunResult()
			if first == nil || first.Outcome != RunOutcomeFailed || first.ExitCode != 1 {
				t.Fatalf("LastRunResult = %#v, want failed/exit 1", first)
			}
			if c.sessionData == nil || c.sessionData.RunResult == nil || c.sessionData.RunResult.Outcome != RunOutcomeFailed {
				t.Fatalf("session RunResult = %#v, want failed", c.sessionData)
			}
			// A second finalizer call must not replace the canonical result or
			// create another terminal lifecycle record.
			c.finalizePublicInvocationFailure(errors.New("duplicate admission report"))
			if c.LastRunResult() != first {
				t.Fatalf("idempotent finalizer replaced result: got=%#v want=%#v", c.LastRunResult(), first)
			}
			eventStore, err := OpenEventStore(workspace)
			if err != nil {
				t.Fatal(err)
			}
			defer eventStore.Close()
			events, err := eventStore.ReadEvents()
			if err != nil {
				t.Fatal(err)
			}
			finished := 0
			for _, event := range events {
				if event.Type == "run_finished" {
					finished++
				}
			}
			if finished != 1 {
				t.Fatalf("run_finished count = %d, want exactly one; events=%#v", finished, events)
			}
		})
	}
}

func TestRecoveryAdmissionUsesCanonicalTerminalLifecycle(t *testing.T) {
	tests := []struct {
		name string
		call func(*Coordinator) error
	}{
		{
			name: "run",
			call: func(c *Coordinator) error {
				_, err := c.Run(context.Background(), "must not execute")
				return err
			},
		},
		{
			name: "continue",
			call: func(c *Coordinator) error {
				_, err := c.ContinueWithPrompt(context.Background(), "must not execute")
				return err
			},
		},
		{
			name: "direct agent",
			call: func(c *Coordinator) error {
				_, err := c.RunDirectAgent(context.Background(), "helper", "must not execute")
				return err
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			workspace := t.TempDir()
			store, err := NewEventStore(workspace, "seed-run", "seed-session")
			if err != nil {
				t.Fatal(err)
			}
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}

			providerCalls := 0
			c := &Coordinator{
				providerManager: &agent.ProviderManager{},
				providerBoundaryStart: func(context.Context, string) error {
					providerCalls++
					return nil
				},
				session: &TeamSession{
					Workspace: workspace,
					Config:    agent.TeamConfig{Name: "recovery-team"},
				},
				sessionData: NewSession(),
				taskTracker: NewTaskTracker(),
			}
			c.sessionData.RecoveryRequired = true
			c.sessionData.RecoveryReason = "projection mismatch"

			publicErr := tc.call(c)
			if publicErr == nil {
				t.Fatal("recovery admission unexpectedly succeeded")
			}
			var outcomeErr *RunOutcomeError
			if !errors.As(publicErr, &outcomeErr) {
				t.Fatalf("public recovery error = %T (%v), want RunOutcomeError", publicErr, publicErr)
			}
			if got := outcomeErr.ProcessExitCode(); got != 7 {
				t.Fatalf("public recovery ProcessExitCode() = %d, want 7", got)
			}
			if providerCalls != 0 {
				t.Fatalf("provider boundary calls = %d, want 0", providerCalls)
			}
			if got := len(c.taskTracker.TodoList().Items()); got != 0 {
				t.Fatalf("created tasks = %d, want 0", got)
			}

			canonical := c.LastRunResult()
			if outcomeErr.Result != canonical {
				t.Fatalf("public error result = %#v, want canonical result %#v", outcomeErr.Result, canonical)
			}
			if canonical == nil || canonical.Outcome != RunOutcomeBlocked || canonical.StopReason != StopReasonPolicyViolation || canonical.ExitCode != 7 {
				t.Fatalf("LastRunResult = %#v, want blocked/policy_violation/7", canonical)
			}
			if len(canonical.UnresolvedTasks) != 1 || canonical.UnresolvedTasks[0].FailureClass != FailurePolicy {
				t.Fatalf("blocked task reference = %#v, want one policy failure", canonical.UnresolvedTasks)
			}
			if c.sessionData.RunResult != canonical {
				t.Fatalf("session result pointer = %#v, want canonical result", c.sessionData.RunResult)
			}

			reloaded := LoadSession(workspace)
			if reloaded == nil || reloaded.RunResult == nil {
				t.Fatalf("reloaded session result = %#v, want canonical result", reloaded)
			}
			if reloaded.RunResult.Outcome != canonical.Outcome || reloaded.RunResult.StopReason != canonical.StopReason || reloaded.RunResult.ExitCode != canonical.ExitCode || len(reloaded.RunResult.UnresolvedTasks) != 1 || reloaded.RunResult.UnresolvedTasks[0].FailureClass != FailurePolicy {
				t.Fatalf("reloaded result = %#v, want parity with %#v", reloaded.RunResult, canonical)
			}

			first := canonical
			c.finalizePublicInvocationFailure(errors.New("duplicate recovery finalization"))
			if c.LastRunResult() != first {
				t.Fatalf("idempotent finalizer replaced result: got=%#v want=%#v", c.LastRunResult(), first)
			}
			eventStore, err := OpenEventStore(workspace)
			if err != nil {
				t.Fatal(err)
			}
			defer eventStore.Close()
			events, err := eventStore.ReadEvents()
			if err != nil {
				t.Fatal(err)
			}
			finished := 0
			for _, event := range events {
				if event.Type != "run_finished" {
					continue
				}
				finished++
				var payload LifecycleEventPayload
				if err := json.Unmarshal(event.Payload, &payload); err != nil {
					t.Fatal(err)
				}
				if payload.Outcome != RunOutcomeBlocked || payload.StopReason != StopReasonPolicyViolation || payload.ExitCode != 7 {
					t.Fatalf("run_finished payload = %#v, want blocked/policy_violation/7", payload)
				}
			}
			if finished != 1 {
				t.Fatalf("run_finished count = %d, want exactly one; events=%#v", finished, events)
			}
			replayed := ReduceToSessionData(events)
			if replayed == nil || replayed.RunResult == nil || replayed.RunResult.Outcome != canonical.Outcome || replayed.RunResult.StopReason != canonical.StopReason || replayed.RunResult.ExitCode != canonical.ExitCode || len(replayed.RunResult.UnresolvedTasks) != 1 || replayed.RunResult.UnresolvedTasks[0].FailureClass != FailurePolicy {
				t.Fatalf("replayed result = %#v, want parity with %#v", replayed, canonical)
			}
		})
	}
}
