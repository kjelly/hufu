package team

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/anomalyco/hufu/internal/agent"
	"github.com/anomalyco/hufu/internal/utils"
)

// This file contains integration-level characterization tests for the WP-08
// refactoring that routes the retry loop through DecideRecovery. Each test
// drives the actual executeTask retry loop with a mock agent and verifies
// that the five pre-refactoring early-break paths produce the same observable
// behaviour (worker call count, task status, error) as before.
//
// Refs: docs/hufu-generic-task-reliability-mechanisms.md §5, §6.1, WP-08

// --- Mock agents ---

// alwaysFailAgent returns a non-nil error on every call.
type alwaysFailAgent struct {
	calls int
	err   error
}

func (a *alwaysFailAgent) Generate(ctx context.Context, call fantasy.AgentCall) (*fantasy.AgentResult, error) {
	return a.Stream(ctx, fantasy.AgentStreamCall{Prompt: call.Prompt, Messages: call.Messages})
}

func (a *alwaysFailAgent) Stream(ctx context.Context, call fantasy.AgentStreamCall) (*fantasy.AgentResult, error) {
	a.calls++
	if a.err == nil {
		a.err = errors.New("worker failed: generic execution error")
	}
	// Return partial output alongside the error so runAgentWithStatusAndHistory
	// preserves evidence (§6.1: retry prompt must include what was attempted).
	return &fantasy.AgentResult{
		Response: fantasy.Response{Content: fantasy.ResponseContent{
			fantasy.TextContent{Text: "partial work before failure"},
		}},
	}, a.err
}

// succeedOnSecondAgent fails on the first call and succeeds on the second.
type succeedOnSecondAgent struct {
	calls int
}

func (a *succeedOnSecondAgent) Generate(ctx context.Context, call fantasy.AgentCall) (*fantasy.AgentResult, error) {
	return a.Stream(ctx, fantasy.AgentStreamCall{Prompt: call.Prompt, Messages: call.Messages})
}

func (a *succeedOnSecondAgent) Stream(ctx context.Context, call fantasy.AgentStreamCall) (*fantasy.AgentResult, error) {
	a.calls++
	if a.calls == 1 {
		return &fantasy.AgentResult{
			Response: fantasy.Response{Content: fantasy.ResponseContent{
				fantasy.TextContent{Text: "first attempt produced partial output"},
			}},
		}, errors.New("worker failed: first attempt error")
	}
	return &fantasy.AgentResult{
		Response: fantasy.Response{Content: fantasy.ResponseContent{
			fantasy.TextContent{Text: "succeeded on retry"},
		}},
	}, nil
}

// unfixableVerifyAgent produces output that triggers a wrong-polarity verify
// failure. The verify command uses `grep -c` without negation, which the
// coordinator detects as unfixable wrong polarity.
type unfixableVerifyAgent struct {
	calls int
}

func (a *unfixableVerifyAgent) Generate(ctx context.Context, call fantasy.AgentCall) (*fantasy.AgentResult, error) {
	return a.Stream(ctx, fantasy.AgentStreamCall{Prompt: call.Prompt, Messages: call.Messages})
}

func (a *unfixableVerifyAgent) Stream(ctx context.Context, call fantasy.AgentStreamCall) (*fantasy.AgentResult, error) {
	a.calls++
	return &fantasy.AgentResult{
		Response: fantasy.Response{Content: fantasy.ResponseContent{
			fantasy.TextContent{Text: "task completed"},
		}},
	}, nil
}

// --- Helpers ---

// newWP08TestCoordinator builds a minimal Coordinator wired for retry-loop
// integration tests.
func newWP08TestCoordinator(t *testing.T, worker fantasy.Agent, maxRetries int) (*Coordinator, *[]string) {
	t.Helper()
	workspace := t.TempDir()
	t.Cleanup(func() { time.Sleep(100 * time.Millisecond) })
	var events []string
	c := &Coordinator{
		session: &TeamSession{
			Workspace: workspace,
			Config: agent.TeamConfig{
				Name:       "wp08-retry",
				Timeout:    30,
				MaxRetries: maxRetries,
			},
			Agents: map[string]*agent.AgentDef{
				"worker": {Name: "worker", Role: "worker", Generation: agent.GenerationParams{Model: "test"}, MaxRetries: -1},
			},
		},
		sessionTime:     time.Now(),
		taskTracker:     NewTaskTracker(),
		reportStatus:    func(e StatusEvent) { events = append(events, e.Type) },
		taskResultCache: make(map[string][]cachedTaskEntry),
		executionRunID:  "run-wp08",
	}
	c.workerAgentOverride = worker
	return c, &events
}

// --- Characterization tests for the five early-break paths ---

// TestWP08_Path1_TerminalBlocked verifies that when a terminal session is
// active, the retry loop stops immediately after the first attempt without
// retrying (path 1: terminalBlocked).
//
// Refs: docs/hufu-generic-task-reliability-mechanisms.md §6.1, WP-08
func TestWP08_Path1_TerminalBlocked(t *testing.T) {
	worker := &alwaysFailAgent{}
	c, _ := newWP08TestCoordinator(t, worker, 3)
	items := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "do work"}})
	todoID := items[0].ID

	// Inject an unreconciled restored session. A PID identity mismatch makes
	// cleanup fail closed and therefore requires human reconciliation instead
	// of replaying the task.
	mgr, err := NewTerminalSessionManager(c.session.Workspace, nil)
	if err != nil {
		t.Fatalf("NewTerminalSessionManager: %v", err)
	}
	// no explicit cleanup needed; temp workspace is removed by t.TempDir()
	mgr.mu.Lock()
	mgr.sessions[todoID] = &managedTerminalSession{
		session: TerminalSession{
			ID:          "test-session",
			OwnerTaskID: todoID,
			PID:         os.Getpid(),
			State:       TerminalSessionUnknown,
			ProcessIdentity: &ProcessIdentity{
				PID: os.Getpid(), StartTime: -1,
			},
		},
	}
	mgr.mu.Unlock()
	c.terminalSessionMgr = mgr

	_, err = c.executeTask(context.Background(), TaskDef{
		Agent: "worker", Goal: "do work",
	}, todoID)
	if err == nil {
		t.Fatal("expected error from terminal-blocked execution")
	}
	if worker.calls != 1 {
		t.Errorf("worker dispatched %d time(s), want 1 (terminal blocked must break before retry)", worker.calls)
	}
	var item *TodoItem
	for _, candidate := range c.taskTracker.TodoList().Items() {
		if candidate.ID == todoID {
			item = candidate
			break
		}
	}
	if item == nil || item.Status != TaskBlocked {
		t.Fatalf("task status after terminal cleanup = %#v, want blocked", item)
	}
	sessions, listErr := mgr.List(context.Background(), "")
	if listErr != nil || len(sessions) != 1 {
		t.Fatalf("persisted terminal sessions = %#v, err=%v", sessions, listErr)
	}
	if sessions[0].CleanupState != TerminalCleanupManual || !strings.Contains(sessions[0].CleanupError, "identity mismatch") {
		t.Fatalf("persisted terminal cleanup disposition = %#v, want manual identity-mismatch intervention", sessions[0])
	}
	restored, restoreErr := NewTerminalSessionManager(c.session.Workspace, nil)
	if restoreErr != nil {
		t.Fatal(restoreErr)
	}
	persisted, listErr := restored.List(context.Background(), "")
	if listErr != nil || len(persisted) != 1 || persisted[0].CleanupState != TerminalCleanupManual {
		t.Fatalf("manual terminal disposition was not durable: %#v, err=%v", persisted, listErr)
	}
}

// TestWP08_Path2_ProtocolFailureNonReplayable verifies that a protocol failure
// on a non-replayable task (external_write) blocks for reconciliation without
// retrying the worker (path 2: protocolFailure && !IsTaskReplayable).
//
// Refs: docs/hufu-generic-task-reliability-mechanisms.md §6.1, WP-08
func TestWP08_Path2_ProtocolFailureNonReplayable(t *testing.T) {
	worker := &alwaysFailAgent{}
	c, _ := newWP08TestCoordinator(t, worker, 3)
	items := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "external task"}})
	todoID := items[0].ID

	_, err := c.executeTask(context.Background(), TaskDef{
		Agent:      "worker",
		Goal:       "external task",
		SideEffect: SideEffectExternalWrite,
		Execution:  ExecutionContract{RequiresResult: true},
	}, todoID)
	if err == nil {
		t.Fatal("expected error from protocol failure on non-replayable task")
	}
	// Worker should only be called once — the protocol failure on a
	// non-replayable task blocks immediately.
	if worker.calls > 1 {
		t.Errorf("worker dispatched %d time(s), want ≤ 1 (protocol failure on non-replayable must not retry)", worker.calls)
	}
	item := c.taskTracker.TodoList().Items()[0]
	if item.Status != TaskBlocked {
		t.Errorf("task status = %s, want blocked (protocol failure on non-replayable must block)", item.Status)
	}
}

// TestWP08_Path3_NonReplayableTask verifies that any failure on a
// non-replayable task (non-protocol) blocks for reconciliation without
// retrying (path 3: !CanAutomaticallyReplay).
//
// Refs: docs/hufu-generic-task-reliability-mechanisms.md §6.1, WP-08
func TestWP08_Path3_NonReplayableTask(t *testing.T) {
	worker := &alwaysFailAgent{}
	c, _ := newWP08TestCoordinator(t, worker, 3)
	items := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "infra task"}})
	todoID := items[0].ID

	_, err := c.executeTask(context.Background(), TaskDef{
		Agent:      "worker",
		Goal:       "infra task",
		SideEffect: SideEffectInfraMutation,
	}, todoID)
	if err == nil {
		t.Fatal("expected error from non-replayable task failure")
	}
	if worker.calls != 1 {
		t.Errorf("worker dispatched %d time(s), want 1 (non-replayable task must not retry)", worker.calls)
	}
	item := c.taskTracker.TodoList().Items()[0]
	if item.Status != TaskBlocked {
		t.Errorf("task status = %s, want blocked (non-replayable task must block)", item.Status)
	}
}

// TestWP08_Path4_UnfixableVerifyFailure verifies that a wrong-polarity verify
// command stops retrying after the first occurrence (path 4:
// isUnfixableVerifyFailure). The verify command is set by the coordinator and
// the worker cannot fix it by retrying.
//
// Refs: docs/hufu-generic-task-reliability-mechanisms.md §6.1, WP-08
func TestWP08_Path4_UnfixableVerifyFailure(t *testing.T) {
	worker := &alwaysFailAgent{err: errWrongVerificationPolarity}
	c, _ := newWP08TestCoordinator(t, worker, 3)
	items := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "cleanup task"}})
	todoID := items[0].ID

	_, err := c.executeTask(context.Background(), TaskDef{
		Agent:  "worker",
		Goal:   "cleanup task",
		Verify: "nonexistent-cmd | grep -c running",
	}, todoID)
	if err == nil {
		t.Fatal("expected error from unfixable verify failure")
	}
	// The unfixable verify error should stop retries after the first attempt
	// (when attempt < maxRetries).
	if worker.calls > 1 {
		t.Errorf("worker dispatched %d time(s), want ≤ 1 (unfixable verify must not retry)", worker.calls)
	}
}

// TestWP08_Path5_SameFailureRepeated verifies that when the same error occurs
// on consecutive attempts, the retry loop stops early (path 5: sameFailure).
//
// Refs: docs/hufu-generic-task-reliability-mechanisms.md §6.1, WP-08
func TestWP08_Path5_SameFailureRepeated(t *testing.T) {
	worker := &alwaysFailAgent{err: errors.New("worker failed: identical error")}
	c, _ := newWP08TestCoordinator(t, worker, 3)
	items := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "repeated failure task"}})
	todoID := items[0].ID

	_, err := c.executeTask(context.Background(), TaskDef{
		Agent: "worker", Goal: "repeated failure task",
	}, todoID)
	if err == nil {
		t.Fatal("expected error from repeated failure")
	}
	// The same failure on attempt 2 should stop retries — worker should
	// be called exactly 2 times (attempt 1 fails, attempt 2 repeats, stop).
	if worker.calls != 2 {
		t.Errorf("worker dispatched %d time(s), want 2 (same failure repeated should stop after 2nd attempt)", worker.calls)
	}
}

// TestWP08_NormalRetry verifies that a normal execution failure on a
// replayable task retries the worker (the fallthrough path that became
// RetryWorker in the refactored loop).
//
// Refs: docs/hufu-generic-task-reliability-mechanisms.md §6.1, WP-08
func TestWP08_NormalRetry(t *testing.T) {
	worker := &succeedOnSecondAgent{}
	c, _ := newWP08TestCoordinator(t, worker, 3)
	items := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "retryable task"}})
	todoID := items[0].ID

	out, err := c.executeTask(context.Background(), TaskDef{
		Agent: "worker", Goal: "retryable task",
	}, todoID)
	if err != nil {
		t.Fatalf("expected retry to succeed, got: %v", err)
	}
	if worker.calls != 2 {
		t.Errorf("worker dispatched %d time(s), want 2 (normal retry should dispatch twice)", worker.calls)
	}
	if strings.TrimSpace(out) == "" {
		t.Error("expected non-empty output after successful retry")
	}
	item := c.taskTracker.TodoList().Items()[0]
	if item.Status != TaskDone {
		t.Errorf("task status = %s, want done (normal retry should succeed)", item.Status)
	}
}

// TestWP08_ParentContextCancelled verifies that when the parent context is
// cancelled, the retry loop stops after the first attempt without retrying
// (matching the pre-refactoring parentCtx.Err() check).
//
// Refs: docs/hufu-generic-task-reliability-mechanisms.md §5.3, WP-08
func TestWP08_ParentContextCancelled(t *testing.T) {
	worker := &alwaysFailAgent{}
	c, _ := newWP08TestCoordinator(t, worker, 3)
	items := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "cancelled task"}})
	todoID := items[0].ID

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := c.executeTask(ctx, TaskDef{
		Agent: "worker", Goal: "cancelled task",
	}, todoID)
	if err == nil {
		t.Fatal("expected error from cancelled execution")
	}
	if worker.calls != 1 {
		t.Errorf("worker dispatched %d time(s), want 1 (parent cancel must break before retry)", worker.calls)
	}
}

// varyingErrorAgent returns a different error on each call so that
// the normalized fingerprint differs and sameFailure/repeat detection
// does not trigger. The errors use distinct failure reasons (not just
// attempt numbers, which NormalizeFailureError treats as volatile).
type varyingErrorAgent struct {
	calls int
}

func (a *varyingErrorAgent) Generate(ctx context.Context, call fantasy.AgentCall) (*fantasy.AgentResult, error) {
	return a.Stream(ctx, fantasy.AgentStreamCall{Prompt: call.Prompt, Messages: call.Messages})
}

func (a *varyingErrorAgent) Stream(ctx context.Context, call fantasy.AgentStreamCall) (*fantasy.AgentResult, error) {
	a.calls++
	partialOutput := fmt.Sprintf("partial output for attempt %d", a.calls)
	switch a.calls {
	case 1:
		return &fantasy.AgentResult{Response: fantasy.Response{Content: fantasy.ResponseContent{fantasy.TextContent{Text: partialOutput}}}}, errors.New("worker failed: connection refused")
	case 2:
		return &fantasy.AgentResult{Response: fantasy.Response{Content: fantasy.ResponseContent{fantasy.TextContent{Text: partialOutput}}}}, errors.New("worker failed: file not found")
	default:
		return &fantasy.AgentResult{Response: fantasy.Response{Content: fantasy.ResponseContent{fantasy.TextContent{Text: partialOutput}}}}, errors.New("worker failed: permission denied")
	}
}

// TestWP08_BudgetExhausted verifies that when the retry budget is exhausted,
// the loop stops after maxRetries attempts.
//
// Refs: docs/hufu-generic-task-reliability-mechanisms.md §6.1, WP-08
func TestWP08_BudgetExhausted(t *testing.T) {
	worker := &varyingErrorAgent{}
	c, _ := newWP08TestCoordinator(t, worker, 3)
	items := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "budget task"}})
	todoID := items[0].ID

	_, err := c.executeTask(context.Background(), TaskDef{
		Agent: "worker", Goal: "budget task",
	}, todoID)
	if err == nil {
		t.Fatal("expected error from budget exhaustion")
	}
	// Each attempt has a different error (attempt number is in the error),
	// so sameFailure does not trigger. Worker should be called maxRetries times.
	if worker.calls != 3 {
		t.Errorf("worker dispatched %d time(s), want 3 (budget exhausted after 3 attempts)", worker.calls)
	}
}

// TestWP08_ClassBased_ContractFailure verifies that a contract failure on a
// replayable task stops retrying (new behaviour from §5: contract →
// replan_required). This is a NEW behaviour introduced by routing the loop
// through DecideRecovery's class-based disposition.
//
// Refs: docs/hufu-generic-task-reliability-mechanisms.md §5, §6.1, WP-08
func TestWP08_ClassBased_ContractFailure(t *testing.T) {
	worker := &alwaysFailAgent{err: fmt.Errorf("contract preflight failed: verify (verifier_not_asserting): tail || echo")}
	c, _ := newWP08TestCoordinator(t, worker, 3)
	items := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "contract task"}})
	todoID := items[0].ID

	_, err := c.executeTask(context.Background(), TaskDef{
		Agent: "worker", Goal: "contract task",
	}, todoID)
	if err == nil {
		t.Fatal("expected error from contract failure")
	}
	// Contract failures should not retry the worker (§5: replan_required).
	if worker.calls != 1 {
		t.Errorf("worker dispatched %d time(s), want 1 (contract failure should not retry, §5)", worker.calls)
	}
}

// TestWP08_ClassBased_EnvironmentFailure verifies that an environment failure
// on a replayable task stops retrying (new behaviour from §5: environment →
// replan_required).
//
// Refs: docs/hufu-generic-task-reliability-mechanisms.md §5, WP-08
func TestWP08_ClassBased_EnvironmentFailure(t *testing.T) {
	worker := &alwaysFailAgent{err: errors.New("bash: nonexistent-cmd: command not found")}
	c, _ := newWP08TestCoordinator(t, worker, 3)
	items := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "env task"}})
	todoID := items[0].ID

	_, err := c.executeTask(context.Background(), TaskDef{
		Agent: "worker", Goal: "env task",
	}, todoID)
	if err == nil {
		t.Fatal("expected error from environment failure")
	}
	// Environment failures should not retry (§5: replan_required).
	if worker.calls != 1 {
		t.Errorf("worker dispatched %d time(s), want 1 (environment failure should not retry, §5)", worker.calls)
	}
}

// TestWP08_ProfilePolicyNotBypassed_Unattended verifies that an empty task
// recovery policy under the unattended profile (which resolves to
// RecoveryReconcile) blocks the task instead of retrying the worker. This is
// the reviewer's P1 finding: the resolved profile recovery policy must not
// be bypassed by the raw task.Recovery field.
//
// Refs: docs/hufu-generic-task-reliability-mechanisms.md §6.1, WP-08 (reviewer P1)
func TestWP08_ProfilePolicyNotBypassed_Unattended(t *testing.T) {
	worker := &alwaysFailAgent{}
	c, _ := newWP08TestCoordinator(t, worker, 3)
	c.unattended = true
	unattended, _ := GetBuiltinProfile(string(ProfileUnattended))
	c.executionProfile = unattended
	items := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "unattended task"}})
	todoID := items[0].ID

	_, err := c.executeTask(context.Background(), TaskDef{
		Agent: "worker", Goal: "unattended task",
		// No explicit SideEffect or Recovery — defaults to SideEffectNone,
		// but the unattended profile resolves RecoveryPolicy to
		// RecoveryReconcile, which must block retry.
	}, todoID)
	if err == nil {
		t.Fatal("expected error from execution failure under unattended profile")
	}
	// Worker should only be called once — the resolved reconcile policy
	// must block retry.
	if worker.calls != 1 {
		t.Errorf("worker dispatched %d time(s), want 1 (unattended profile resolve=reconcile must block retry, §6.1)", worker.calls)
	}
	item := c.taskTracker.TodoList().Items()[0]
	if item.Status != TaskBlocked {
		t.Errorf("task status = %s, want blocked (unattended profile resolves to reconcile, §6.1)", item.Status)
	}
}

// TestWP08_ProfilePolicyNotBypassed_StrictVerification verifies the same
// invariant under the strict-verification profile.
//
// Refs: docs/hufu-generic-task-reliability-mechanisms.md §6.1, WP-08 (reviewer P1)
func TestWP08_ProfilePolicyNotBypassed_StrictVerification(t *testing.T) {
	worker := &alwaysFailAgent{}
	c, _ := newWP08TestCoordinator(t, worker, 3)
	strict, _ := GetBuiltinProfile(string(ProfileStrictVerification))
	c.executionProfile = strict
	items := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "strict task"}})
	todoID := items[0].ID

	_, err := c.executeTask(context.Background(), TaskDef{
		Agent: "worker", Goal: "strict task",
	}, todoID)
	if err == nil {
		t.Fatal("expected error from execution failure under strict profile")
	}
	if worker.calls != 1 {
		t.Errorf("worker dispatched %d time(s), want 1 (strict profile resolve=reconcile must block retry, §6.1)", worker.calls)
	}
	item := c.taskTracker.TodoList().Items()[0]
	if item.Status != TaskBlocked {
		t.Errorf("task status = %s, want blocked (strict profile resolves to reconcile, §6.1)", item.Status)
	}
}

// TestWP08_FirstContractFailureNotRecordedAsRepeated verifies that a first
// contract/environment/policy failure is NOT persisted as a "repeated
// failure". This is the reviewer's P2 finding: the ReplanRequired branch was
// setting lastErr = err before the sameFailure check, making the comparison
// always true.
//
// Refs: docs/hufu-generic-task-reliability-mechanisms.md §6.1, WP-08 (reviewer P2)
func TestWP08_FirstContractFailureNotRecordedAsRepeated(t *testing.T) {
	worker := &alwaysFailAgent{err: fmt.Errorf("contract preflight failed: verify (verifier_not_asserting): tail || echo")}
	c, events := newWP08TestCoordinator(t, worker, 3)
	items := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "contract task"}})
	todoID := items[0].ID

	_, err := c.executeTask(context.Background(), TaskDef{
		Agent: "worker", Goal: "contract task",
	}, todoID)
	if err == nil {
		t.Fatal("expected error from contract failure")
	}
	if worker.calls != 1 {
		t.Errorf("worker dispatched %d time(s), want 1 (contract failure should not retry)", worker.calls)
	}

	// The error message must NOT contain "repeated failure" — a first
	// contract failure is not a repeat.
	if strings.Contains(err.Error(), "repeated failure") {
		t.Errorf("first contract failure persisted as 'repeated failure': %q (reviewer P2: lastErr=err before sameFailure check makes it always true)", err.Error())
	}

	// Check the step events for the "repeated" message
	for _, e := range *events {
		_ = e // events only records types, not messages; error check is sufficient
	}
}

// TestWP08_FirstEnvironmentFailureNotRecordedAsRepeated is the same
// regression test for environment failures.
//
// Refs: docs/hufu-generic-task-reliability-mechanisms.md §6.1, WP-08 (reviewer P2)
func TestWP08_FirstEnvironmentFailureNotRecordedAsRepeated(t *testing.T) {
	worker := &alwaysFailAgent{err: errors.New("bash: nonexistent-cmd: command not found")}
	c, _ := newWP08TestCoordinator(t, worker, 3)
	items := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "env task"}})
	todoID := items[0].ID

	_, err := c.executeTask(context.Background(), TaskDef{
		Agent: "worker", Goal: "env task",
	}, todoID)
	if err == nil {
		t.Fatal("expected error from environment failure")
	}
	if worker.calls != 1 {
		t.Errorf("worker dispatched %d time(s), want 1 (environment failure should not retry)", worker.calls)
	}
	if strings.Contains(err.Error(), "repeated failure") {
		t.Errorf("first environment failure persisted as 'repeated failure': %q (reviewer P2)", err.Error())
	}
}

// promptCaptureAgent captures the prompt it receives on each call and
// returns an error on the first call, then succeeds on the second.
type promptCaptureAgent struct {
	calls   int
	prompts []string
}

func (a *promptCaptureAgent) Generate(ctx context.Context, call fantasy.AgentCall) (*fantasy.AgentResult, error) {
	return a.Stream(ctx, fantasy.AgentStreamCall{Prompt: call.Prompt, Messages: call.Messages})
}

func (a *promptCaptureAgent) Stream(ctx context.Context, call fantasy.AgentStreamCall) (*fantasy.AgentResult, error) {
	a.calls++
	a.prompts = append(a.prompts, call.Prompt)
	if a.calls == 1 {
		return &fantasy.AgentResult{
			Response: fantasy.Response{Content: fantasy.ResponseContent{
				fantasy.TextContent{Text: "first attempt partial output before error"},
			}},
		}, errors.New("worker failed: execution error on first attempt")
	}
	return &fantasy.AgentResult{
		Response: fantasy.Response{Content: fantasy.ResponseContent{
			fantasy.TextContent{Text: "succeeded on retry"},
		}},
	}, nil
}

// TestWP08_RetryContextContainsRequiredFields verifies that the second
// attempt's prompt contains the §6.1 required fields: failure class,
// evidence reference, and explicit mutable fields. When the worker fails
// with an execution error (no verify), the retry context includes class,
// evidence (error), and change guidance.
//
// Refs: docs/hufu-generic-task-reliability-mechanisms.md §6.1, WP-08 (reviewer P1)
func TestWP08_RetryContextContainsRequiredFields(t *testing.T) {
	worker := &promptCaptureAgent{}
	c, _ := newWP08TestCoordinator(t, worker, 3)
	items := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "retry context task"}})
	todoID := items[0].ID

	_, err := c.executeTask(context.Background(), TaskDef{
		Agent: "worker", Goal: "retry context task",
	}, todoID)
	if err != nil {
		t.Fatalf("expected retry to succeed, got: %v (calls=%d)", err, worker.calls)
	}
	if worker.calls != 2 {
		t.Fatalf("worker dispatched %d time(s), want 2", worker.calls)
	}
	if len(worker.prompts) < 2 {
		t.Fatalf("expected at least 2 captured prompts, got %d", len(worker.prompts))
	}

	retryPrompt := worker.prompts[1]

	// 1. Failure class must be present
	if !strings.Contains(retryPrompt, "Failure class:") {
		t.Errorf("retry prompt missing 'Failure class:' field (§6.1)\nprompt: %s", retryPrompt)
	}

	// 2. Evidence reference must be present
	if !strings.Contains(retryPrompt, "Evidence:") {
		t.Errorf("retry prompt missing 'Evidence:' field (§6.1)\nprompt: %s", retryPrompt)
	}

	// 3. Previous command/exit must be present (always rendered, even if unavailable)
	if !strings.Contains(retryPrompt, "Previous command/exit:") {
		t.Errorf("retry prompt missing 'Previous command/exit:' field (§6.1)\nprompt: %s", retryPrompt)
	}

	// 4. Explicit mutable next-step fields must be present
	if !strings.Contains(retryPrompt, "What you can change:") {
		t.Errorf("retry prompt missing 'What you can change:' field (§6.1)\nprompt: %s", retryPrompt)
	}
}

// TestWP08_RetryContextContainsVerifyCommand verifies that when the previous
// attempt ran verification, the retry context includes the verify command and
// exit code. This uses a worker that always produces output (so verify runs)
// and a verify command that always fails (so the task fails and retries).
//
// Refs: docs/hufu-generic-task-reliability-mechanisms.md §6.1, WP-08 (reviewer P1)
func TestWP08_RetryContextContainsVerifyCommand(t *testing.T) {
	// Worker that always produces output (so err==nil and verify runs)
	worker := &mockWorkerTextAgent{text: "work output"}
	c, _ := newWP08TestCoordinator(t, worker, 3)
	items := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "verify context task"}})
	todoID := items[0].ID

	// Use a verify command that always fails. The same error will repeat,
	// so only 2 attempts will run (sameFailure detection on attempt 2).
	_, _ = c.executeTask(context.Background(), TaskDef{
		Agent:  "worker",
		Goal:   "verify context task",
		Verify: "test -f /nonexistent-deliverable-for-retry-test",
	}, todoID)

	// The task fails, but we only need to verify the retry context was built.
	// We can't capture the prompt with mockWorkerTextAgent, so test
	// buildRetryContext directly to verify verify command is included.
	verifyCmd := "test -f /nonexistent-deliverable"
	ctx := buildRetryContext(FailureVerify, errors.New("deliverable verification failed: exit code 1"), "", verifyCmd, 1, nil, "", "", "", false, "", TaskDef{Verify: verifyCmd})
	if !strings.Contains(ctx, "Previous command/exit:") {
		t.Errorf("retry context missing 'Previous command/exit:' field\ncontext: %s", ctx)
	}
	if !strings.Contains(ctx, "verify") {
		t.Errorf("retry context missing verify command reference\ncontext: %s", ctx)
	}
	if !strings.Contains(ctx, "exit: 1") {
		t.Errorf("retry context missing verify exit code\ncontext: %s", ctx)
	}
}

// TestWP08_RetryContextContainsExecutionClass verifies that the retry context
// correctly reports the failure class from the previous attempt.
//
// Refs: docs/hufu-generic-task-reliability-mechanisms.md §6.1, WP-08 (reviewer P1)
func TestWP08_RetryContextContainsExecutionClass(t *testing.T) {
	worker := &promptCaptureAgent{}
	c, _ := newWP08TestCoordinator(t, worker, 3)
	items := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "exec class task"}})
	todoID := items[0].ID

	_, _ = c.executeTask(context.Background(), TaskDef{
		Agent: "worker", Goal: "exec class task",
	}, todoID)

	if worker.calls < 2 {
		t.Fatalf("expected at least 2 worker calls, got %d", worker.calls)
	}

	retryPrompt := worker.prompts[1]
	// The first attempt failed with "worker failed: execution error" which
	// classifies as FailureExecution. The retry context should include this.
	if !strings.Contains(retryPrompt, "execution") {
		t.Errorf("retry prompt should contain 'execution' failure class, got: %s", retryPrompt)
	}
}

// TestWP08_ComputeEvidenceComplete_TranscriptRequired verifies that
// computeEvidenceComplete returns false when a transcript is required but
// not captured (the production EvidenceComplete gate is meaningful, not
// hard-coded true).
//
// Refs: docs/hufu-generic-task-reliability-mechanisms.md §6.1, WP-08 (reviewer P1)
func TestWP08_ComputeEvidenceComplete_TranscriptRequired(t *testing.T) {
	// Task requires a transcript (RequiresResult) but transcriptRef is empty
	// (manifest creation failed). Evidence should be incomplete.
	task := TaskDef{
		Execution: ExecutionContract{RequiresResult: true},
	}
	if computeEvidenceComplete(task, "", nil, "") {
		t.Error("computeEvidenceComplete should return false when transcript required but transcriptRef empty")
	}

	// Same task but with a valid transcriptRef → evidence complete
	if !computeEvidenceComplete(task, "/workspace/logs/transcript.json", nil, "") {
		t.Error("computeEvidenceComplete should return true when transcript required and transcriptRef present")
	}

	// Task without transcript requirement but no steps/output → evidence incomplete
	task2 := TaskDef{}
	if computeEvidenceComplete(task2, "", nil, "") {
		t.Error("computeEvidenceComplete should return false when no steps and no output (even without transcript requirement)")
	}
	// Task without transcript requirement but with output → evidence complete
	if !computeEvidenceComplete(task2, "", nil, "some output") {
		t.Error("computeEvidenceComplete should return true when output is present")
	}
	// Task without transcript requirement but with substantive steps (with messages) → evidence complete
	if !computeEvidenceComplete(task2, "", []fantasy.StepResult{{Messages: []fantasy.Message{fantasy.NewUserMessage("test")}}}, "") {
		t.Error("computeEvidenceComplete should return true when steps with messages are present")
	}
	// Empty StepResult{} (no messages, no content) → evidence incomplete
	if computeEvidenceComplete(task2, "", []fantasy.StepResult{{}}, "") {
		t.Error("computeEvidenceComplete should return false for empty StepResult{} (no messages = no evidence)")
	}
}

// TestWP08_EvidenceIncompleteBlocksRetry verifies that when a transcript is
// required but not captured, the retry loop does not dispatch the worker a
// second time (EvidenceComplete=false → ReplanRequired → no retry).
//
// This test creates a task with RequiresResult=true and a worker that
// produces output but omits submit_result (protocol failure). The transcript
// IS created in this case, so we instead verify the logic by directly testing
// that computeEvidenceComplete=false leads to no retry via DecideRecovery.
//
// Refs: docs/hufu-generic-task-reliability-mechanisms.md §6.1, WP-08 (reviewer P1)
func TestWP08_EvidenceIncompleteBlocksRetry(t *testing.T) {
	// Unit test: when EvidenceComplete=false, DecideRecovery returns
	// ReplanRequired (not RetryWorker), which stops the retry loop.
	in := RecoveryDecisionInput{
		Replayable:       true,
		FailureClass:     FailureExecution,
		RecoveryPolicy:   RecoveryRetry,
		EvidenceComplete: false,
		Attempt:          1,
		MaxRetries:       3,
	}
	got, _ := DecideRecovery(in)
	if got != ReplanRequired {
		t.Errorf("EvidenceComplete=false → %q, want ReplanRequired (no retry without complete evidence, §6.1)", got)
	}

	// When EvidenceComplete=true, the same input returns RetryWorker.
	in.EvidenceComplete = true
	got, _ = DecideRecovery(in)
	if got != RetryWorker {
		t.Errorf("EvidenceComplete=true → %q, want RetryWorker (retry with complete evidence)", got)
	}
}

// TestWP08_RetryContextContainsPartialOutput verifies that the partial
// output from the first attempt (which authorized EvidenceComplete=true)
// appears in the second attempt's retry context. The reviewer required that
// the evidence used by the gate must appear in the retry prompt.
//
// Refs: docs/hufu-generic-task-reliability-mechanisms.md §6.1, WP-08 (reviewer P1a)
func TestWP08_RetryContextContainsPartialOutput(t *testing.T) {
	worker := &promptCaptureAgent{}
	c, _ := newWP08TestCoordinator(t, worker, 3)
	items := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "evidence output task"}})
	todoID := items[0].ID

	_, err := c.executeTask(context.Background(), TaskDef{
		Agent: "worker", Goal: "evidence output task",
	}, todoID)
	if err != nil {
		t.Fatalf("expected retry to succeed, got: %v (calls=%d)", err, worker.calls)
	}
	if worker.calls != 2 {
		t.Fatalf("worker dispatched %d time(s), want 2", worker.calls)
	}

	retryPrompt := worker.prompts[1]
	// The partial output from attempt 1 ("first attempt partial output
	// before error") must appear in the retry context's Evidence field.
	if !strings.Contains(retryPrompt, "partial output:") {
		t.Errorf("retry prompt missing 'partial output:' in Evidence field (§6.1: evidence that authorized retry must appear in prompt)\nprompt: %s", retryPrompt)
	}
	if !strings.Contains(retryPrompt, "first attempt partial output") {
		t.Errorf("retry prompt missing the actual partial output text from attempt 1\nprompt: %s", retryPrompt)
	}
}

// TestWP08_EmptyStepDoesNotAuthorizeRetry verifies that an empty StepResult{}
// (no Messages, no content) does NOT authorize retry. The reviewer required
// that the evidence gate check for substantive content, not just slice length.
//
// Refs: docs/hufu-generic-task-reliability-mechanisms.md §6.1, WP-08 (reviewer P1a)
func TestWP08_EmptyStepDoesNotAuthorizeRetry(t *testing.T) {
	// Empty StepResult{} with no Messages → EvidenceComplete=false
	if computeEvidenceComplete(TaskDef{}, "", []fantasy.StepResult{{}}, "") {
		t.Error("computeEvidenceComplete should return false for empty StepResult{} (no messages = no evidence)")
	}
	// StepResult with Messages → EvidenceComplete=true
	if !computeEvidenceComplete(TaskDef{}, "", []fantasy.StepResult{{Messages: []fantasy.Message{fantasy.NewUserMessage("test")}}}, "") {
		t.Error("computeEvidenceComplete should return true for StepResult with Messages")
	}
	// Non-empty output → EvidenceComplete=true
	if !computeEvidenceComplete(TaskDef{}, "", nil, "some output") {
		t.Error("computeEvidenceComplete should return true for non-empty output")
	}
	// No steps, no output → EvidenceComplete=false
	if computeEvidenceComplete(TaskDef{}, "", nil, "") {
		t.Error("computeEvidenceComplete should return false for no steps and no output")
	}
}

// TestWP08_RetryContextContainsToolInput verifies that the retry context
// includes the actual tool call input (the command), not just the tool name.
// The reviewer required that the "Previous command/exit" field contain the
// actual previous command/input for tool-driven work.
//
// Refs: docs/hufu-generic-task-reliability-mechanisms.md §6.1, WP-08 (reviewer P1b)
func TestWP08_RetryContextContainsToolInput(t *testing.T) {
	// Direct unit test of buildRetryContext with tool call info
	ctx := buildRetryContext(
		FailureExecution,
		errors.New("worker failed: execution error"),
		"", "", 0, nil,
		"bash",                // lastToolCall
		"echo hello && false", // lastToolInput (actual command)
		"hello\nexit code: 1", // lastToolResult
		true,                  // lastToolResultErr
		"partial output",      // lastOutput
		TaskDef{},
	)
	if !strings.Contains(ctx, "Previous command/exit:") {
		t.Errorf("retry context missing 'Previous command/exit:' field\ncontext: %s", ctx)
	}
	if !strings.Contains(ctx, "bash") {
		t.Errorf("retry context missing tool name 'bash'\ncontext: %s", ctx)
	}
	if !strings.Contains(ctx, "echo hello") {
		t.Errorf("retry context missing actual tool input 'echo hello'\ncontext: %s", ctx)
	}
	if !strings.Contains(ctx, "error") {
		t.Errorf("retry context missing error indicator for failed tool result\ncontext: %s", ctx)
	}
	if strings.Count(ctx, "echo hello && false") != 1 {
		t.Errorf("retry context duplicated tool input; want one occurrence\ncontext: %s", ctx)
	}
	if !strings.Contains(ctx, "partial output:") {
		t.Errorf("retry context missing partial output evidence\ncontext: %s", ctx)
	}
}

// TestWP08_RetryContextRedactsCredentials verifies that tool input containing
// credentials is redacted before appearing in the retry context. The
// OnToolCall callback applies utils.RedactSecrets to tool input before
// storing it in per-attempt evidence. This test verifies the end-to-end
// redaction by passing credential-shaped input through the redaction
// pipeline and checking the retry context output.
//
// Refs: docs/hufu-generic-task-reliability-mechanisms.md §6.1, §9, WP-08 (reviewer P1b)
func TestWP08_RetryContextRedactsCredentials(t *testing.T) {
	// Simulate what the OnToolCall callback does: redact then truncate
	credentialInput := `export API_KEY=sk-secret-1234567890abcdef && curl -H "Authorization: Bearer sk-secret-1234567890abcdef" https://api.example.com`
	redactedInput := utils.TruncateRunes(utils.RedactSecrets(credentialInput), 500)

	// The redacted input must NOT contain the secret
	if strings.Contains(redactedInput, "sk-secret-1234567890abcdef") {
		t.Errorf("RedactSecrets did not redact the API key: %s", redactedInput)
	}

	// Build retry context with the redacted tool input
	ctx := buildRetryContext(
		FailureExecution,
		errors.New("worker failed: execution error"),
		"", "", 0, nil,
		"bash",           // lastToolCall
		redactedInput,    // lastToolInput (already redacted)
		"command failed", // lastToolResult
		true,             // lastToolResultErr
		"partial output", // lastOutput
		TaskDef{},
	)

	// The retry context must NOT contain the raw secret
	if strings.Contains(ctx, "sk-secret-1234567890abcdef") {
		t.Errorf("retry context leaked credential: %s", ctx)
	}
	// The retry context MUST contain the redacted marker
	if !strings.Contains(ctx, "[REDACTED]") {
		t.Errorf("retry context missing [REDACTED] marker for redacted credential\ncontext: %s", ctx)
	}
}

// TestWP08_RetryPromptRedactsAllUntrustedFields verifies the final prompt
// boundary, rather than only the tool callbacks. Errors, verifier commands,
// fallback output, and transcript references can all contain credentials.
func TestWP08_RetryPromptRedactsAllUntrustedFields(t *testing.T) {
	const (
		errSecret        = "err-secret-123"
		verifySecret     = "verify-secret-456"
		outputSecret     = "output-secret-789"
		transcriptSecret = "API_KEY=transcript-secret-000"
	)
	ctx := buildRetryContext(
		FailureVerify,
		errors.New("provider failed: api_key="+errSecret),
		"/tmp/"+transcriptSecret+"/transcript.json",
		"curl -H 'Authorization: Bearer "+verifySecret+"' https://example.invalid",
		1, nil, "bash", "echo safe", "failed", true,
		"previous output password="+outputSecret, TaskDef{},
	)
	for _, secret := range []string{errSecret, verifySecret, outputSecret, transcriptSecret} {
		if strings.Contains(ctx, secret) {
			t.Errorf("retry prompt leaked %q: %s", secret, ctx)
		}
	}
	if !strings.Contains(ctx, "[REDACTED]") {
		t.Errorf("retry prompt missing redaction marker: %s", ctx)
	}

	reflection := buildFailureReflectionPrompt("worker", "safe goal", "authorization: Bearer reflection-secret-321")
	if strings.Contains(reflection, "reflection-secret-321") || !strings.Contains(reflection, "[REDACTED]") {
		t.Errorf("reflection prompt did not redact failure input: %s", reflection)
	}
}

func TestWP08_RetryPartialOutputRedactsFallback(t *testing.T) {
	const secret = "fallback-secret-654"
	got := retryPartialOutput("", "previous output: token="+secret)
	if strings.Contains(got, secret) || !strings.Contains(got, "[REDACTED]") {
		t.Fatalf("fallback retry output was not redacted: %q", got)
	}
}

// sameCoordinatorConcurrentAgent synchronizes both first attempts and uses
// the todo ID carried in the execution context to produce task-specific
// evidence. It is shared by two tasks on one Coordinator so a coordinator-
// global evidence slot would be observable.
type sameCoordinatorConcurrentAgent struct {
	mu          sync.Mutex
	firstSeen   int
	release     chan struct{}
	calls       map[string]int
	prompts     map[string][]string
	toolInputs  map[string]string
	toolResults map[string]string
}

func newSameCoordinatorConcurrentAgent() *sameCoordinatorConcurrentAgent {
	return &sameCoordinatorConcurrentAgent{
		release: make(chan struct{}), calls: make(map[string]int), prompts: make(map[string][]string),
		toolInputs: make(map[string]string), toolResults: make(map[string]string),
	}
}

func (a *sameCoordinatorConcurrentAgent) Generate(ctx context.Context, call fantasy.AgentCall) (*fantasy.AgentResult, error) {
	return a.run(ctx, call.Prompt, nil, nil)
}

func (a *sameCoordinatorConcurrentAgent) Stream(ctx context.Context, call fantasy.AgentStreamCall) (*fantasy.AgentResult, error) {
	return a.run(ctx, call.Prompt, call.OnToolCall, call.OnToolResult)
}

func (a *sameCoordinatorConcurrentAgent) run(ctx context.Context, prompt string, onToolCall fantasy.OnToolCallFunc, onToolResult fantasy.OnToolResultFunc) (*fantasy.AgentResult, error) {
	todoID, _ := ctx.Value(todoIDKey{}).(string)
	a.mu.Lock()
	a.calls[todoID]++
	attempt := a.calls[todoID]
	a.prompts[todoID] = append(a.prompts[todoID], prompt)
	if attempt == 1 {
		a.firstSeen++
		if a.firstSeen == 2 {
			close(a.release)
		}
	}
	a.mu.Unlock()
	if attempt == 1 {
		toolInput := "echo evidence-" + todoID
		toolResult := "tool result for " + todoID
		if onToolCall != nil {
			if err := onToolCall(fantasy.ToolCallContent{
				ToolCallID: "call-" + todoID, ToolName: "bash", Input: toolInput,
			}); err != nil {
				return nil, err
			}
		}
		if onToolResult != nil {
			if err := onToolResult(fantasy.ToolResultContent{
				ToolCallID: "call-" + todoID, ToolName: "bash",
				Result: fantasy.ToolResultOutputContentText{Text: toolResult},
			}); err != nil {
				return nil, err
			}
		}
		a.mu.Lock()
		a.toolInputs[todoID] = toolInput
		a.toolResults[todoID] = toolResult
		a.mu.Unlock()
		<-a.release
		return &fantasy.AgentResult{Response: fantasy.Response{Content: fantasy.ResponseContent{
			fantasy.TextContent{Text: "partial evidence for " + todoID},
		}}}, errors.New("worker failed after task-specific evidence")
	}
	return &fantasy.AgentResult{Response: fantasy.Response{Content: fantasy.ResponseContent{
		fantasy.TextContent{Text: "completed " + todoID},
	}}}, nil
}

// TestWP08_ToolCallEvidencePerAttempt verifies per-attempt evidence under
// actual same-Coordinator concurrency. Both tasks share one worker override
// and overlap their first attempts; each retry prompt must contain only that
// task's evidence.
func TestWP08_ToolCallEvidencePerAttempt(t *testing.T) {
	worker := newSameCoordinatorConcurrentAgent()
	c, _ := newWP08TestCoordinator(t, worker, 3)
	c.reportStatus = func(StatusEvent) {}
	items := c.taskTracker.TodoList().AddBatch([]TodoSpec{
		{Agent: "worker", Desc: "task-A"},
		{Agent: "worker", Desc: "task-B"},
	})
	todoID1, todoID2 := items[0].ID, items[1].ID

	done := make(chan error, 2)
	go func() {
		_, err := c.executeTask(context.Background(), TaskDef{Agent: "worker", Goal: "task-A"}, todoID1)
		done <- err
	}()
	go func() {
		_, err := c.executeTask(context.Background(), TaskDef{Agent: "worker", Goal: "task-B"}, todoID2)
		done <- err
	}()
	for i := 0; i < 2; i++ {
		if err := <-done; err != nil {
			t.Errorf("task %d failed: %v", i, err)
		}
	}

	worker.mu.Lock()
	defer worker.mu.Unlock()
	if worker.calls[todoID1] != 2 || worker.calls[todoID2] != 2 {
		t.Fatalf("calls task-A=%d task-B=%d, want 2 each", worker.calls[todoID1], worker.calls[todoID2])
	}
	retryPrompt1 := worker.prompts[todoID1][1]
	retryPrompt2 := worker.prompts[todoID2][1]
	if !strings.Contains(retryPrompt1, "partial evidence for "+todoID1) || !strings.Contains(retryPrompt2, "partial evidence for "+todoID2) {
		t.Fatalf("retry prompts lost task-specific evidence\nA: %s\nB: %s", retryPrompt1, retryPrompt2)
	}
	if !strings.Contains(retryPrompt1, "echo evidence-"+todoID1) || !strings.Contains(retryPrompt2, "echo evidence-"+todoID2) {
		t.Fatalf("retry prompts lost callback tool input\nA: %s\nB: %s", retryPrompt1, retryPrompt2)
	}
	if !strings.Contains(retryPrompt1, "tool result for "+todoID1) || !strings.Contains(retryPrompt2, "tool result for "+todoID2) {
		t.Fatalf("retry prompts lost callback tool result\nA: %s\nB: %s", retryPrompt1, retryPrompt2)
	}
	if worker.toolInputs[todoID1] == "" || worker.toolInputs[todoID2] == "" || worker.toolResults[todoID1] == "" || worker.toolResults[todoID2] == "" {
		t.Fatalf("mock did not observe both callback paths: inputs=%#v results=%#v", worker.toolInputs, worker.toolResults)
	}
	if strings.Contains(retryPrompt1, "partial evidence for "+todoID2) || strings.Contains(retryPrompt2, "partial evidence for "+todoID1) {
		t.Fatalf("same-Coordinator retry prompts leaked cross-task evidence\nA: %s\nB: %s", retryPrompt1, retryPrompt2)
	}
	if strings.Contains(retryPrompt1, "echo evidence-"+todoID2) || strings.Contains(retryPrompt2, "echo evidence-"+todoID1) || strings.Contains(retryPrompt1, "tool result for "+todoID2) || strings.Contains(retryPrompt2, "tool result for "+todoID1) {
		t.Fatalf("same-Coordinator callback evidence leaked across retry prompts\nA: %s\nB: %s", retryPrompt1, retryPrompt2)
	}
}

// TestWP08_ToolCallEvidenceStruct verifies that toolCallEvidence is a
// per-attempt struct (not coordinator-global) by checking that two instances
// are independent and don't share state.
//
// Refs: docs/hufu-generic-task-reliability-mechanisms.md §6.1, WP-08 (reviewer P1b)
func TestWP08_ToolCallEvidenceStruct(t *testing.T) {
	ev1 := &toolCallEvidence{toolName: "bash", toolInput: "echo task1"}
	ev2 := &toolCallEvidence{toolName: "grep", toolInput: "echo task2"}

	// Modifying ev1 must not affect ev2
	ev1.resultText = "task1 result"
	ev1.resultErr = true

	if ev2.toolName != "grep" {
		t.Errorf("ev2.toolName = %q, want 'grep' (independent struct)", ev2.toolName)
	}
	if ev2.toolInput != "echo task2" {
		t.Errorf("ev2.toolInput = %q, want 'echo task2' (independent struct)", ev2.toolInput)
	}
	if ev2.resultText != "" {
		t.Errorf("ev2.resultText = %q, want '' (ev1 modification must not leak)", ev2.resultText)
	}
	if ev2.resultErr != false {
		t.Errorf("ev2.resultErr = %v, want false (ev1 modification must not leak)", ev2.resultErr)
	}
}

// TestWP08_RedactSecretsOnToolInput verifies that utils.RedactSecrets
// redacts common credential patterns that could appear in tool input.
//
// Refs: docs/hufu-generic-task-reliability-mechanisms.md §9, WP-08 (reviewer P1b)
func TestWP08_RedactSecretsOnToolInput(t *testing.T) {
	tests := []struct {
		name  string
		input string
	}{
		{"API key in env var", "export API_KEY=sk-secret-1234567890abcdef"},
		{"Authorization header", `curl -H "Authorization: Bearer sk-secret-token"`},
		{"Private key block", "-----BEGIN RSA PRIVATE KEY-----\nMIIEpAI...\n-----END RSA PRIVATE KEY-----"},
		{"Token in assignment", "TOKEN=abc123secretkey456"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			redacted := utils.RedactSecrets(tt.input)
			// The redacted version should contain [REDACTED] marker
			if !strings.Contains(redacted, "[REDACTED]") {
				t.Errorf("RedactSecrets did not redact credentials in %q: got %q", tt.name, redacted)
			}
			// Verify buildRetryContext with redacted input doesn't leak
			ctx := buildRetryContext(
				FailureExecution, errors.New("failed"), "", "", 0, nil,
				"bash", redacted, "error", true, "output", TaskDef{},
			)
			if strings.Contains(ctx, "sk-secret") || strings.Contains(ctx, "abc123secretkey") || strings.Contains(ctx, "MIIEpAI") {
				t.Errorf("retry context leaked credential from %q: %s", tt.name, ctx)
			}
		})
	}
}
