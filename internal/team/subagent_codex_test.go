package team

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
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

func newCodexHarness(t *testing.T, workspace string, steps []fakeCodexStep) (*Coordinator, *CodexSubagentProvider, *TodoItem) {
	t.Helper()
	scriptDir := t.TempDir()
	scriptPath := filepath.Join(scriptDir, "script.json")
	data, err := json.Marshal(fakeCodexScript{Steps: steps})
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
	return c, provider, item
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
	return fakeCodexStep{Result: rawJSON(t, map[string]any{"protocol_version": "v2", "server_version": "fake-1.0"})}
}

func codexThreadStartStep(t *testing.T, threadID, cwd string) fakeCodexStep {
	return fakeCodexStep{Result: rawJSON(t, map[string]any{"thread_id": threadID, "cwd": cwd, "model": "gpt-5-codex", "sandbox": "workspace-write"})}
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
	c, provider, item := newCodexHarness(t, workspace, []fakeCodexStep{
		codexInitializeStep(t),
		codexThreadStartStep(t, "thread-1", workspace),
		{
			Result:        rawJSON(t, map[string]any{"turn_id": "turn-1"}),
			Notifications: []fakeCodexNotification{{Method: "turn/completed", Params: rawJSON(t, map[string]any{"turn_id": "turn-1"})}},
		},
		{Result: rawJSON(t, map[string]any{"final_output": validProposalJSON("")})},
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
	c, provider, item := newCodexHarness(t, workspace, []fakeCodexStep{
		codexInitializeStep(t),
		codexThreadStartStep(t, "thread-cancel", workspace),
		{Result: rawJSON(t, map[string]any{"turn_id": "turn-cancel"})}, // acked, but turn/completed never arrives
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
	_, provider, item := newCodexHarness(t, workspace, []fakeCodexStep{
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
	_, provider, item := newCodexHarness(t, workspace, []fakeCodexStep{
		codexInitializeStep(t),
		codexThreadStartStep(t, "thread-timeout", workspace),
		{Result: rawJSON(t, map[string]any{"turn_id": "turn-timeout"})}, // acked, turn/completed never arrives
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
