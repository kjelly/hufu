package team

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/kjelly/hufu/internal/agent"
)

// Phase 5 tests (spec.md §36 PR-11/PR-12): the real CodexSubagentProvider,
// registered and driven end to end against a real fake app-server
// subprocess (codex_fake_server_test.go).

// newCodexWorkspace returns a fresh workspace directory. It is separate from
// newCodexHarness because a test's scripted thread/start response must
// assert the exact effective cwd, which is this same workspace path — the
// workspace has to exist before the script that references it is written.
func newCodexWorkspace(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("ProcessSupervisor has no Windows implementation yet (§16.2)")
	}
	return t.TempDir()
}

func newCodexHarness(t *testing.T, workspace string, steps []fakeCodexStep) (*Coordinator, *CodexSubagentProvider, *TodoItem, string) {
	t.Helper()
	scriptDir := t.TempDir()
	scriptPath := filepath.Join(scriptDir, "script.json")
	rawLogPath := filepath.Join(scriptDir, "raw_calls.log")
	data, err := json.Marshal(fakeCodexScript{Steps: steps, RawLogPath: rawLogPath})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(scriptPath, data, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv(fakeCodexServerScriptEnvVar, scriptPath)

	store, err := NewEventStore(workspace, "codex-attempt-run", "codex-attempt-session")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	c := &Coordinator{
		session:        &TeamSession{Workspace: workspace, Config: agent.TeamConfig{Name: "codex-attempt-test"}},
		projectDir:     workspace,
		taskTracker:    NewTaskTracker(),
		sessionData:    NewSession(),
		eventStore:     store,
		executionRunID: "codex-attempt-run",
		reportStatus:   func(StatusEvent) {},
	}
	item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "codex attempt", Goal: "codex attempt"}})[0]

	provider := NewCodexSubagentProvider(c, "codex", agent.SubagentProviderConfig{
		Type: codexAppServerProviderType, Command: []string{os.Args[0]},
		InheritEnv:     []string{fakeCodexServerScriptEnvVar},
		StartupTimeout: "5s", InterruptGrace: "200ms", ShutdownGrace: "200ms",
	})
	return c, provider, item, rawLogPath
}

func codexAttemptRequest(item *TodoItem, prompt string) AttemptRequest {
	return AttemptRequest{
		RunID: "codex-attempt-run", TaskID: item.ID, Attempt: 1,
		Task:   TaskDef{Agent: "worker", Goal: "codex attempt", SideEffect: SideEffectWorkspaceWrite},
		Prompt: prompt, ModelID: "gpt-5-codex", Provider: "codex",
	}
}

// codexInitializeStep answers the "initialize" call StartCodexAppServer
// always makes first, before any thread/turn operation (§13.2 step 8).
func codexInitializeStep(t *testing.T) fakeCodexStep {
	return fakeCodexStep{Result: rawJSON(t, map[string]any{"userAgent": "fake/1.0", "codexHome": "/fake/home"})}
}

// codexThreadStartStep answers a thread/start call with a fixed
// gpt-5-codex/workspace-write effective state — see
// codex_thread_lifecycle_test.go's fakeCodexThreadStartStep for the real
// (thread.id-nested, sandbox-policy-object) response shape this wraps.
func codexThreadStartStep(t *testing.T, threadID, cwd string) fakeCodexStep {
	return fakeCodexThreadStartStep(threadID, cwd, "gpt-5-codex", "workspace-write", t)
}

// codexThreadResumeStep answers a thread/resume call, with an explicit
// sandbox so tests can prove Hufu correctly validates (or a compliant
// app-server correctly honors) a forced sandbox override such as §23's
// read-only repair resume. The real protocol's thread/resume response has
// the same shape as thread/start's, so this reuses the same builder.
func codexThreadResumeStep(t *testing.T, threadID, cwd, sandbox string) fakeCodexStep {
	return fakeCodexThreadStartStep(threadID, cwd, "gpt-5-codex", sandbox, t)
}

// readRawCalls decodes every raw JSON-RPC request line recorded at path (see
// fakeCodexScript.RawLogPath), generically, so a test can inspect a specific
// call's params rather than just its method name.
func readRawCalls(t *testing.T, path string) []map[string]json.RawMessage {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatal(err)
	}
	trimmed := strings.TrimSpace(string(data))
	if trimmed == "" {
		return nil
	}
	var calls []map[string]json.RawMessage
	for _, line := range strings.Split(trimmed, "\n") {
		var call map[string]json.RawMessage
		if err := json.Unmarshal([]byte(line), &call); err != nil {
			t.Fatal(err)
		}
		calls = append(calls, call)
	}
	return calls
}

// findRawCall returns the first recorded call to method, or fails the test.
func findRawCall(t *testing.T, calls []map[string]json.RawMessage, method string) map[string]json.RawMessage {
	t.Helper()
	for _, call := range calls {
		var m string
		_ = json.Unmarshal(call["method"], &m)
		if m == method {
			return call
		}
	}
	t.Fatalf("no recorded call to %q among %d calls", method, len(calls))
	return nil
}

// rawCallMethods returns every recorded call's method, in arrival order.
func rawCallMethods(t *testing.T, calls []map[string]json.RawMessage) []string {
	t.Helper()
	methods := make([]string, 0, len(calls))
	for _, call := range calls {
		var m string
		_ = json.Unmarshal(call["method"], &m)
		methods = append(methods, m)
	}
	return methods
}

// TestCodexProviderRegisteredAndResolvable is supplementary (not one of
// PR-11/12's named tests): proves a configured codex-app-server entry
// actually becomes reachable through SubagentRegistry — the PR-11 wiring
// gap that has no dedicated spec-named test.
func TestCodexProviderRegisteredAndResolvable(t *testing.T) {
	c := &Coordinator{session: &TeamSession{Config: agent.TeamConfig{
		Name: "codex-registration-test",
		SubagentProviders: map[string]agent.SubagentProviderConfig{
			"codex": {Type: codexAppServerProviderType, Command: []string{"codex", "app-server"}},
		},
	}}}
	provider, err := c.SubagentRegistry().Resolve("codex")
	if err != nil {
		t.Fatalf("codex provider was not registered: %v", err)
	}
	if provider.Name() != "codex" {
		t.Fatalf("resolved provider name = %q, want %q", provider.Name(), "codex")
	}
	if _, ok := provider.(*CodexSubagentProvider); !ok {
		t.Fatalf("resolved provider = %T, want *CodexSubagentProvider", provider)
	}
}

// TestCodexProviderResultBecomesCanonical is supplementary: a full success
// path proving RunAttempt's canonicalize step produces a usable
// CanonicalResult while correctly leaving TypedResult nil (§8.3), and that
// the session binding is durably visible afterward.
func TestCodexProviderResultBecomesCanonical(t *testing.T) {
	workspace := newCodexWorkspace(t)
	c, provider, item, _ := newCodexHarness(t, workspace, []fakeCodexStep{
		codexInitializeStep(t),
		codexThreadStartStep(t, "thread-1", workspace),
		fakeCodexTurnCompletedStep(t, "turn-1", validProposalJSON("")),
	})

	result, err := provider.RunAttempt(context.Background(), codexAttemptRequest(item, "do the work"))
	if err != nil {
		t.Fatalf("RunAttempt: %v", err)
	}
	if result.TypedResult != nil {
		t.Fatalf("TypedResult = %#v, want nil for an external provider (§8.3)", result.TypedResult)
	}
	if result.CanonicalResult == nil || result.CanonicalResult.Status != TaskResultStatusSuccess || result.CanonicalResult.Source != ExternalProviderProposalSource {
		t.Fatalf("CanonicalResult = %#v, want a successful canonical result sourced from the external provider", result.CanonicalResult)
	}
	if result.ProviderSessionID != "thread-1" {
		t.Fatalf("ProviderSessionID = %q, want %q", result.ProviderSessionID, "thread-1")
	}
	if got := c.todoItemByID(item.ID).ProviderBinding; got == nil || got.SessionID != "thread-1" {
		t.Fatalf("durable ProviderBinding = %#v, want SessionID thread-1", got)
	}
}

// TestCodexCancellationPreservesBindingAndTranscript proves §16.1/§16.3: when
// the caller's context is cancelled mid-turn, RunAttempt returns promptly
// with a classified error, but the already-persisted durable session
// binding is not erased and a non-empty transcript is preserved.
func TestCodexCancellationPreservesBindingAndTranscript(t *testing.T) {
	workspace := newCodexWorkspace(t)
	c, provider, item, _ := newCodexHarness(t, workspace, []fakeCodexStep{
		codexInitializeStep(t),
		codexThreadStartStep(t, "thread-cancel", workspace),
		{Result: rawJSON(t, map[string]any{"turn": map[string]any{"id": "turn-cancel"}})}, // acked, but turn/completed never arrives
	})

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(200 * time.Millisecond)
		cancel()
	}()

	result, err := provider.RunAttempt(ctx, codexAttemptRequest(item, "do the work"))
	if err == nil {
		t.Fatal("expected RunAttempt to fail once the context was cancelled")
	}
	var provErr *CodexProviderError
	if errors.As(err, &provErr) && provErr.Class != CodexFailureCancelled {
		t.Fatalf("failure class = %q, want %q", provErr.Class, CodexFailureCancelled)
	}
	if result.TranscriptRef == "" {
		t.Fatal("TranscriptRef is empty, want a preserved transcript reference")
	}
	if content, readErr := os.ReadFile(result.TranscriptRef); readErr != nil || len(content) == 0 {
		t.Fatalf("transcript file = (err=%v, len=%d), want a non-empty preserved transcript", readErr, len(content))
	}
	binding := c.todoItemByID(item.ID).ProviderBinding
	if binding == nil || binding.SessionID != "thread-cancel" {
		t.Fatalf("durable ProviderBinding = %#v, want the session bound before cancellation to survive", binding)
	}
}

// TestCodexCrashPreservesWorkspaceDelta proves §16.3: even when the
// app-server process crashes mid-turn, Hufu still captures whatever the
// process changed on disk before it died, rather than discarding evidence
// along with the failed attempt.
func TestCodexCrashPreservesWorkspaceDelta(t *testing.T) {
	workspace := newCodexWorkspace(t)
	_, provider, item, _ := newCodexHarness(t, workspace, []fakeCodexStep{
		codexInitializeStep(t),
		codexThreadStartStep(t, "thread-crash", workspace),
		{WriteFile: "changed-before-crash.txt", WriteFileContent: "written just before the crash", CloseAfter: true},
	})

	result, err := provider.RunAttempt(context.Background(), codexAttemptRequest(item, "do the work"))
	if err == nil {
		t.Fatal("expected RunAttempt to fail when the app-server process crashes mid-turn")
	}
	found := false
	for _, f := range result.WorkspaceDelta.Added {
		if f.Path == "changed-before-crash.txt" {
			found = true
		}
	}
	if !found {
		t.Fatalf("WorkspaceDelta = %#v, want it to include changed-before-crash.txt despite the crash", result.WorkspaceDelta)
	}
}

// TestCodexTimeoutDoesNotReportSuccess proves §16: an attempt that exceeds
// its own timeout must never be reported as a success — no CanonicalResult,
// no ResultProposal, and a classified provider_timeout error.
func TestCodexTimeoutDoesNotReportSuccess(t *testing.T) {
	workspace := newCodexWorkspace(t)
	_, provider, item, _ := newCodexHarness(t, workspace, []fakeCodexStep{
		codexInitializeStep(t),
		codexThreadStartStep(t, "thread-timeout", workspace),
		{Result: rawJSON(t, map[string]any{"turn": map[string]any{"id": "turn-timeout"}})}, // acked, turn/completed never arrives
	})

	request := codexAttemptRequest(item, "do the work")
	request.Timeout = 200 * time.Millisecond
	result, err := provider.RunAttempt(context.Background(), request)
	if err == nil {
		t.Fatal("expected RunAttempt to fail once its own timeout elapsed")
	}
	var provErr *CodexProviderError
	if !errors.As(err, &provErr) || provErr.Class != CodexFailureTimeout {
		t.Fatalf("err = %v, want a classified %q failure", err, CodexFailureTimeout)
	}
	if result.CanonicalResult != nil || result.ResultProposal != nil {
		t.Fatalf("result = %#v, want no proposal/canonical result reported on timeout", result)
	}
}

// Phase 6 tests (spec.md §36 PR-13/PR-14): result-only protocol repair and
// crash/restart reconciliation.

// codexRepairScript builds the 6-request script every result-only-repair
// test shares: the original turn's init/thread-start/turn-completion,
// followed by the repair process's own init/thread-resume/turn-completion
// (the fake server continues consuming this same list across both
// processes — see codex_fake_server_test.go's RawLogPath cursor). Real
// turn/completed notifications carry the final answer inline, so there is
// no separate thread/read step. originalFinalOutput and repairFinalOutput
// are each test's only variable.
func codexRepairScript(t *testing.T, workspace, threadID, originalFinalOutput, repairFinalOutput string, originalWrite, repairWrite fakeCodexStep) []fakeCodexStep {
	original := fakeCodexTurnCompletedStep(t, "turn-original", originalFinalOutput)
	original.WriteFile, original.WriteFileContent = originalWrite.WriteFile, originalWrite.WriteFileContent
	repair := fakeCodexTurnCompletedStep(t, "turn-repair", repairFinalOutput)
	repair.WriteFile, repair.WriteFileContent = repairWrite.WriteFile, repairWrite.WriteFileContent
	return []fakeCodexStep{
		codexInitializeStep(t),
		codexThreadStartStep(t, threadID, workspace),
		original,
		codexInitializeStep(t),
		codexThreadResumeStep(t, threadID, workspace, codexSandboxReadOnly),
		repair,
	}
}

// TestCodexResultRepairIsReadOnly proves §23: the repair resume requests a
// read-only sandbox, never the original task's (here, workspace-write)
// sandbox.
func TestCodexResultRepairIsReadOnly(t *testing.T) {
	workspace := newCodexWorkspace(t)
	_, provider, item, rawLogPath := newCodexHarness(t, workspace, codexRepairScript(t, workspace, "thread-repair-ro",
		"", validProposalJSON(""), fakeCodexStep{}, fakeCodexStep{}))

	result, err := provider.RunAttempt(context.Background(), codexAttemptRequest(item, "do the work"))
	if err != nil {
		t.Fatalf("RunAttempt: %v", err)
	}
	if result.CanonicalResult == nil {
		t.Fatalf("CanonicalResult = nil, want the repair proposal to become the canonical result")
	}

	resumeCall := findRawCall(t, readRawCalls(t, rawLogPath), codexMethodThreadResume)
	var params struct {
		Sandbox string `json:"sandbox"`
	}
	if err := json.Unmarshal(resumeCall["params"], &params); err != nil {
		t.Fatal(err)
	}
	if params.Sandbox != codexSandboxReadOnly {
		t.Fatalf("repair thread/resume sandbox = %q, want %q", params.Sandbox, codexSandboxReadOnly)
	}
}

// TestCodexResultRepairDoesNotReplayWorkspaceWrite proves §23: a repair
// proposal cannot retroactively change observed workspace history. Even if
// the repair turn writes a file (an app-server disobeying the read-only
// instruction, worst case), Hufu's canonical WorkspaceDelta reflects only
// what was already frozen before the repair began.
func TestCodexResultRepairDoesNotReplayWorkspaceWrite(t *testing.T) {
	workspace := newCodexWorkspace(t)
	_, provider, item, _ := newCodexHarness(t, workspace, codexRepairScript(t, workspace, "thread-repair-freeze",
		"", validProposalJSON(""),
		fakeCodexStep{WriteFile: "before-repair.txt", WriteFileContent: "written by the original turn"},
		fakeCodexStep{WriteFile: "during-repair.txt", WriteFileContent: "written despite the read-only instruction"},
	))

	result, err := provider.RunAttempt(context.Background(), codexAttemptRequest(item, "do the work"))
	if err != nil {
		t.Fatalf("RunAttempt: %v", err)
	}
	added := make(map[string]bool, len(result.WorkspaceDelta.Added))
	for _, f := range result.WorkspaceDelta.Added {
		added[f.Path] = true
	}
	if !added["before-repair.txt"] {
		t.Fatalf("WorkspaceDelta.Added = %v, want the original turn's file frozen as evidence", result.WorkspaceDelta.Added)
	}
	if added["during-repair.txt"] {
		t.Fatalf("WorkspaceDelta.Added = %v, want the repair turn's file excluded (frozen history)", result.WorkspaceDelta.Added)
	}
}

// TestCodexSecondInvalidResultBecomesProtocolIncomplete proves §23: when the
// repair turn ALSO fails to produce a valid proposal, Hufu does not attempt
// a further repair — it classifies the attempt as protocol-incomplete and
// reports no fabricated success.
func TestCodexSecondInvalidResultBecomesProtocolIncomplete(t *testing.T) {
	workspace := newCodexWorkspace(t)
	_, provider, item, _ := newCodexHarness(t, workspace, codexRepairScript(t, workspace, "thread-repair-fail",
		"", "", fakeCodexStep{}, fakeCodexStep{}))

	result, err := provider.RunAttempt(context.Background(), codexAttemptRequest(item, "do the work"))
	if err == nil {
		t.Fatal("expected RunAttempt to fail when both the original and repair turns produce no valid proposal")
	}
	var protoErr *CodexProtocolIncompleteError
	if !errors.As(err, &protoErr) {
		t.Fatalf("err = %v, want *CodexProtocolIncompleteError", err)
	}
	if result.CanonicalResult != nil || result.ResultProposal != nil {
		t.Fatalf("result = %#v, want no proposal/canonical result after a failed repair", result)
	}
}

// TestResumeDoesNotReplayUnsafeSideEffect proves §22.4/§33: an existing
// provider session binding never bypasses the side-effect admission gate —
// a credential-mutation task is rejected before any process or RPC activity,
// resume or not.
func TestResumeDoesNotReplayUnsafeSideEffect(t *testing.T) {
	workspace := newCodexWorkspace(t)
	_, provider, item, rawLogPath := newCodexHarness(t, workspace, nil)

	request := codexAttemptRequest(item, "do the work")
	request.Task.SideEffect = SideEffectCredential
	request.ProviderBinding = &ProviderBinding{Provider: "codex", SessionID: "thread-existing", ResumeSupported: true}

	result, err := provider.RunAttempt(context.Background(), request)
	if err == nil {
		t.Fatal("expected RunAttempt to reject a credential-mutation side effect even with an existing session binding")
	}
	var provErr *CodexProviderError
	if !errors.As(err, &provErr) || provErr.Class != CodexFailureUnavailable {
		t.Fatalf("err = %v, want a classified %q failure", err, CodexFailureUnavailable)
	}
	if result.ProviderSessionID != "" || result.CanonicalResult != nil {
		t.Fatalf("result = %#v, want no session/result activity for a rejected side effect", result)
	}
	if calls := readRawCalls(t, rawLogPath); len(calls) != 0 {
		t.Fatalf("recorded calls = %v, want no process/RPC activity before rejection", rawCallMethods(t, calls))
	}
}

// TestCrashAfterProviderSessionBeforeTurnTerminal proves §24.2: on resume, a
// file that already existed on disk before the crash (i.e. before this
// RunAttempt call's baseline snapshot) is reconciled as baseline, not
// credited as new evidence from the resumed turn — only what the resumed
// turn itself changes is new.
func TestCrashAfterProviderSessionBeforeTurnTerminal(t *testing.T) {
	workspace := newCodexWorkspace(t)
	if err := os.WriteFile(filepath.Join(workspace, "pre-crash-change.txt"), []byte("already there before the crash"), 0o644); err != nil {
		t.Fatal(err)
	}
	resumeStep := fakeCodexTurnCompletedStep(t, "turn-resume", validProposalJSON(""))
	resumeStep.WriteFile, resumeStep.WriteFileContent = "new-work.txt", "written after resuming"
	_, provider, item, _ := newCodexHarness(t, workspace, []fakeCodexStep{
		codexInitializeStep(t),
		codexThreadResumeStep(t, "thread-precrash", workspace, codexSandboxWorkspaceWrite),
		resumeStep,
	})

	request := codexAttemptRequest(item, "continue the work")
	request.Attempt = 2
	request.ProviderBinding = &ProviderBinding{Provider: "codex", SessionID: "thread-precrash", ResumeSupported: true}

	result, err := provider.RunAttempt(context.Background(), request)
	if err != nil {
		t.Fatalf("RunAttempt: %v", err)
	}
	added := make(map[string]bool, len(result.WorkspaceDelta.Added))
	for _, f := range result.WorkspaceDelta.Added {
		added[f.Path] = true
	}
	if added["pre-crash-change.txt"] {
		t.Fatalf("WorkspaceDelta.Added = %v, want the pre-crash file treated as baseline, not new evidence", result.WorkspaceDelta.Added)
	}
	if !added["new-work.txt"] {
		t.Fatalf("WorkspaceDelta.Added = %v, want the resumed turn's own new file", result.WorkspaceDelta.Added)
	}
}

// TestCrashAfterWorkspaceMutationBeforeVerification proves §24.3: Hufu never
// reuses a prior attempt's cached proposal/delta — a second, independent
// RunAttempt call (as Hufu's own retry policy would drive if verification
// failed after the first attempt) reconciles entirely fresh from what it
// itself observes.
func TestCrashAfterWorkspaceMutationBeforeVerification(t *testing.T) {
	workspace := newCodexWorkspace(t)

	firstStep := fakeCodexTurnCompletedStep(t, "turn-1", `{"status":"success","summary":"first pass done"}`)
	firstStep.WriteFile, firstStep.WriteFileContent = "first.txt", "first pass"
	_, provider1, item1, _ := newCodexHarness(t, workspace, []fakeCodexStep{
		codexInitializeStep(t),
		codexThreadStartStep(t, "thread-unverified", workspace),
		firstStep,
	})
	result1, err := provider1.RunAttempt(context.Background(), codexAttemptRequest(item1, "do the work"))
	if err != nil {
		t.Fatalf("attempt 1 RunAttempt: %v", err)
	}

	// A second, independent RunAttempt call — as Hufu's own retry policy
	// would issue after attempt 1's verification failed downstream, not
	// because this provider remembers anything from attempt 1.
	secondStep := fakeCodexTurnCompletedStep(t, "turn-2", `{"status":"success","summary":"second pass done"}`)
	secondStep.WriteFile, secondStep.WriteFileContent = "second.txt", "second pass"
	_, provider2, item2, _ := newCodexHarness(t, workspace, []fakeCodexStep{
		codexInitializeStep(t),
		codexThreadResumeStep(t, "thread-unverified", workspace, codexSandboxWorkspaceWrite),
		secondStep,
	})
	request2 := codexAttemptRequest(item2, "continue the work")
	request2.Attempt = 2
	request2.ProviderBinding = &ProviderBinding{Provider: "codex", SessionID: result1.ProviderSessionID, ResumeSupported: true}

	result2, err := provider2.RunAttempt(context.Background(), request2)
	if err != nil {
		t.Fatalf("attempt 2 RunAttempt: %v", err)
	}
	if result2.CanonicalResult == nil || result2.CanonicalResult.Summary == result1.CanonicalResult.Summary {
		t.Fatalf("attempt 2 CanonicalResult = %#v, want a fresh summary distinct from attempt 1's %q", result2.CanonicalResult, result1.CanonicalResult.Summary)
	}
	added := make(map[string]bool, len(result2.WorkspaceDelta.Added))
	for _, f := range result2.WorkspaceDelta.Added {
		added[f.Path] = true
	}
	if added["first.txt"] {
		t.Fatalf("attempt 2 WorkspaceDelta.Added = %v, want attempt 1's file excluded (already baseline)", result2.WorkspaceDelta.Added)
	}
	if !added["second.txt"] {
		t.Fatalf("attempt 2 WorkspaceDelta.Added = %v, want attempt 2's own new file", result2.WorkspaceDelta.Added)
	}
}

// TestCodexResumeAfterHufuRestart proves §24.2 end to end through the real
// EventStore: a durable provider session binding, written before a
// simulated Hufu restart, survives replay into a brand-new
// Coordinator/TaskTracker with no in-memory state carried over, and driving
// a further attempt from that replayed binding alone resumes the existing
// thread rather than starting a new one.
func TestCodexResumeAfterHufuRestart(t *testing.T) {
	workspace := newCodexWorkspace(t)

	c1, provider1, item1, _ := newCodexHarness(t, workspace, []fakeCodexStep{
		codexInitializeStep(t),
		codexThreadStartStep(t, "thread-restart", workspace),
		fakeCodexTurnCompletedStep(t, "turn-1", validProposalJSON("")),
	})
	// The harness adds item1 straight to the in-memory TodoList (a testing
	// shortcut), never through a durable task_created event. Append one now
	// so the event log this test replays from is a faithful stand-in for the
	// real event-first admission boundary (§7.2).
	createdPayload, err := json.Marshal(taskTransitionPayload(item1))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c1.EventJournal().Append(context.Background(), RunEvent{Type: string(EventTaskCreated), Actor: "coordinator", TaskID: item1.ID, Payload: createdPayload}); err != nil {
		t.Fatal(err)
	}
	if _, err := provider1.RunAttempt(context.Background(), codexAttemptRequest(item1, "do the work")); err != nil {
		t.Fatalf("attempt 1 RunAttempt: %v", err)
	}

	// Simulate a Hufu restart: replay the durable event log from scratch,
	// with no in-memory state carried over, and recover the binding purely
	// from what is on disk (§24.2 step 1).
	store, err := OpenEventStore(workspace)
	if err != nil {
		t.Fatal(err)
	}
	events, err := store.ReadEvents()
	if err != nil {
		t.Fatal(err)
	}
	_ = store.Close()
	replayed := ReduceToTodoList(events)
	var binding *ProviderBinding
	for _, it := range replayed {
		if it.ID == item1.ID {
			binding = it.ProviderBinding
		}
	}
	if binding == nil || binding.SessionID != "thread-restart" {
		t.Fatalf("replayed ProviderBinding = %#v, want a durable thread-restart session", binding)
	}

	// Attempt 2, after the simulated restart: a fresh Coordinator/provider,
	// driven only by the replayed binding above.
	_, provider2, item2, rawLogPath2 := newCodexHarness(t, workspace, []fakeCodexStep{
		codexInitializeStep(t),
		codexThreadResumeStep(t, "thread-restart", workspace, codexSandboxWorkspaceWrite),
		fakeCodexTurnCompletedStep(t, "turn-2", validProposalJSON("")),
	})
	request := codexAttemptRequest(item2, "continue the work")
	request.Attempt = 2
	request.ProviderBinding = binding

	result, err := provider2.RunAttempt(context.Background(), request)
	if err != nil {
		t.Fatalf("attempt 2 RunAttempt: %v", err)
	}
	if result.ProviderSessionID != "thread-restart" {
		t.Fatalf("ProviderSessionID = %q, want the resumed thread-restart session", result.ProviderSessionID)
	}
	methods := rawCallMethods(t, readRawCalls(t, rawLogPath2))
	sawResume, sawStart := false, false
	for _, m := range methods {
		switch m {
		case codexMethodThreadResume:
			sawResume = true
		case codexMethodThreadStart:
			sawStart = true
		}
	}
	if !sawResume {
		t.Fatalf("recorded calls = %v, want a thread/resume after the simulated restart", methods)
	}
	if sawStart {
		t.Fatalf("recorded calls = %v, want no fresh thread/start — the durable session must be reused", methods)
	}
}

// TestCodexRejectsExtraModelFanout proves §29: a task whose ModelTopology
// has more than one leaf (Hufu's own extra-model fanout) fails closed before
// any process or RPC activity, rather than silently running a single Codex
// occurrence as if it had covered every leaf — v1 has no implementation
// routing Hufu's local fanout concept into Codex's own internal behavior.
func TestCodexRejectsExtraModelFanout(t *testing.T) {
	workspace := newCodexWorkspace(t)
	_, provider, item, rawLogPath := newCodexHarness(t, workspace, nil)

	request := codexAttemptRequest(item, "do the work")
	request.Task.ModelTopology = []string{"gpt-5-codex", "gpt-5-codex-mini"}

	result, err := provider.RunAttempt(context.Background(), request)
	if err == nil {
		t.Fatal("expected RunAttempt to reject a multi-leaf ModelTopology")
	}
	var provErr *CodexProviderError
	if !errors.As(err, &provErr) || provErr.Class != CodexFailureUnavailable {
		t.Fatalf("err = %v, want a classified %q failure", err, CodexFailureUnavailable)
	}
	if result.CanonicalResult != nil {
		t.Fatalf("result = %#v, want no canonical result for a rejected fanout request", result)
	}
	if calls := readRawCalls(t, rawLogPath); len(calls) != 0 {
		t.Fatalf("recorded calls = %v, want no process/RPC activity before rejection", rawCallMethods(t, calls))
	}
}

// TestCodexAllowsSingleLeafModelTopology proves the §29 guard only rejects
// an actual fanout (more than one leaf) — a normal single-model task, which
// happens to carry a length-1 ModelTopology (the common case set by
// initialTaskModelTopology), must still run normally.
func TestCodexAllowsSingleLeafModelTopology(t *testing.T) {
	workspace := newCodexWorkspace(t)
	_, provider, item, _ := newCodexHarness(t, workspace, []fakeCodexStep{
		codexInitializeStep(t),
		codexThreadStartStep(t, "thread-single-leaf", workspace),
		fakeCodexTurnCompletedStep(t, "turn-1", validProposalJSON("")),
	})

	request := codexAttemptRequest(item, "do the work")
	request.Task.ModelTopology = []string{"gpt-5-codex"}

	result, err := provider.RunAttempt(context.Background(), request)
	if err != nil {
		t.Fatalf("RunAttempt: %v", err)
	}
	if result.CanonicalResult == nil {
		t.Fatalf("result = %#v, want a canonical result for a single-leaf topology", result)
	}
}

// TestCodexPreflightRejectsMissingExecutable exercises §25's "preflight
// failure MUST occur before the task enters provider execution": a
// nonexistent executable must fail RunAttempt without any process/RPC
// activity, via the plain LookPath check in codexRunCheapPreflightChecks.
func TestCodexPreflightRejectsMissingExecutable(t *testing.T) {
	workspace := newCodexWorkspace(t)
	c := &Coordinator{
		session:      &TeamSession{Workspace: workspace, Config: agent.TeamConfig{Name: "preflight-test"}},
		projectDir:   workspace,
		taskTracker:  NewTaskTracker(),
		sessionData:  NewSession(),
		reportStatus: func(StatusEvent) {},
	}
	provider := NewCodexSubagentProvider(c, "codex", agent.SubagentProviderConfig{
		Type:    codexAppServerProviderType,
		Command: []string{"definitely-not-a-real-codex-binary-xyz"},
	})
	item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "x", Goal: "x"}})[0]

	_, err := provider.RunAttempt(context.Background(), codexAttemptRequest(item, "do the work"))
	if err == nil {
		t.Fatal("expected RunAttempt to fail preflight for a missing executable")
	}
	var provErr *CodexProviderError
	if !errors.As(err, &provErr) || provErr.Class != CodexFailureUnavailable {
		t.Fatalf("err = %v, want a classified %q failure", err, CodexFailureUnavailable)
	}
}

// TestCodexPreflightRejectsMissingCodexHomeAuth exercises the CODEX_HOME
// authentication heuristic: when CODEX_HOME is inherited but has no
// auth.json, preflight must fail closed before any subprocess is started —
// the configured Command here (os.Args[0]) is never actually invoked, since
// there is no fake-server script wired up.
func TestCodexPreflightRejectsMissingCodexHomeAuth(t *testing.T) {
	workspace := newCodexWorkspace(t)
	t.Setenv("CODEX_HOME", t.TempDir())

	c := &Coordinator{
		session:      &TeamSession{Workspace: workspace, Config: agent.TeamConfig{Name: "preflight-test"}},
		projectDir:   workspace,
		taskTracker:  NewTaskTracker(),
		sessionData:  NewSession(),
		reportStatus: func(StatusEvent) {},
	}
	provider := NewCodexSubagentProvider(c, "codex", agent.SubagentProviderConfig{
		Type:       codexAppServerProviderType,
		Command:    []string{os.Args[0]},
		InheritEnv: []string{"CODEX_HOME"},
	})
	item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "x", Goal: "x"}})[0]

	_, err := provider.RunAttempt(context.Background(), codexAttemptRequest(item, "do the work"))
	if err == nil {
		t.Fatal("expected RunAttempt to fail preflight for CODEX_HOME with no auth.json")
	}
	var provErr *CodexProviderError
	if !errors.As(err, &provErr) || provErr.Class != CodexFailureUnavailable {
		t.Fatalf("err = %v, want a classified %q failure", err, CodexFailureUnavailable)
	}
}

// TestCodexPreflightCachesAcrossAttempts confirms §25's "cacheable by
// provider binary/version" behavior: once a provider instance has passed
// preflight, a later attempt on the same instance must reuse the cached
// result rather than re-running the CODEX_HOME/auth.json check — proven here
// by deleting auth.json between two attempts and observing the second one
// still succeeds.
func TestCodexPreflightCachesAcrossAttempts(t *testing.T) {
	workspace := newCodexWorkspace(t)
	codexHome := t.TempDir()
	authPath := filepath.Join(codexHome, "auth.json")
	if err := os.WriteFile(authPath, []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_HOME", codexHome)

	scriptDir := t.TempDir()
	scriptPath := filepath.Join(scriptDir, "script.json")
	rawLogPath := filepath.Join(scriptDir, "raw_calls.log")
	steps := []fakeCodexStep{
		codexInitializeStep(t),
		codexThreadStartStep(t, "thread-preflight-cache-1", workspace),
		fakeCodexTurnCompletedStep(t, "turn-1", validProposalJSON("")),
		codexInitializeStep(t),
		codexThreadStartStep(t, "thread-preflight-cache-2", workspace),
		fakeCodexTurnCompletedStep(t, "turn-2", validProposalJSON("")),
	}
	data, err := json.Marshal(fakeCodexScript{Steps: steps, RawLogPath: rawLogPath})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(scriptPath, data, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv(fakeCodexServerScriptEnvVar, scriptPath)

	store, err := NewEventStore(workspace, "codex-attempt-run", "codex-attempt-session")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	c := &Coordinator{
		session:        &TeamSession{Workspace: workspace, Config: agent.TeamConfig{Name: "preflight-cache-test"}},
		projectDir:     workspace,
		taskTracker:    NewTaskTracker(),
		sessionData:    NewSession(),
		eventStore:     store,
		executionRunID: "codex-attempt-run",
		reportStatus:   func(StatusEvent) {},
	}
	provider := NewCodexSubagentProvider(c, "codex", agent.SubagentProviderConfig{
		Type:           codexAppServerProviderType,
		Command:        []string{os.Args[0]},
		InheritEnv:     []string{fakeCodexServerScriptEnvVar, "CODEX_HOME"},
		StartupTimeout: "5s", InterruptGrace: "200ms", ShutdownGrace: "200ms",
	})
	item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "codex attempt", Goal: "codex attempt"}})[0]

	result, err := provider.RunAttempt(context.Background(), codexAttemptRequest(item, "do the work"))
	if err != nil {
		t.Fatalf("first RunAttempt: %v", err)
	}
	if result.CanonicalResult == nil {
		t.Fatal("first RunAttempt: want a canonical result")
	}

	if err := os.Remove(authPath); err != nil {
		t.Fatal(err)
	}

	item2 := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "codex attempt 2", Goal: "codex attempt 2"}})[0]
	request2 := AttemptRequest{
		RunID: "codex-attempt-run", TaskID: item2.ID, Attempt: 1,
		Task:   TaskDef{Agent: "worker", Goal: "codex attempt", SideEffect: SideEffectWorkspaceWrite},
		Prompt: "do the work", ModelID: "gpt-5-codex", Provider: "codex",
	}
	result2, err := provider.RunAttempt(context.Background(), request2)
	if err != nil {
		t.Fatalf("second RunAttempt (expected cached preflight to skip auth.json re-check): %v", err)
	}
	if result2.CanonicalResult == nil {
		t.Fatal("second RunAttempt: want a canonical result")
	}
}

// TestCodexHomeLooksAuthenticatedFallsBackToDefaultWhenUnset proves a fix
// found by actually running the §38 live smoke suite against a real
// account: every scenario failed preflight with "CODEX_HOME is not set"
// even though a real, authenticated CODEX_HOME existed at the real CLI's own
// default location — because the environment simply never exported
// CODEX_HOME explicitly. The real codex CLI itself defaults to
// $HOME/.codex, so preflight must apply the same fallback rather than
// treating an unset CODEX_HOME as an outright failure.
func TestCodexHomeLooksAuthenticatedFallsBackToDefaultWhenUnset(t *testing.T) {
	fakeHome := t.TempDir()
	if err := os.MkdirAll(filepath.Join(fakeHome, ".codex"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fakeHome, ".codex", "auth.json"), []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", fakeHome)
	t.Setenv("CODEX_HOME", "")

	if err := codexHomeLooksAuthenticated(); err != nil {
		t.Fatalf("codexHomeLooksAuthenticated() = %v, want it to fall back to $HOME/.codex and succeed", err)
	}
}

// TestCodexHomeLooksAuthenticatedFallbackFailsWithoutAuthFile proves the
// fallback still fails closed — it is a real check, not a bypass — when the
// default $HOME/.codex has no auth.json either.
func TestCodexHomeLooksAuthenticatedFallbackFailsWithoutAuthFile(t *testing.T) {
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)
	t.Setenv("CODEX_HOME", "")

	if err := codexHomeLooksAuthenticated(); err == nil {
		t.Fatal("codexHomeLooksAuthenticated() = nil, want an error when the default $HOME/.codex has no auth.json")
	}
}
