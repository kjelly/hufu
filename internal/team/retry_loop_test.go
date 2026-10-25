package team

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"

	"charm.land/fantasy"
	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/utils"
)

// The tests in this file drive the real executeTask retry loop with a scripted
// worker and check the observable outcome: how many times the worker ran, the
// task status, the retry statistics, and what the next attempt was told.
// DecideRecovery itself is covered by disposition_test.go; the failure
// classifier by failure_classify_test.go.

func builtinProfile(t *testing.T, name ExecutionProfileName) ExecutionProfile {
	t.Helper()
	profile, ok := GetBuiltinProfile(string(name))
	if !ok {
		t.Fatalf("builtin profile %q not found", name)
	}
	return profile
}

func TestExecuteTaskRetryDisposition(t *testing.T) {
	contractErr := errors.New("contract preflight failed: verify (verifier_not_asserting): tail || echo")
	environmentErr := errors.New("bash: nonexistent-cmd: command not found")

	tests := []struct {
		name   string
		worker *scriptedWorker
		task   TaskDef
		setup  func(t *testing.T, c *Coordinator)
		// cancelParent runs the task under an already-cancelled context.
		cancelParent bool
		wantCalls    int
		// atMost treats wantCalls as an upper bound; the loop may stop before
		// the first dispatch when preflight blocks the task.
		atMost         bool
		wantStatus     TaskStatus
		errNotContains string
		wantRetries    map[TaskFailureClass]int
		wantNoRetries  bool
	}{
		{
			name:       "protocol failure on a non-replayable task blocks without retry",
			worker:     scriptWorker(),
			task:       TaskDef{SideEffect: SideEffectExternalWrite, Execution: ExecutionContract{RequiresResult: true}},
			wantCalls:  1,
			atMost:     true,
			wantStatus: TaskBlocked,
		},
		{
			name:       "non-replayable infra mutation blocks without retry",
			worker:     scriptWorker(),
			task:       TaskDef{SideEffect: SideEffectInfraMutation},
			wantCalls:  1,
			wantStatus: TaskBlocked,
		},
		{
			name:      "wrong-polarity verify command is not retried",
			worker:    scriptWorker(failTurn(errWrongVerificationPolarity)),
			task:      TaskDef{Verify: "nonexistent-cmd | grep -c running"},
			wantCalls: 1,
			atMost:    true,
		},
		{
			name:      "identical failure on the second attempt stops the loop",
			worker:    scriptWorker(failTurn(errors.New("worker failed: identical error"))),
			wantCalls: 2,
		},
		{
			name: "distinct failures exhaust the retry budget",
			worker: scriptWorker(
				failTurn(errors.New("worker failed: connection refused")),
				failTurn(errors.New("worker failed: file not found")),
				failTurn(errors.New("worker failed: permission denied")),
			),
			wantCalls: 3,
		},
		{
			name:           "contract failure replans without retry",
			worker:         scriptWorker(failTurn(contractErr)),
			wantCalls:      1,
			errNotContains: "repeated failure",
		},
		{
			name:           "environment failure replans without retry",
			worker:         scriptWorker(failTurn(environmentErr)),
			wantCalls:      1,
			errNotContains: "repeated failure",
		},
		{
			name:   "unattended profile resolves reconcile and blocks",
			worker: scriptWorker(),
			setup: func(t *testing.T, c *Coordinator) {
				c.unattended = true
				c.executionProfile = builtinProfile(t, ProfileUnattended)
			},
			wantCalls:  1,
			wantStatus: TaskBlocked,
		},
		{
			name:   "strict verification profile resolves reconcile and blocks",
			worker: scriptWorker(),
			setup: func(t *testing.T, c *Coordinator) {
				c.executionProfile = builtinProfile(t, ProfileStrictVerification)
			},
			wantCalls:  1,
			wantStatus: TaskBlocked,
		},
		{
			name:         "cancelled parent context stops before a second dispatch",
			worker:       scriptWorker(),
			cancelParent: true,
			wantCalls:    1,
		},
		{
			name:          "cancelled parent with a cancelling worker records no retry classes",
			worker:        scriptWorker(scriptedTurn{err: context.Canceled}),
			cancelParent:  true,
			wantCalls:     1,
			wantNoRetries: true,
		},
		{
			name: "worker cancellation stops without counting a retry",
			worker: scriptWorker(
				scriptedTurn{text: "partial work before cancellation", err: context.Canceled},
				textTurn("succeeded on retry"),
			),
			wantCalls:   1,
			wantRetries: map[TaskFailureClass]int{FailureCancelled: 0},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newTestCoordinator(t, withWorker(tt.worker))
			if tt.setup != nil {
				tt.setup(t, c)
			}
			todoID := addTodo(t, c, "do work")
			ctx := context.Background()
			if tt.cancelParent {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			task := tt.task
			task.Agent, task.Goal = "worker", "do work"

			_, err := c.executeTask(ctx, task, todoID)
			if err == nil {
				t.Fatal("executeTask succeeded, want failure")
			}
			if tt.errNotContains != "" && strings.Contains(err.Error(), tt.errNotContains) {
				t.Errorf("error %q must not mention %q", err, tt.errNotContains)
			}
			switch calls := tt.worker.Calls(); {
			case tt.atMost && calls > tt.wantCalls:
				t.Errorf("worker dispatched %d times, want at most %d", calls, tt.wantCalls)
			case !tt.atMost && calls != tt.wantCalls:
				t.Errorf("worker dispatched %d times, want %d", calls, tt.wantCalls)
			}
			if tt.wantStatus != "" {
				if got := todoByID(t, c, todoID).Status; got != tt.wantStatus {
					t.Errorf("task status = %s, want %s", got, tt.wantStatus)
				}
			}
			retries := c.Metrics().RetriesByFailureClass
			if tt.wantNoRetries && len(retries) != 0 {
				t.Errorf("RetriesByFailureClass = %v, want empty", retries)
			}
			for class, want := range tt.wantRetries {
				if got := retries[class]; got != want {
					t.Errorf("RetriesByFailureClass[%s] = %d, want %d", class, got, want)
				}
			}
		})
	}
}

func TestExecuteTaskTerminalBlockedRequiresManualCustody(t *testing.T) {
	worker := scriptWorker()
	c := newTestCoordinator(t, withWorker(worker))
	todoID := addTodo(t, c, "do work")

	// An unreconciled restored session whose PID identity mismatches makes
	// cleanup fail closed, so the task needs human reconciliation instead of
	// a replay.
	mgr, err := NewTerminalSessionManager(c.session.Workspace, nil)
	if err != nil {
		t.Fatalf("NewTerminalSessionManager: %v", err)
	}
	mgr.mu.Lock()
	mgr.sessions[todoID] = &managedTerminalSession{
		session: TerminalSession{
			ID:              "test-session",
			OwnerTaskID:     todoID,
			PID:             os.Getpid(),
			State:           TerminalSessionUnknown,
			ProcessIdentity: &ProcessIdentity{PID: os.Getpid(), StartTime: -1},
		},
	}
	mgr.mu.Unlock()
	c.terminalSessionMgr = mgr

	if _, err := c.executeTask(context.Background(), TaskDef{Agent: "worker", Goal: "do work"}, todoID); err == nil {
		t.Fatal("expected error from terminal-blocked execution")
	}
	if calls := worker.Calls(); calls != 1 {
		t.Errorf("worker dispatched %d times, want 1 (terminal block must break before retry)", calls)
	}
	if item := todoByID(t, c, todoID); item.Status != TaskBlocked {
		t.Fatalf("task status after terminal cleanup = %s, want blocked", item.Status)
	}
	sessions, err := mgr.List(context.Background(), "")
	if err != nil || len(sessions) != 1 {
		t.Fatalf("persisted terminal sessions = %#v, err=%v", sessions, err)
	}
	if sessions[0].CleanupState != TerminalCleanupManual || !strings.Contains(sessions[0].CleanupError, "manual custody") {
		t.Fatalf("persisted terminal cleanup disposition = %#v, want manual custody intervention", sessions[0])
	}
	restored, err := NewTerminalSessionManager(c.session.Workspace, nil)
	if err != nil {
		t.Fatal(err)
	}
	persisted, err := restored.List(context.Background(), "")
	if err != nil || len(persisted) != 1 || persisted[0].CleanupState != TerminalCleanupManual {
		t.Fatalf("manual terminal disposition was not durable: %#v, err=%v", persisted, err)
	}
}

func TestExecuteTaskRetryIssuesFreshContextPerAttempt(t *testing.T) {
	worker := scriptWorker(
		scriptedTurn{text: "first attempt produced partial output", err: errors.New("worker failed: first attempt error")},
		textTurn("succeeded on retry"),
	)
	c := newTestCoordinator(t, withWorker(worker))
	todoID := addTodo(t, c, "do work")

	out, err := c.executeTask(context.Background(), TaskDef{Agent: "worker", Goal: "do work"}, todoID)
	if err != nil {
		t.Fatalf("expected retry to succeed, got: %v", err)
	}
	if calls := worker.Calls(); calls != 2 {
		t.Errorf("worker dispatched %d times, want 2", calls)
	}
	if strings.TrimSpace(out) == "" {
		t.Error("expected non-empty output after successful retry")
	}
	item := todoByID(t, c, todoID)
	if item.Status != TaskDone {
		t.Errorf("task status = %s, want done", item.Status)
	}
	if len(item.ContextManifests) != 2 {
		t.Fatalf("retry context manifests = %#v, want one durable manifest per attempt", item.ContextManifests)
	}
	first, second := item.ContextManifests[0], item.ContextManifests[1]
	if first.Attempt != 1 || first.Trigger != ContextTriggerTaskDispatch || second.Attempt != 2 || second.Trigger != ContextTriggerRetry {
		t.Fatalf("retry manifest attempt/trigger identity = %#v", item.ContextManifests)
	}
	if first.RequestID == second.RequestID || first.RequestHash == second.RequestHash || first.Fingerprint == second.Fingerprint {
		t.Fatalf("retry reused prior context identity: first=%#v second=%#v", first, second)
	}
	if len(item.ExecutionReceipts) < 2 || item.ExecutionReceipts[0].ContextManifest == nil || item.ExecutionReceipts[1].ContextManifest == nil {
		t.Fatalf("retry receipts missing matching context manifests: %#v", item.ExecutionReceipts)
	}
	if item.ExecutionReceipts[0].ContextManifest.RequestID != first.RequestID || item.ExecutionReceipts[1].ContextManifest.RequestID != second.RequestID {
		t.Fatalf("receipt/manifest identity mismatch: receipts=%#v manifests=%#v", item.ExecutionReceipts, item.ContextManifests)
	}
}

func TestExecuteTaskRetryPromptCarriesPreviousAttemptEvidence(t *testing.T) {
	worker := scriptWorker(
		scriptedTurn{text: "first attempt partial output before error", err: errors.New("worker failed: execution error on first attempt")},
		textTurn("succeeded on retry"),
	)
	c := newTestCoordinator(t, withWorker(worker))
	todoID := addTodo(t, c, "do work")

	if _, err := c.executeTask(context.Background(), TaskDef{Agent: "worker", Goal: "do work"}, todoID); err != nil {
		t.Fatalf("expected retry to succeed, got: %v (calls=%d)", err, worker.Calls())
	}
	if calls := worker.Calls(); calls != 2 {
		t.Fatalf("worker dispatched %d times, want 2", calls)
	}
	retryPrompt := worker.Prompt(t, 2)
	for _, want := range []string{
		"Failure class:", "execution",
		"Evidence:", "partial output:", "first attempt partial output",
		"Previous command/exit:",
		"What you can change:",
	} {
		if !strings.Contains(retryPrompt, want) {
			t.Errorf("retry prompt missing %q\nprompt: %s", want, retryPrompt)
		}
	}
}

func TestExecuteTaskRetryPromptIncludesVerifyCommand(t *testing.T) {
	worker := scriptWorker(textTurn("work output"))
	c := newTestCoordinator(t, withWorker(worker))
	todoID := addTodo(t, c, "do work")

	// Verification fails on every attempt, so the loop stops once the same
	// failure repeats; attempt 2 must still see attempt 1's verify evidence.
	const verify = "test -f /nonexistent-deliverable-for-retry-test"
	if _, err := c.executeTask(context.Background(), TaskDef{Agent: "worker", Goal: "do work", Verify: verify}, todoID); err == nil {
		t.Fatal("expected verification to fail")
	}
	retryPrompt := worker.Prompt(t, 2)
	for _, want := range []string{"Previous command/exit:", "verify `" + verify + "`", "exit: 1"} {
		if !strings.Contains(retryPrompt, want) {
			t.Errorf("retry prompt missing %q\nprompt: %s", want, retryPrompt)
		}
	}
}

func TestExecuteTaskClearsStaleVerifyResultBetweenRetries(t *testing.T) {
	// Attempt 1 produces output and fails verification (exit 1). Attempt 2
	// fails before verification, so its classification must fall back to the
	// error text instead of inheriting attempt 1's exit code. Attempts 3 and
	// 4 fail verification again.
	worker := scriptWorker(
		textTurn("attempt 1 output"),
		scriptedTurn{text: "attempt 2 partial output before error", err: errors.New("agent failed: connection reset")},
		textTurn("attempt 3 success"),
	)
	c := newTestCoordinator(t, withWorker(worker), withTeamConfig(func(cfg *agent.TeamConfig) {
		// Exercise every classification attempt without reaching the
		// separate anti-thrashing boundary.
		cfg.Reliability.MaxSameFailureFingerprint = 5
	}))
	todoID := addTodo(t, c, "do work")

	task := TaskDef{Agent: "worker", Goal: "do work", Verify: "test -f /nonexistent-artifact"}
	if _, err := c.executeTask(context.Background(), task, todoID); err == nil {
		t.Fatal("expected executeTask to fail (verify never passes)")
	}
	if calls := worker.Calls(); calls != 4 {
		t.Fatalf("worker dispatched %d times, want 4 (initial attempt plus three retries)", calls)
	}
	retries := c.Metrics().RetriesByFailureClass
	if got := retries[FailureVerify]; got != 2 {
		t.Errorf("FailureVerify retries = %d, want 2 (attempt 2 must not inherit the stale exit code)", got)
	}
	if got := retries[FailureExecution]; got != 1 {
		t.Errorf("FailureExecution retries = %d, want 1 (attempt 2's connection reset)", got)
	}

	// Clearing the todo-wide slot must not discard attempt 1's verification
	// evidence: it stays on that attempt's receipt for forensics.
	item := todoByID(t, c, todoID)
	var first *ExecutionReceipt
	for i := range item.ExecutionReceipts {
		if item.ExecutionReceipts[i].Attempt == 1 {
			first = &item.ExecutionReceipts[i]
			break
		}
	}
	if first == nil || first.VerifyResult == nil {
		t.Fatalf("attempt 1 receipt lost its verify result: %#v", first)
	}
	if first.VerifyResult.ExitCode != 1 || first.VerifyResult.Command == "" {
		t.Errorf("attempt 1 receipt verify result = %#v, want exit 1 with the verify command", first.VerifyResult)
	}
}

func TestRecordRetrySkipsCancelledClass(t *testing.T) {
	c := &Coordinator{}
	c.recordRetry(FailureExecution)
	c.recordRetry(FailureEnvironment)
	c.recordRetry(FailureCancelled)
	got := c.Metrics().RetriesByFailureClass
	for class, want := range map[TaskFailureClass]int{FailureExecution: 1, FailureEnvironment: 1, FailureCancelled: 0} {
		if got[class] != want {
			t.Errorf("RetriesByFailureClass[%s] = %d, want %d", class, got[class], want)
		}
	}
}

func TestPersistFailureExcludesCancelledFromFingerprint(t *testing.T) {
	c, store := systemicCoordinator(t, nil)
	todoID := addTodo(t, c, "do work")

	cancelled := func() {
		c.PersistFailure("worker", "do work", todoID, c.FailureDetail(context.Canceled, FailureSourceSigint))
	}
	cancelled()
	events, _ := store.ReadEvents()
	counts := map[string]int{}
	for _, e := range events {
		counts[e.Type]++
	}
	for _, forbidden := range []string{"failure_fingerprint", "repeated_failure_fingerprint", "anti_thrashing_limit_reached"} {
		if counts[forbidden] != 0 {
			t.Errorf("%s emitted %d times for a cancelled failure, want 0", forbidden, counts[forbidden])
		}
	}
	if counts["task_cancelled"] != 1 || counts["task_failed"] != 0 {
		t.Errorf("terminal events = %v, want one task_cancelled and no task_failed", counts)
	}
	// A second identical cancellation must not count as a repeated failure.
	cancelled()
	if got := c.Metrics().RepeatedFailureFingerprints; got != 0 {
		t.Errorf("RepeatedFailureFingerprints = %d, want 0 (cancelled excluded)", got)
	}
}

func TestComputeEvidenceComplete(t *testing.T) {
	requiresResult := TaskDef{Execution: ExecutionContract{RequiresResult: true}}
	tests := []struct {
		name          string
		task          TaskDef
		transcriptRef string
		steps         []fantasy.StepResult
		output        string
		want          bool
	}{
		{name: "transcript required but missing", task: requiresResult},
		{name: "transcript required and present", task: requiresResult, transcriptRef: "/workspace/logs/transcript.json", want: true},
		{name: "no steps and no output"},
		{name: "output present", output: "some output", want: true},
		{name: "step with messages", steps: []fantasy.StepResult{{Messages: []fantasy.Message{fantasy.NewUserMessage("test")}}}, want: true},
		{name: "empty step is not evidence", steps: []fantasy.StepResult{{}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := computeEvidenceComplete(tt.task, tt.transcriptRef, tt.steps, tt.output); got != tt.want {
				t.Errorf("computeEvidenceComplete() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestBuildRetryContext(t *testing.T) {
	const (
		apiKey           = "sk-secret-1234567890abcdef"
		errSecret        = "err-secret-123"
		verifySecret     = "verify-secret-456"
		outputSecret     = "output-secret-789"
		transcriptSecret = "API_KEY=transcript-secret-000"
	)
	// The OnToolCall callback redacts and truncates tool input before it is
	// stored as evidence; the retry context must keep that marker.
	redactedInput := utils.TruncateRunes(utils.RedactSecrets(`export API_KEY=`+apiKey+` && curl -H "Authorization: Bearer `+apiKey+`" https://api.example.com`), 500)
	if strings.Contains(redactedInput, apiKey) {
		t.Fatalf("RedactSecrets kept the API key: %s", redactedInput)
	}

	tests := []struct {
		name            string
		class           TaskFailureClass
		err             error
		transcriptRef   string
		verifyCmd       string
		verifyExit      int
		toolCall        string
		toolInput       string
		toolResult      string
		toolResultErr   bool
		output          string
		wantContains    []string
		wantNotContains []string
		wantOnce        string
	}{
		{
			name:         "verify command and exit code are rendered",
			class:        FailureVerify,
			err:          errors.New("deliverable verification failed: exit code 1"),
			verifyCmd:    "test -f /nonexistent-deliverable",
			verifyExit:   1,
			wantContains: []string{"Previous command/exit:", "verify", "exit: 1"},
		},
		{
			name:          "tool input is rendered once with its error state",
			class:         FailureExecution,
			err:           errors.New("worker failed: execution error"),
			toolCall:      "bash",
			toolInput:     "echo hello && false",
			toolResult:    "hello\nexit code: 1",
			toolResultErr: true,
			output:        "partial output",
			wantContains:  []string{"Previous command/exit:", "bash", "echo hello", "error", "partial output:"},
			wantOnce:      "echo hello && false",
		},
		{
			name:            "redacted tool input keeps the redaction marker",
			class:           FailureExecution,
			err:             errors.New("worker failed: execution error"),
			toolCall:        "bash",
			toolInput:       redactedInput,
			toolResult:      "command failed",
			toolResultErr:   true,
			output:          "partial output",
			wantContains:    []string{"[REDACTED]"},
			wantNotContains: []string{apiKey},
		},
		{
			name:            "every untrusted field is redacted at the prompt boundary",
			class:           FailureVerify,
			err:             errors.New("provider failed: api_key=" + errSecret),
			transcriptRef:   "/tmp/" + transcriptSecret + "/transcript.json",
			verifyCmd:       "curl -H 'Authorization: Bearer " + verifySecret + "' https://example.invalid",
			verifyExit:      1,
			toolCall:        "bash",
			toolInput:       "echo safe",
			toolResult:      "failed",
			toolResultErr:   true,
			output:          "previous output password=" + outputSecret,
			wantContains:    []string{"[REDACTED]"},
			wantNotContains: []string{errSecret, verifySecret, outputSecret, transcriptSecret},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := buildRetryContext(tt.class, tt.err, tt.transcriptRef, tt.verifyCmd, tt.verifyExit, nil,
				tt.toolCall, tt.toolInput, tt.toolResult, tt.toolResultErr, tt.output, TaskDef{Verify: tt.verifyCmd})
			for _, want := range tt.wantContains {
				if !strings.Contains(got, want) {
					t.Errorf("retry context missing %q\ncontext: %s", want, got)
				}
			}
			for _, secret := range tt.wantNotContains {
				if strings.Contains(got, secret) {
					t.Errorf("retry context leaked %q\ncontext: %s", secret, got)
				}
			}
			if tt.wantOnce != "" && strings.Count(got, tt.wantOnce) != 1 {
				t.Errorf("retry context should contain %q exactly once\ncontext: %s", tt.wantOnce, got)
			}
		})
	}
}

func TestRetryContextRedactsCredentialPatterns(t *testing.T) {
	tests := []struct {
		name   string
		input  string
		secret string
	}{
		{"API key in env var", "export API_KEY=sk-secret-1234567890abcdef", "sk-secret"},
		{"Authorization header", `curl -H "Authorization: Bearer sk-secret-token"`, "sk-secret"},
		{"Private key block", "-----BEGIN RSA PRIVATE KEY-----\nMIIEpAI...\n-----END RSA PRIVATE KEY-----", "MIIEpAI"},
		{"Token in assignment", "TOKEN=abc123secretkey456", "abc123secretkey"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			redacted := utils.RedactSecrets(tt.input)
			if !strings.Contains(redacted, "[REDACTED]") {
				t.Fatalf("RedactSecrets(%q) = %q, want a redaction marker", tt.input, redacted)
			}
			got := buildRetryContext(FailureExecution, errors.New("failed"), "", "", 0, nil, "bash", redacted, "error", true, "output", TaskDef{})
			if strings.Contains(got, tt.secret) {
				t.Errorf("retry context leaked %q: %s", tt.secret, got)
			}
		})
	}
}

func TestRetryPromptHelpersRedactSecrets(t *testing.T) {
	const secret = "fallback-secret-654"
	if got := retryPartialOutput("", "previous output: token="+secret); strings.Contains(got, secret) || !strings.Contains(got, "[REDACTED]") {
		t.Errorf("fallback retry output was not redacted: %q", got)
	}
	reflection := buildFailureReflectionPrompt("worker", "safe goal", "authorization: Bearer reflection-secret-321")
	if strings.Contains(reflection, "reflection-secret-321") || !strings.Contains(reflection, "[REDACTED]") {
		t.Errorf("reflection prompt did not redact failure input: %s", reflection)
	}
}

// sameCoordinatorConcurrentAgent synchronizes both first attempts and uses the
// todo ID carried in the execution context to produce task-specific evidence.
// Two tasks share it on one Coordinator so a coordinator-global evidence slot
// would be observable.
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
	if attempt > 1 {
		return textAgentResult("completed " + todoID), nil
	}
	toolInput := "echo evidence-" + todoID
	toolResult := "tool result for " + todoID
	if onToolCall != nil {
		if err := onToolCall(fantasy.ToolCallContent{ToolCallID: "call-" + todoID, ToolName: "bash", Input: toolInput}); err != nil {
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
	return textAgentResult("partial evidence for " + todoID), errors.New("worker failed after task-specific evidence")
}

func TestExecuteTaskRetryPromptKeepsEvidencePerTaskUnderConcurrency(t *testing.T) {
	worker := newSameCoordinatorConcurrentAgent()
	c := newTestCoordinator(t, withWorker(worker), withTeamConfig(func(cfg *agent.TeamConfig) {
		// Both tasks fail once with the same category; keep the shared
		// fingerprint limit above those two failures.
		cfg.Reliability.MaxSameFailureFingerprint = 3
	}))
	items := c.taskTracker.TodoList().AddBatch([]TodoSpec{
		{Agent: "worker", Desc: "task-A"},
		{Agent: "worker", Desc: "task-B"},
	})
	todoA, todoB := items[0].ID, items[1].ID

	done := make(chan error, 2)
	for _, todoID := range []string{todoA, todoB} {
		go func() {
			_, err := c.executeTask(context.Background(), TaskDef{Agent: "worker", Goal: "do work"}, todoID)
			done <- err
		}()
	}
	for range 2 {
		if err := <-done; err != nil {
			t.Errorf("task failed: %v", err)
		}
	}

	worker.mu.Lock()
	defer worker.mu.Unlock()
	if worker.calls[todoA] != 2 || worker.calls[todoB] != 2 {
		t.Fatalf("calls task-A=%d task-B=%d, want 2 each", worker.calls[todoA], worker.calls[todoB])
	}
	if worker.toolInputs[todoA] == "" || worker.toolInputs[todoB] == "" || worker.toolResults[todoA] == "" || worker.toolResults[todoB] == "" {
		t.Fatalf("mock did not observe both callback paths: inputs=%#v results=%#v", worker.toolInputs, worker.toolResults)
	}
	for _, pair := range []struct{ own, other string }{{todoA, todoB}, {todoB, todoA}} {
		retryPrompt := worker.prompts[pair.own][1]
		for _, want := range []string{"partial evidence for " + pair.own, "echo evidence-" + pair.own, "tool result for " + pair.own} {
			if !strings.Contains(retryPrompt, want) {
				t.Errorf("retry prompt for %s lost its own evidence %q\n%s", pair.own, want, retryPrompt)
			}
		}
		for _, leaked := range []string{"partial evidence for " + pair.other, "echo evidence-" + pair.other, "tool result for " + pair.other} {
			if strings.Contains(retryPrompt, leaked) {
				t.Errorf("retry prompt for %s leaked %q from the other task\n%s", pair.own, leaked, retryPrompt)
			}
		}
	}
}
