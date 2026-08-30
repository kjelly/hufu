package team

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/kjelly/hufu/internal/agent"
)

// TestClassifyTaskFailureStructured_NewClasses verifies the WP-05 additions of
// the environment and cancelled failure classes, and that structured inputs
// (context error, resolve findings) take precedence over text matching.
//
// Refs: docs/hufu-generic-task-reliability-mechanisms.md §5, §5.3, WP-05
func TestClassifyTaskFailureStructured_NewClasses(t *testing.T) {
	tests := []struct {
		name string
		in   FailureClassificationInput
		want TaskFailureClass
	}{
		{
			name: "context.Canceled via Err → cancelled",
			in:   FailureClassificationInput{Err: context.Canceled},
			want: FailureCancelled,
		},
		{
			name: "context.Canceled via ContextErr → cancelled",
			in:   FailureClassificationInput{Err: errors.New("agent stopped"), ContextErr: context.Canceled},
			want: FailureCancelled,
		},
		{
			name: "wrapped context.Canceled → cancelled",
			in:   FailureClassificationInput{Err: fmt.Errorf("worker: %w", context.Canceled)},
			want: FailureCancelled,
		},
		{
			name: "executable_unresolved finding → environment",
			in: FailureClassificationInput{
				Err: errors.New("command not found"),
				ResolveFindings: []ContractFinding{
					{Severity: FindingSeverityError, Code: FindingExecutableUnresolved, Field: "verify", Message: "missing binary"},
				},
			},
			want: FailureEnvironment,
		},
		{
			name: "other error finding → contract",
			in: FailureClassificationInput{
				Err: errors.New("verifier malformed"),
				ResolveFindings: []ContractFinding{
					{Severity: FindingSeverityError, Code: FindingVerifierNotAsserting, Field: "verify", Message: "tail || echo"},
				},
			},
			want: FailureContract,
		},
		{
			name: "warning finding does not override text path",
			in: FailureClassificationInput{
				Err: errors.New("verification failed: deliverable missing"),
				ResolveFindings: []ContractFinding{
					{Severity: FindingSeverityWarning, Code: FindingExecutableUnresolved, Field: "verify"},
				},
			},
			want: FailureVerify,
		},
		{
			name: "source=environment detail → environment",
			in:   FailureClassificationInput{Err: errors.New("source=environment | error=command not found")},
			want: FailureEnvironment,
		},
		{
			name: "source=cancelled detail → cancelled",
			in:   FailureClassificationInput{Err: errors.New("source=cancelled | error=user sigint")},
			want: FailureCancelled,
		},
		{
			name: "source=sigint detail → cancelled",
			in:   FailureClassificationInput{Err: errors.New("source=sigint | error=user sigint")},
			want: FailureCancelled,
		},
		{
			name: "source=context_canceled detail → cancelled",
			in:   FailureClassificationInput{Err: errors.New("source=context_canceled | error=parent cancel")},
			want: FailureCancelled,
		},
		{
			name: "text: command not found → environment (fallback)",
			in:   FailureClassificationInput{Err: errors.New("bash: foo: command not found")},
			want: FailureEnvironment,
		},
		{
			name: "text: executable unresolved → environment (fallback)",
			in:   FailureClassificationInput{Err: errors.New("executable unresolved in verify stage")},
			want: FailureEnvironment,
		},
		{
			name: "text: environment → environment (fallback)",
			in:   FailureClassificationInput{Err: errors.New("environment setup failed")},
			want: FailureEnvironment,
		},
		{
			name: "nil err → execution (default)",
			in:   FailureClassificationInput{Err: nil},
			want: FailureExecution,
		},
		{
			name: "deadline exceeded via ContextErr → timeout",
			in:   FailureClassificationInput{Err: errors.New("agent stopped"), ContextErr: context.DeadlineExceeded},
			want: FailureTimeout,
		},
		{
			name: "contract detail still recognized",
			in:   FailureClassificationInput{Err: errors.New("source=contract | error=verifier_not_asserting")},
			want: FailureContract,
		},
		{
			name: "execution fallback",
			in:   FailureClassificationInput{Err: errors.New("agent failed: exit code 1")},
			want: FailureExecution,
		},
		{
			name: "verify exit code non-zero → verification (overrides text)",
			in: FailureClassificationInput{
				Err:            errors.New("agent failed: exit code 1"),
				ExitCode:       2,
				ExitCodeSource: ExitCodeSourceVerify,
			},
			want: FailureVerify,
		},
		{
			name: "verify exit code non-zero beats execution text",
			in: FailureClassificationInput{
				Err:            errors.New("agent failed: exit code 1"),
				ExitCode:       1,
				ExitCodeSource: ExitCodeSourceVerify,
			},
			want: FailureVerify,
		},
		{
			name: "verify exit code zero does not override (ambiguous)",
			in: FailureClassificationInput{
				Err:            errors.New("agent failed: exit code 1"),
				ExitCode:       0,
				ExitCodeSource: ExitCodeSourceVerify,
			},
			want: FailureExecution,
		},
		{
			name: "verify exit code unknown (-1) does not override",
			in: FailureClassificationInput{
				Err:            errors.New("agent failed: exit code 1"),
				ExitCode:       -1,
				ExitCodeSource: ExitCodeSourceVerify,
			},
			want: FailureExecution,
		},
		{
			name: "worker exit code non-zero does not override (not verify source)",
			in: FailureClassificationInput{
				Err:            errors.New("agent failed: exit code 1"),
				ExitCode:       1,
				ExitCodeSource: ExitCodeSourceWorker,
			},
			want: FailureExecution,
		},
		{
			name: "unspecified exit code source does not override",
			in: FailureClassificationInput{
				Err:            errors.New("agent failed: exit code 1"),
				ExitCode:       1,
				ExitCodeSource: "",
			},
			want: FailureExecution,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ClassifyTaskFailureStructured(tt.in)
			if got != tt.want {
				t.Errorf("ClassifyTaskFailureStructured() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestClassifyTaskFailure_CancelledTakesPrecedenceOverTimeout verifies that a
// wrapped context.Canceled is classified as cancelled even when the error
// message contains timeout-like text, because §5.3 requires cancelled to be
// separated before any other classification.
//
// Refs: docs/hufu-generic-task-reliability-mechanisms.md §5.3, WP-05
func TestClassifyTaskFailure_CancelledTakesPrecedenceOverTimeout(t *testing.T) {
	// A wrapped context.Canceled whose message mentions "deadline" must still
	// be cancelled, not timeout — the structured errors.Is check wins.
	err := fmt.Errorf("context canceled while waiting for deadline: %w", context.Canceled)
	got := ClassifyTaskFailureStructured(FailureClassificationInput{Err: err})
	if got != FailureCancelled {
		t.Errorf("expected cancelled, got %q", got)
	}
}

// TestClassifyTaskFailure_LegacyWrapperMatchesFallback verifies the legacy
// single-argument classifyTaskFailure wrapper delegates to the structured
// classifier and produces the fallback (text-matching) result for errors with
// no structured metadata.
//
// Refs: docs/hufu-generic-task-reliability-mechanisms.md §5, WP-05
func TestClassifyTaskFailure_LegacyWrapperMatchesFallback(t *testing.T) {
	tests := []struct {
		err  error
		want TaskFailureClass
	}{
		{errors.New("contract preflight failed: verify"), FailureContract},
		{errors.New("protocol failure: empty output"), FailureProtocol},
		{errors.New("verification failed"), FailureVerify},
		{errors.New("policy blocked"), FailurePolicy},
		{errors.New("context deadline exceeded"), FailureTimeout},
		{errors.New("command not found"), FailureEnvironment},
		{errors.New("agent failed: exit 1"), FailureExecution},
		{context.Canceled, FailureCancelled},
	}
	for _, tt := range tests {
		got := classifyTaskFailure(tt.err)
		if got != tt.want {
			t.Errorf("classifyTaskFailure(%v) = %q, want %q", tt.err, got, tt.want)
		}
	}
}

// TestWP02ContractCasesStillPass verifies the WP-02 test cases (which pin the
// contract classification) still pass through the new structured classifier.
//
// Refs: docs/hufu-generic-task-reliability-mechanisms.md §5, WP-02, WP-05
func TestWP02ContractCasesStillPass(t *testing.T) {
	cases := []struct {
		err  error
		want TaskFailureClass
	}{
		{errors.New("source=contract | error=verifier contract error: verify (verifier_not_asserting): ..."), FailureContract},
		{errors.New("contract preflight failed: verify (verifier_invalid): missing path"), FailureContract},
		{errors.New("agent failed: exit code 1"), FailureExecution},
		{errors.New("verification failed: deliverable missing"), FailureVerify},
		{errors.New("context deadline exceeded"), FailureTimeout},
	}
	for _, tc := range cases {
		got := ClassifyTaskFailureStructured(FailureClassificationInput{Err: tc.err})
		if got != tc.want {
			t.Errorf("classify(%q) = %q, want %q", tc.err, got, tc.want)
		}
	}
}

// TestPersistFailure_CancelledExcludedFromFingerprint verifies that a
// cancelled failure does not record a failure fingerprint, does not increment
// the anti-thrashing Counts, and does not emit a failure_fingerprint event —
// satisfying §5.3's requirement that cancelled failures be excluded from
// retry, failure-class statistics and the anti-thrashing fingerprint.
//
// Refs: docs/hufu-generic-task-reliability-mechanisms.md §5.3, WP-05
func TestPersistFailure_CancelledExcludedFromFingerprint(t *testing.T) {
	workspace := t.TempDir()
	c := &Coordinator{
		session:     &TeamSession{Workspace: workspace, Config: agent.TeamConfig{Name: "wp05"}},
		sessionData: NewSession(),
		taskTracker: NewTaskTracker(),
	}
	c.taskTracker.TodoList().onChange = func() { c.saveCheckpoint() }

	es, err := NewEventStore(workspace, "run-wp05", "sess-wp05")
	if err != nil {
		t.Fatalf("NewEventStore: %v", err)
	}
	c.eventStore = es

	items := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "do work"}})
	todoID := items[0].ID

	// Persist a cancelled failure (context.Canceled via the sigint source).
	c.PersistFailure("worker", "do work", todoID, c.FailureDetail(context.Canceled, FailureSourceSigint))

	// Read the event store back and assert no fingerprint events were emitted
	// for the cancelled failure, and a task_cancelled event was emitted.
	events, _ := es.ReadEvents()
	foundCancelled := 0
	foundFailed := 0
	for _, e := range events {
		switch e.Type {
		case "failure_fingerprint":
			t.Errorf("failure_fingerprint event emitted for cancelled failure; cancelled must be excluded from fingerprint path (§5.3)")
		case "repeated_failure_fingerprint":
			t.Errorf("repeated_failure_fingerprint event emitted for cancelled failure (§5.3)")
		case "anti_thrashing_limit_reached":
			t.Errorf("anti_thrashing_limit_reached event emitted for cancelled failure (§5.3)")
		case "task_cancelled":
			foundCancelled++
		case "task_failed":
			foundFailed++
		}
	}
	if foundCancelled != 1 || foundFailed != 0 {
		t.Errorf("cancelled terminal events = cancelled:%d failed:%d, want cancelled:1 failed:0", foundCancelled, foundFailed)
	}

	// RepeatedFailureFingerprints must be zero — cancelled failures do not
	// increment the Counts map that feeds it.
	m := c.Metrics()
	if m.RepeatedFailureFingerprints != 0 {
		t.Errorf("RepeatedFailureFingerprints = %d, want 0 (cancelled excluded)", m.RepeatedFailureFingerprints)
	}

	// Persisting the same cancelled failure twice must not increment Counts.
	c.PersistFailure("worker", "do work", todoID, c.FailureDetail(context.Canceled, FailureSourceSigint))
	m2 := c.Metrics()
	if m2.RepeatedFailureFingerprints != 0 {
		t.Errorf("RepeatedFailureFingerprints after 2nd cancelled = %d, want 0", m2.RepeatedFailureFingerprints)
	}
}

// TestRecordRetry_CancelledNotCounted verifies the §5.3 invariant at the
// recordRetry level: the guarded call site must never invoke recordRetry for
// a cancelled class. This test documents the invariant by calling recordRetry
// directly and asserting the counter reflects only non-cancelled calls; the
// guard lives in the retry-loop call site (coordinator_task_run.go).
//
// Refs: docs/hufu-generic-task-reliability-mechanisms.md §5.3, WP-05
func TestRecordRetry_CancelledNotCounted(t *testing.T) {
	c := &Coordinator{}
	// Simulate the guarded call site: only non-cancelled classes reach
	// recordRetry. We assert recordRetry itself is correct for the classes
	// that DO reach it, and document that cancelled is filtered upstream.
	c.recordRetry(FailureExecution)
	c.recordRetry(FailureEnvironment)
	m := c.Metrics()
	if m.RetriesByFailureClass[FailureExecution] != 1 {
		t.Errorf("RetriesByFailureClass[execution] = %d, want 1", m.RetriesByFailureClass[FailureExecution])
	}
	if m.RetriesByFailureClass[FailureEnvironment] != 1 {
		t.Errorf("RetriesByFailureClass[environment] = %d, want 1", m.RetriesByFailureClass[FailureEnvironment])
	}
	if m.RetriesByFailureClass[FailureCancelled] != 0 {
		t.Errorf("RetriesByFailureClass[cancelled] = %d, want 0 (cancelled is filtered before recordRetry)", m.RetriesByFailureClass[FailureCancelled])
	}
}

// TestIsCancelledClass verifies the helper used by the retry loop and
// PersistFailure to gate fingerprint/retry statistics.
//
// Refs: docs/hufu-generic-task-reliability-mechanisms.md §5.3, WP-05
func TestIsCancelledClass(t *testing.T) {
	if !IsCancelledClass(FailureCancelled) {
		t.Errorf("IsCancelledClass(FailureCancelled) = false, want true")
	}
	if IsCancelledClass(FailureExecution) {
		t.Errorf("IsCancelledClass(FailureExecution) = true, want false")
	}
	if IsCancelledClass(FailureTimeout) {
		t.Errorf("IsCancelledClass(FailureTimeout) = true, want false")
	}
}

// TestClassifyTaskFailure_EnvironmentFromTextFallback verifies the text
// fallback recognises the "command not found" pattern observed in real
// shell output, so an environment failure is not misclassified as execution
// when no structured findings are available.
//
// Refs: docs/hufu-generic-task-reliability-mechanisms.md §5.1, §11, WP-05
func TestClassifyTaskFailure_EnvironmentFromTextFallback(t *testing.T) {
	err := errors.New("bash: nonexistent-cmd: command not found")
	got := ClassifyTaskFailureStructured(FailureClassificationInput{Err: err})
	if got != FailureEnvironment {
		t.Errorf("classify(%q) = %q, want environment", err, got)
	}
}

// TestClassifyTaskFailure_ExitCodePrecedence verifies that a non-zero
// verify-command exit code classifies as FailureVerify even when the error
// message text would otherwise fall through to FailureExecution — the
// structured exit-code evidence takes precedence over the text fallback
// (§5, §5.2, WP-05 reviewer P1).
//
// Refs: docs/hufu-generic-task-reliability-mechanisms.md §5, §5.2, WP-05
func TestClassifyTaskFailure_ExitCodePrecedence(t *testing.T) {
	// The error text says "exit code 1" (execution-like), but the structured
	// verify exit code is non-zero. Evidence wins → verification.
	err := errors.New("agent failed: exit code 1")
	got := ClassifyTaskFailureStructured(FailureClassificationInput{
		Err:            err,
		ExitCode:       1,
		ExitCodeSource: ExitCodeSourceVerify,
	})
	if got != FailureVerify {
		t.Errorf("classify with verify exit=1 = %q, want verification (evidence over text)", got)
	}
}

// cancellableWorkerAgent returns context.Canceled on the first Stream call
// and a successful result on subsequent calls, recording how many times
// Stream was invoked. This drives the retry-loop cancellation guard test
// (reviewer P2).
type cancellableWorkerAgent struct {
	calls int
}

func (a *cancellableWorkerAgent) Generate(ctx context.Context, call fantasy.AgentCall) (*fantasy.AgentResult, error) {
	return a.Stream(ctx, fantasy.AgentStreamCall{Prompt: call.Prompt, Messages: call.Messages})
}

func (a *cancellableWorkerAgent) Stream(ctx context.Context, call fantasy.AgentStreamCall) (*fantasy.AgentResult, error) {
	a.calls++
	if a.calls == 1 {
		// Return partial output alongside context.Canceled so evidence is
		// captured (computeEvidenceComplete requires steps or output).
		return &fantasy.AgentResult{
			Response: fantasy.Response{
				Content: fantasy.ResponseContent{
					fantasy.TextContent{Text: "partial work before cancellation"},
				},
			},
		}, context.Canceled
	}
	return &fantasy.AgentResult{
		Response: fantasy.Response{
			Content: fantasy.ResponseContent{
				fantasy.TextContent{Text: "succeeded on retry"},
			},
		},
	}, nil
}

// alwaysCancelWorkerAgent returns context.Canceled on every Stream call.
type alwaysCancelWorkerAgent struct {
	calls int
}

func (a *alwaysCancelWorkerAgent) Generate(ctx context.Context, call fantasy.AgentCall) (*fantasy.AgentResult, error) {
	return a.Stream(ctx, fantasy.AgentStreamCall{Prompt: call.Prompt, Messages: call.Messages})
}

func (a *alwaysCancelWorkerAgent) Stream(ctx context.Context, call fantasy.AgentStreamCall) (*fantasy.AgentResult, error) {
	a.calls++
	return nil, context.Canceled
}

// newWP05RetryTestCoordinator builds a Coordinator wired for retry-loop
// integration tests: a real task tracker, a status reporter that records
// event types, a workerAgentOverride, and enough session state to run
// executeTask without a provider.
func newWP05RetryTestCoordinator(t *testing.T, worker fantasy.Agent) (*Coordinator, *[]string) {
	t.Helper()
	workspace := t.TempDir()
	t.Cleanup(func() { time.Sleep(100 * time.Millisecond) })
	var events []string
	c := &Coordinator{
		session: &TeamSession{
			Workspace: workspace,
			Config: agent.TeamConfig{
				Name:       "wp05-retry",
				Timeout:    30,
				MaxRetries: 3,
			},
			Agents: map[string]*agent.AgentDef{
				"worker": {Name: "worker", Role: "worker", Generation: agent.GenerationParams{Model: "test"}, MaxRetries: -1},
			},
		},
		sessionTime:     time.Now(),
		taskTracker:     NewTaskTracker(),
		reportStatus:    func(e StatusEvent) { events = append(events, e.Type) },
		taskResultCache: make(map[string][]cachedTaskEntry),
		executionRunID:  "run-wp05-retry",
	}
	c.workerAgentOverride = worker
	return c, &events
}

// TestExecuteTask_CancelledPreviousAttempt_NoRetryRecordedForCancelled drives
// the actual executeTask retry loop with a worker that returns context.Canceled.
// Cancellation has disposition none, so the task must stop after the first
// call and must not record a cancelled retry statistic.
//
// Refs: docs/hufu-generic-task-reliability-mechanisms.md §5.3, WP-05
func TestExecuteTask_CancelledPreviousAttempt_NoRetryRecordedForCancelled(t *testing.T) {
	worker := &cancellableWorkerAgent{}
	c, _ := newWP05RetryTestCoordinator(t, worker)

	items := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "do work"}})
	todoID := items[0].ID

	_, err := c.executeTask(context.Background(), TaskDef{
		Agent: "worker",
		Goal:  "do work",
	}, todoID)
	if err == nil {
		t.Fatal("expected cancelled task to stop without retry")
	}
	if worker.calls != 1 {
		t.Fatalf("worker calls = %d, want 1 (cancelled tasks must not retry)", worker.calls)
	}

	// The cancelled failure on attempt 1 must NOT be counted in retry
	// statistics. RetriesByFailureClass must have no cancelled entry.
	m := c.Metrics()
	if m.RetriesByFailureClass[FailureCancelled] != 0 {
		t.Errorf("RetriesByFailureClass[cancelled] = %d, want 0 (guard must skip recordRetry for cancelled, §5.3)", m.RetriesByFailureClass[FailureCancelled])
	}
}

// TestExecuteTask_CancelledPreviousAttempt_GuardBlocksRecordRetry verifies
// the metric boundary directly and through executeTask: cancellation must not
// enter retry statistics even if a future call site accidentally calls
// recordRetry.
//
// Refs: docs/hufu-generic-task-reliability-mechanisms.md §5.3, WP-05
func TestExecuteTask_CancelledPreviousAttempt_GuardBlocksRecordRetry(t *testing.T) {
	// The metric helper itself rejects cancelled classes so every caller keeps
	// the §5.3 invariant.
	c := &Coordinator{}
	c.recordRetry(FailureCancelled)
	if got := c.Metrics().RetriesByFailureClass[FailureCancelled]; got != 0 {
		t.Fatalf("recordRetry(FailureCancelled) = %d, want 0", got)
	}

	// Now drive executeTask with a cancelled worker on a fresh coordinator.
	worker := &cancellableWorkerAgent{}
	c2, _ := newWP05RetryTestCoordinator(t, worker)
	items := c2.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "do work"}})
	if _, err := c2.executeTask(context.Background(), TaskDef{Agent: "worker", Goal: "do work"}, items[0].ID); err == nil {
		t.Fatal("expected cancelled task to stop without retry")
	}
	m := c2.Metrics()
	if m.RetriesByFailureClass[FailureCancelled] != 0 {
		t.Errorf("RetriesByFailureClass[cancelled] = %d after executeTask, want 0 (call-site guard must block recordRetry for cancelled, §5.3)", m.RetriesByFailureClass[FailureCancelled])
	}
}

// TestExecuteTask_ParentContextCancelled_NoSubsequentDispatch verifies that
// when the parent context is cancelled before the retry loop continues, no
// subsequent worker dispatch happens and the cancelled failure is not
// recorded as a retry (reviewer P2: "ideally no subsequent dispatch after
// parent cancellation").
//
// Refs: docs/hufu-generic-task-reliability-mechanisms.md §5.3, WP-05
func TestExecuteTask_ParentContextCancelled_NoSubsequentDispatch(t *testing.T) {
	worker := &alwaysCancelWorkerAgent{}
	c, _ := newWP05RetryTestCoordinator(t, worker)
	items := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "do work"}})
	todoID := items[0].ID

	// Use a parent context that is already cancelled. The worker returns
	// context.Canceled on every attempt; the loop's `parentCtx.Err() != nil`
	// check breaks before a second dispatch.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := c.executeTask(ctx, TaskDef{Agent: "worker", Goal: "do work"}, todoID)
	if err == nil {
		t.Fatalf("expected error from cancelled execution")
	}

	// Only one worker dispatch should have occurred — the loop breaks at the
	// `parentCtx.Err() != nil` check before reaching attempt 2.
	if worker.calls != 1 {
		t.Errorf("worker dispatched %d time(s), want 1 (parent cancel must break before retry)", worker.calls)
	}

	// No retry statistics at all (the `attempt > 1` block never ran).
	m := c.Metrics()
	if len(m.RetriesByFailureClass) != 0 {
		t.Errorf("RetriesByFailureClass = %v, want empty (no retry classification ran)", m.RetriesByFailureClass)
	}
}

// TestVerifyResultForTodo_ReadsStoredVerificationResult verifies the helpers
// that thread the verify exit code and environment evidence into
// FailureClassificationInput read the stored VerificationResult from the todo
// item, returning -1 / nil when absent.
//
// Refs: docs/hufu-generic-task-reliability-mechanisms.md §5, §5.1, §5.2, WP-05
func TestVerifyResultForTodo_ReadsStoredVerificationResult(t *testing.T) {
	c := &Coordinator{taskTracker: NewTaskTracker()}
	items := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "do work"}})
	todoID := items[0].ID

	// No verification result stored → nil / -1 (unknown).
	if vr := verifyResultForTodo(c, todoID); vr != nil {
		t.Errorf("verifyResultForTodo with no result = %v, want nil", vr)
	}
	if got := exitCodeFromVerifyResult(verifyResultForTodo(c, todoID)); got != -1 {
		t.Errorf("exitCode with no result = %d, want -1", got)
	}

	// Store a non-zero exit code and read it back.
	if err := c.taskTracker.TodoList().SetVerificationResult(todoID, &VerificationResult{ExitCode: 2}); err != nil {
		t.Fatalf("SetVerificationResult: %v", err)
	}
	if got := exitCodeFromVerifyResult(verifyResultForTodo(c, todoID)); got != 2 {
		t.Errorf("exitCode after set = %d, want 2", got)
	}

	// Unknown todoID → nil / -1.
	if vr := verifyResultForTodo(c, "nonexistent"); vr != nil {
		t.Errorf("verifyResultForTodo(unknown) = %v, want nil", vr)
	}
	if got := exitCodeFromVerifyResult(verifyResultForTodo(c, "nonexistent")); got != -1 {
		t.Errorf("exitCode(unknown) = %d, want -1", got)
	}

	// Nil coordinator → nil / -1.
	if vr := verifyResultForTodo(nil, todoID); vr != nil {
		t.Errorf("verifyResultForTodo(nil) = %v, want nil", vr)
	}
	if got := exitCodeFromVerifyResult(verifyResultForTodo(nil, todoID)); got != -1 {
		t.Errorf("exitCode(nil) = %d, want -1", got)
	}
}

// TestEnvironmentFindingsFromVerifyResult_CommandNotFound verifies that a
// verify result whose stderr/stdout contains a command-not-found signal yields
// an executable_unresolved finding, so the structured classifier classifies
// the failure as FailureEnvironment rather than letting the non-zero exit code
// override it to FailureVerify (reviewer P1, §5.1).
//
// The detector is deliberately narrow: only unambiguous shell/executable
// diagnostics match. Generic "not found" / "no such file or directory" phrases
// do NOT match, because they appear in ordinary missing-artifact verifier
// output and must remain FailureVerify (§5.1: distinguish command/shell
// failures from assertion failures before classifying).
//
// Refs: docs/hufu-generic-task-reliability-mechanisms.md §5.1, WP-05
func TestEnvironmentFindingsFromVerifyResult_CommandNotFound(t *testing.T) {
	tests := []struct {
		name string
		vr   *VerificationResult
		want bool
	}{
		{
			name: "stderr command not found → environment finding",
			vr:   &VerificationResult{ExitCode: 127, Stderr: "bash: nonexistent-cmd: command not found"},
			want: true,
		},
		{
			name: "executable file not found → environment finding",
			vr:   &VerificationResult{ExitCode: 127, Stderr: "executable file not found in $PATH"},
			want: true,
		},
		{
			name: "executable unresolved → environment finding",
			vr:   &VerificationResult{ExitCode: 127, Stderr: "verify stage executable unresolved"},
			want: true,
		},
		// Reviewer P1: these generic phrases must NOT be environment failures.
		{
			name: "artifact not found (assertion) → NOT environment (stay verification)",
			vr:   &VerificationResult{ExitCode: 1, Stderr: "assertion failed: artifact not found"},
			want: false,
		},
		{
			name: "grep missing input file → NOT environment (stay verification)",
			vr:   &VerificationResult{ExitCode: 2, Stderr: "grep: report.json: No such file or directory"},
			want: false,
		},
		{
			name: "sh missing path exec → NOT environment (generic no-such-file is ambiguous)",
			vr:   &VerificationResult{ExitCode: 127, Stderr: "sh: /missing/path: No such file or directory"},
			want: false,
		},
		{
			name: "plain 'not found' without command context → NOT environment",
			vr:   &VerificationResult{ExitCode: 1, Stdout: "target not found"},
			want: false,
		},
		{
			name: "plain verification failure (no env signal) → no environment finding",
			vr:   &VerificationResult{ExitCode: 1, Stderr: "assertion failed: expected file to exist"},
			want: false,
		},
		{
			name: "nil result → no finding",
			vr:   nil,
			want: false,
		},
		{
			name: "empty stderr/stdout → no finding",
			vr:   &VerificationResult{ExitCode: 1},
			want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			findings := environmentFindingsFromVerifyResult(tt.vr)
			got := len(findings) > 0 && findings[0].Code == FindingExecutableUnresolved
			if got != tt.want {
				t.Errorf("environmentFindingsFromVerifyResult got environment=%v, want %v", got, tt.want)
			}
		})
	}
}

// TestClassifyTaskFailure_ExitCodeDoesNotOverrideEnvironment verifies the
// precedence fix (reviewer P1): when a verify result carries both a non-zero
// exit code AND a command-not-found signal, the structured classifier
// classifies as FailureEnvironment (environment evidence wins over exit code,
// §5.1), not FailureVerify.
//
// Refs: docs/hufu-generic-task-reliability-mechanisms.md §5.1, WP-05
func TestClassifyTaskFailure_ExitCodeDoesNotOverrideEnvironment(t *testing.T) {
	envFindings := environmentFindingsFromVerifyResult(&VerificationResult{
		ExitCode: 127,
		Stderr:   "bash: nonexistent-cmd: command not found",
	})
	if len(envFindings) == 0 {
		t.Fatalf("expected environment findings from command-not-found verify result")
	}
	got := ClassifyTaskFailureStructured(FailureClassificationInput{
		Err:             errors.New("deliverable verification failed: exit code 127"),
		ExitCode:        127,
		ExitCodeSource:  ExitCodeSourceVerify,
		ResolveFindings: envFindings,
	})
	if got != FailureEnvironment {
		t.Errorf("classify with command-not-found verify result = %q, want environment (§5.1: environment evidence beats exit code)", got)
	}
}

// TestExecuteTask_ClearsStaleVerifyResultBetweenRetries verifies the
// stale-result fix (reviewer P2): when attempt 1 runs verification (sets
// VerifyResult with exit 1) and fails, the retry-classification block at the
// start of attempt 2 reads that result (classifying attempt 1's failure as
// FailureVerify, correctly) and then CLEARS it. Attempt 2 then fails with a
// non-verify error before reaching verification. At the start of attempt 3,
// the retry-classification block sees nil VerifyResult (cleared) and falls
// back to the error text, classifying attempt 2's failure as FailureExecution
// — NOT FailureVerify from a stale exit code.
//
// Without the fix, attempt 2's classification would read attempt 1's stale
// exit code and misclassify "connection reset" as FailureVerify, inflating
// the FailureVerify retry count to 2.
//
// Refs: docs/hufu-generic-task-reliability-mechanisms.md §5, §5.1, WP-05
func TestExecuteTask_ClearsStaleVerifyResultBetweenRetries(t *testing.T) {
	worker := &staleVerifyWorkerAgent{}
	c, _ := newWP05RetryTestCoordinator(t, worker)
	items := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "do work"}})
	todoID := items[0].ID

	// Attach a verify command so attempt 1 runs verification and sets a
	// VerifyResult (exit 1, because /nonexistent-artifact doesn't exist).
	// The task will ultimately fail (verify never passes), but we assert the
	// retry-classification metrics, not the final task status.
	task := TaskDef{
		Agent:  "worker",
		Goal:   "do work",
		Verify: "test -f /nonexistent-artifact",
	}
	_, err := c.executeTask(context.Background(), task, todoID)
	if err == nil {
		t.Fatal("expected executeTask to fail (verify never passes), got nil error")
	}
	if worker.calls != 4 {
		t.Fatalf("expected 4 worker calls (initial attempt plus three retries), got %d", worker.calls)
	}

	m := c.Metrics()
	// Attempt 1's failure is a genuine verify failure (exit 1) → exactly 1
	// FailureVerify retry. Attempt 2's failure ("connection reset") must be
	// FailureExecution, not a second FailureVerify from the stale exit code.
	if got := m.RetriesByFailureClass[FailureVerify]; got != 2 {
		t.Errorf("FailureVerify retries = %d, want 2 (attempts 1 and 3/4 genuinely fail verification; attempt 2 must not inherit stale exit code, §5.1)", got)
	}
	if got := m.RetriesByFailureClass[FailureExecution]; got != 1 {
		t.Errorf("FailureExecution retries = %d, want 1 (attempt 2's connection-reset failure, classified via text fallback after stale result cleared)", got)
	}
}

// staleVerifyWorkerAgent drives a 3-attempt scenario:
//   - Attempt 1: returns text output (verification runs and fails with exit 1
//     because the artifact doesn't exist).
//   - Attempt 2: returns a plain error (not a verify error) so it fails before
//     verification. The retry classification for this failure must not use
//     attempt 1's stale verify exit code.
//   - Attempt 3: succeeds with text output.
type staleVerifyWorkerAgent struct {
	calls int
}

func (a *staleVerifyWorkerAgent) Generate(ctx context.Context, call fantasy.AgentCall) (*fantasy.AgentResult, error) {
	return a.Stream(ctx, fantasy.AgentStreamCall{Prompt: call.Prompt, Messages: call.Messages})
}

func (a *staleVerifyWorkerAgent) Stream(ctx context.Context, call fantasy.AgentStreamCall) (*fantasy.AgentResult, error) {
	a.calls++
	switch a.calls {
	case 1:
		return &fantasy.AgentResult{
			Response: fantasy.Response{Content: fantasy.ResponseContent{
				fantasy.TextContent{Text: "attempt 1 output"},
			}},
		}, nil
	case 2:
		// Return partial output alongside a plain error so evidence is
		// captured (computeEvidenceComplete requires steps or output).
		// The error is not a verify error so attempt 2 fails before
		// any verification runs. The retry classification for this failure
		// must not use attempt 1's stale verify exit code.
		return &fantasy.AgentResult{
			Response: fantasy.Response{Content: fantasy.ResponseContent{
				fantasy.TextContent{Text: "attempt 2 partial output before error"},
			}},
		}, errors.New("agent failed: connection reset")
	default:
		return &fantasy.AgentResult{
			Response: fantasy.Response{Content: fantasy.ResponseContent{
				fantasy.TextContent{Text: "attempt 3 success"},
			}},
		}, nil
	}
}

// TestVerifyResultClearedAfterRetryClassification is a focused unit test that
// verifies the retry-classification block clears the todo's VerifyResult after
// reading it. It directly exercises the clear by pre-setting a VerifyResult,
// then simulating the retry-classification path via the coordinator's
// recordRetry-classify-clear sequence (reviewer P2).
//
// Refs: docs/hufu-generic-task-reliability-mechanisms.md §5, §5.1, WP-05
func TestVerifyResultClearedAfterRetryClassification(t *testing.T) {
	c := &Coordinator{taskTracker: NewTaskTracker()}
	items := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "do work"}})
	todoID := items[0].ID

	// Pre-set a stale verify result (as if attempt 1 ran verification).
	if err := c.taskTracker.TodoList().SetVerificationResult(todoID, &VerificationResult{ExitCode: 1}); err != nil {
		t.Fatalf("SetVerificationResult: %v", err)
	}
	if vr := verifyResultForTodo(c, todoID); vr == nil {
		t.Fatalf("precondition: VerifyResult not set")
	}

	// Simulate the retry-classification block: read, classify, clear.
	vr := verifyResultForTodo(c, todoID)
	class := ClassifyTaskFailureStructured(FailureClassificationInput{
		Err:            errors.New("deliverable verification failed: exit code 1"),
		ExitCode:       exitCodeFromVerifyResult(vr),
		ExitCodeSource: ExitCodeSourceVerify,
	})
	if class != FailureVerify {
		t.Errorf("classification with exit=1 = %q, want verification", class)
	}
	// The block clears the result after classification.
	if err := c.taskTracker.TodoList().SetVerificationResult(todoID, nil); err != nil {
		t.Fatalf("clear VerifyResult: %v", err)
	}

	// Now the todo's VerifyResult must be nil — the stale result is gone.
	if vr := verifyResultForTodo(c, todoID); vr != nil {
		t.Errorf("VerifyResult not cleared after retry classification = %v, want nil (§5.1: stale result must not leak to next attempt)", vr)
	}
}

// TestExecuteTask_RetainsVerificationEvidenceOnReceiptAfterClear verifies the
// evidence-retention fix (reviewer P1): when the retry block clears the
// todo-wide VerifyResult, the prior attempt's verification evidence (command,
// exit code, stdout, stderr) is retained on that attempt's ExecutionReceipt so
// forensics can still access it (§5, §9 evidence retention). The test drives
// a 3-attempt scenario: attempt 1 runs verification (exit 1, with stdout/stderr),
// attempt 2 fails before verification, attempt 3 runs verification. After the
// run, the attempt-1 ExecutionReceipt must carry the attempt-1 VerifyResult.
//
// Refs: docs/hufu-generic-task-reliability-mechanisms.md §5, §9, WP-05
func TestExecuteTask_RetainsVerificationEvidenceOnReceiptAfterClear(t *testing.T) {
	worker := &staleVerifyWorkerAgent{}
	c, _ := newWP05RetryTestCoordinator(t, worker)
	items := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "do work"}})
	todoID := items[0].ID

	task := TaskDef{
		Agent:  "worker",
		Goal:   "do work",
		Verify: "test -f /nonexistent-artifact",
	}
	_, _ = c.executeTask(context.Background(), task, todoID)
	if worker.calls != 4 {
		t.Fatalf("expected 4 worker calls (initial attempt plus three retries), got %d", worker.calls)
	}

	// The todo-wide VerifyResult slot may hold attempt 3's result (the final
	// verification), but the key assertion is that attempt 1's verification
	// evidence is retained on its ExecutionReceipt even though the slot was
	// cleared at the start of attempt 2.
	items2 := c.taskTracker.TodoList().Items()
	var attempt1Receipt *ExecutionReceipt
	for _, item := range items2 {
		if item.ID != todoID {
			continue
		}
		for i := range item.ExecutionReceipts {
			if item.ExecutionReceipts[i].Attempt == 1 {
				attempt1Receipt = &item.ExecutionReceipts[i]
				break
			}
		}
		break
	}
	if attempt1Receipt == nil {
		t.Fatal("attempt 1 ExecutionReceipt not found")
	}
	if attempt1Receipt.VerifyResult == nil {
		t.Fatal("attempt 1 ExecutionReceipt.VerifyResult is nil; verification evidence was discarded when the todo-wide slot was cleared (§5, §9 evidence retention requires per-attempt evidence to survive)")
	}
	// The retained evidence must carry the verify command's exit code and
	// output from attempt 1.
	if attempt1Receipt.VerifyResult.ExitCode != 1 {
		t.Errorf("attempt 1 receipt VerifyResult.ExitCode = %d, want 1 (test -f on nonexistent artifact)", attempt1Receipt.VerifyResult.ExitCode)
	}
	if attempt1Receipt.VerifyResult.Command == "" {
		t.Errorf("attempt 1 receipt VerifyResult.Command is empty; forensics needs the verify command")
	}
}

// TestUpdateReceiptVerifyResult_AttachesToMatchingAttempt verifies the
// TodoList.UpdateReceiptVerifyResult method attaches a verification result to
// the receipt matching (runID, taskID, attempt) and leaves other attempts
// unchanged.
//
// Refs: docs/hufu-generic-task-reliability-mechanisms.md §5, §9, WP-05
func TestUpdateReceiptVerifyResult_AttachesToMatchingAttempt(t *testing.T) {
	tl := NewTaskTracker().TodoList()
	items := tl.AddBatch([]TodoSpec{{Agent: "worker", Desc: "do work"}})
	todoID := items[0].ID

	// Seed two receipts (attempt 1 and 2) without verify results, same run.
	zero1, zero2 := 0, 0
	tl.SetExecutionReceipt(todoID, &ExecutionReceipt{RunID: "run-a", TaskID: todoID, Attempt: 1, ExitCode: &zero1})
	tl.SetExecutionReceipt(todoID, &ExecutionReceipt{RunID: "run-a", TaskID: todoID, Attempt: 2, ExitCode: &zero2})

	// Attach a verify result to attempt 1 of run-a.
	vr := &VerificationResult{Command: "test -f x", ExitCode: 1, Stderr: "missing"}
	tl.UpdateReceiptVerifyResult("run-a", todoID, 1, vr)

	for _, item := range tl.Items() {
		if item.ID != todoID {
			continue
		}
		for _, r := range item.ExecutionReceipts {
			if r.Attempt == 1 && r.RunID == "run-a" {
				if r.VerifyResult == nil || r.VerifyResult.ExitCode != 1 || r.VerifyResult.Command != "test -f x" {
					t.Errorf("attempt 1 receipt VerifyResult = %v, want exit 1 / command 'test -f x'", r.VerifyResult)
				}
			} else if r.Attempt == 2 {
				if r.VerifyResult != nil {
					t.Errorf("attempt 2 receipt VerifyResult = %v, want nil (only attempt 1 should be updated)", r.VerifyResult)
				}
			}
		}
		break
	}

	// Nil vr or unknown attempt/run is a no-op.
	tl.UpdateReceiptVerifyResult("run-a", todoID, 99, vr)
	tl.UpdateReceiptVerifyResult("run-a", todoID, 1, nil)
	tl.UpdateReceiptVerifyResult("nonexistent-run", todoID, 1, vr)
	// Attempt 1 of run-a should still have its verify result (nil vr / wrong run are no-ops, not clears).
	for _, item := range tl.Items() {
		if item.ID != todoID {
			continue
		}
		for _, r := range item.ExecutionReceipts {
			if r.Attempt == 1 && r.RunID == "run-a" && r.VerifyResult == nil {
				t.Errorf("attempt 1 receipt VerifyResult cleared by nil vr / wrong run (should be no-op)")
			}
		}
		break
	}
}

// TestUpdateReceiptVerifyResult_MatchesRunIDNotJustAttempt is the
// crash-resume regression requested by the reviewer (P1). After crash-resume,
// a todo can carry a prior run's attempt-1 receipt alongside a new run's
// attempt-1 receipt. UpdateReceiptVerifyResult must match on (RunID, Attempt)
// so attaching the new run's verification evidence updates the new run's
// receipt, not the prior run's.
//
// Refs: docs/hufu-generic-task-reliability-mechanisms.md §5, §9, WP-05
func TestUpdateReceiptVerifyResult_MatchesRunIDNotJustAttempt(t *testing.T) {
	tl := NewTaskTracker().TodoList()
	items := tl.AddBatch([]TodoSpec{{Agent: "worker", Desc: "do work"}})
	todoID := items[0].ID

	// Seed an attempt-1 receipt from a prior run (crash-resume scenario).
	zero1, zero2 := 0, 0
	tl.SetExecutionReceipt(todoID, &ExecutionReceipt{RunID: "run-old", TaskID: todoID, Attempt: 1, ExitCode: &zero1})
	// Seed an attempt-1 receipt from the current run.
	tl.SetExecutionReceipt(todoID, &ExecutionReceipt{RunID: "run-new", TaskID: todoID, Attempt: 1, ExitCode: &zero2})

	// Attach the current run's verification evidence to attempt 1 of run-new.
	vrNew := &VerificationResult{Command: "test -f new", ExitCode: 1, Stderr: "new run missing"}
	tl.UpdateReceiptVerifyResult("run-new", todoID, 1, vrNew)

	for _, item := range tl.Items() {
		if item.ID != todoID {
			continue
		}
		for _, r := range item.ExecutionReceipts {
			if r.RunID == "run-new" && r.Attempt == 1 {
				if r.VerifyResult == nil || r.VerifyResult.Command != "test -f new" {
					t.Errorf("run-new attempt 1 receipt VerifyResult = %v, want command 'test -f new'", r.VerifyResult)
				}
			}
			if r.RunID == "run-old" && r.Attempt == 1 {
				if r.VerifyResult != nil {
					t.Errorf("run-old attempt 1 receipt VerifyResult = %v, want nil (RunID match must prevent cross-run overwrite)", r.VerifyResult)
				}
			}
		}
		break
	}
}

// TestCloneTodoItem_DeepCopiesExecutionReceiptVerifyResult verifies the
// snapshot-isolation fix (reviewer P2): TodoList.Items() returns snapshots
// whose ExecutionReceipts[].VerifyResult pointers are independent deep copies,
// so mutating a snapshot's verify result does not affect the canonical item.
//
// Refs: docs/hufu-generic-task-reliability-mechanisms.md §5, §9, WP-05
func TestCloneTodoItem_DeepCopiesExecutionReceiptVerifyResult(t *testing.T) {
	tl := NewTaskTracker().TodoList()
	items := tl.AddBatch([]TodoSpec{{Agent: "worker", Desc: "do work"}})
	todoID := items[0].ID

	zero := 0
	tl.SetExecutionReceipt(todoID, &ExecutionReceipt{RunID: "r", TaskID: todoID, Attempt: 1, ExitCode: &zero})
	vr := &VerificationResult{Command: "test -f x", ExitCode: 1, Stderr: "original"}
	tl.UpdateReceiptVerifyResult("r", todoID, 1, vr)

	// Take a snapshot via Items().
	snapshots := tl.Items()
	var snapVR *VerificationResult
	for _, item := range snapshots {
		if item.ID != todoID {
			continue
		}
		for i := range item.ExecutionReceipts {
			if item.ExecutionReceipts[i].Attempt == 1 {
				snapVR = item.ExecutionReceipts[i].VerifyResult
				break
			}
		}
		break
	}
	if snapVR == nil {
		t.Fatal("snapshot does not carry the verify result")
	}

	// Mutate the snapshot's verify result outside the todo lock. With a
	// shallow copy this would mutate the canonical item's verify result too.
	snapVR.Stderr = "mutated-by-snapshot"

	// Re-read the canonical item and assert its verify result is unchanged.
	for _, item := range tl.Items() {
		if item.ID != todoID {
			continue
		}
		for i := range item.ExecutionReceipts {
			if item.ExecutionReceipts[i].Attempt == 1 {
				got := item.ExecutionReceipts[i].VerifyResult
				if got == nil {
					t.Errorf("canonical ExecutionReceipt VerifyResult is nil after snapshot mutation")
				} else if got.Stderr != "original" {
					t.Errorf("canonical ExecutionReceipt VerifyResult.Stderr = %q, want 'original' (snapshot mutation must not leak via shared pointer)", got.Stderr)
				}
			}
		}
		break
	}
}
