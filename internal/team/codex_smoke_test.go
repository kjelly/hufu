package team

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/kjelly/hufu/internal/agent"
)

// Real Codex smoke tests (spec.md §38): opt-in, driven against the actually
// installed `codex` binary and a real, pre-authenticated CODEX_HOME — never
// run by default, never required by CI. Enable with:
//
//	HUFU_CODEX_SMOKE=1 go test ./internal/team -run CodexSmoke
//
// Every scenario here drives the exact same production RunAttempt path the
// fake-server-driven unit tests already exercise in detail (cancellation,
// crash/restart resume, protocol repair, completion-gate rejection); what
// this suite adds is confirmation that the real app-server, not a scripted
// stand-in, actually behaves the way those unit tests assume. Assertions are
// deliberately loose on model *content* (a live model's wording is not
// deterministic) and strict on protocol/safety outcomes (error class,
// workspace delta, session identity, rejection) — the properties Hufu's own
// invariants actually depend on.
const hufuCodexSmokeEnvVar = "HUFU_CODEX_SMOKE"

// hufuCodexSmokeModelEnvVar lets the smoke suite target whatever model the
// operator's own Codex account/plan actually supports. There is no single
// model name every account can use: a ChatGPT-plan login, for one real
// example hit running this suite for the first time, rejects "gpt-5-codex"
// outright ("not supported when using Codex with a ChatGPT account") even
// though it is otherwise fully authenticated, while accepting whatever
// CODEX_HOME/config.toml's own `model` default names instead.
const hufuCodexSmokeModelEnvVar = "HUFU_CODEX_SMOKE_MODEL"

func codexSmokeModelID() string {
	if m := strings.TrimSpace(os.Getenv(hufuCodexSmokeModelEnvVar)); m != "" {
		return m
	}
	return "gpt-5-codex"
}

// codexSmokeRequireReady enforces §38's smoke prerequisites (codex
// installed, CODEX_HOME pre-authenticated) and the opt-in gate, skipping
// with a clear reason rather than failing when any of them is unmet.
func codexSmokeRequireReady(t *testing.T) {
	t.Helper()
	if os.Getenv(hufuCodexSmokeEnvVar) != "1" {
		t.Skipf("skipping live Codex smoke test: set %s=1 to opt in (spec.md §38)", hufuCodexSmokeEnvVar)
	}
	if runtime.GOOS == "windows" {
		t.Skip("ProcessSupervisor has no Windows implementation yet (§16.2)")
	}
	if _, err := exec.LookPath("codex"); err != nil {
		t.Skipf("codex executable not found on PATH: %v", err)
	}
	home := strings.TrimSpace(os.Getenv("CODEX_HOME"))
	if home == "" {
		userHome, err := os.UserHomeDir()
		if err != nil {
			t.Skipf("cannot resolve default CODEX_HOME: %v", err)
		}
		home = filepath.Join(userHome, ".codex")
	}
	if _, err := os.Stat(filepath.Join(home, "auth.json")); err != nil {
		t.Skipf("CODEX_HOME %q has no auth.json (run `codex login` first): %v", home, err)
	}
}

// newCodexSmokeWorkspace builds the "explicit scratch repository" §38
// requires: a throwaway temp directory, never the Hufu source checkout,
// initialized as its own tiny git repo so a real coding task has normal
// repository context to work with.
func newCodexSmokeWorkspace(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	runSmokeGit(t, dir, "init", "-q")
	runSmokeGit(t, dir, "config", "user.email", "smoke@example.invalid")
	runSmokeGit(t, dir, "config", "user.name", "hufu-codex-smoke")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("scratch repository for a Hufu Codex smoke test\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runSmokeGit(t, dir, "add", "README.md")
	runSmokeGit(t, dir, "commit", "-q", "-m", "initial commit")
	return dir
}

func runSmokeGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// codexSmokeInheritEnv is the environment allowlist passed to the real
// codex app-server subprocess. execution_world.go's buildAllowlistedEnvironment
// only forwards names explicitly listed here — there is no implicit PATH or
// HOME — so the real binary needs each of these to locate itself, find
// tools it shells out to, and read its own auth/config.
var codexSmokeInheritEnv = []string{"PATH", "HOME", "CODEX_HOME", "TMPDIR"}

// newCodexSmokeHarness builds a real Coordinator/CodexSubagentProvider pair
// against the actually installed `codex` binary — the live counterpart of
// newCodexHarness, which drives a fake in-process stand-in instead.
func newCodexSmokeHarness(t *testing.T, workspace string) (*Coordinator, *CodexSubagentProvider) {
	t.Helper()
	store, err := NewEventStore(workspace, "codex-smoke-run", "codex-smoke-session")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	c := &Coordinator{
		session:        &TeamSession{Workspace: workspace, Config: agent.TeamConfig{Name: "codex-smoke"}},
		projectDir:     workspace,
		taskTracker:    NewTaskTracker(),
		sessionData:    NewSession(),
		eventStore:     store,
		executionRunID: "codex-smoke-run",
		reportStatus:   func(StatusEvent) {},
	}
	provider := NewCodexSubagentProvider(c, "codex", agent.SubagentProviderConfig{
		Type:           codexAppServerProviderType,
		Command:        []string{"codex", "app-server"},
		InheritEnv:     codexSmokeInheritEnv,
		StartupTimeout: "20s", InterruptGrace: "3s", ShutdownGrace: "5s",
	})
	return c, provider
}

func newCodexSmokeItem(c *Coordinator, goal string) *TodoItem {
	return c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: goal, Goal: goal}})[0]
}

// TestCodexSmokeReadOnlyInspect covers §38 scenario 1: a read-only task must
// complete with no workspace change at all, and the real app-server must
// accept a read-only sandbox.
func TestCodexSmokeReadOnlyInspect(t *testing.T) {
	codexSmokeRequireReady(t)
	workspace := newCodexSmokeWorkspace(t)
	c, provider := newCodexSmokeHarness(t, workspace)
	item := newCodexSmokeItem(c, "inspect the repository")

	request := AttemptRequest{
		RunID: "codex-smoke-run", TaskID: item.ID, Attempt: 1,
		Task:    TaskDef{Agent: "worker", Goal: "inspect the repository", SideEffect: SideEffectNone},
		Prompt:  "List the files in this repository and summarize what you see. Do not create, modify, or delete any files.",
		ModelID: codexSmokeModelID(), Provider: "codex", Timeout: 5 * time.Minute,
	}
	result, err := provider.RunAttempt(context.Background(), request)
	if err != nil {
		t.Fatalf("RunAttempt: %v", err)
	}
	if result.CanonicalResult == nil {
		t.Fatal("want a canonical result for a read-only inspect task")
	}
	if len(result.WorkspaceDelta.Added) != 0 || len(result.WorkspaceDelta.Modified) != 0 || len(result.WorkspaceDelta.Deleted) != 0 {
		t.Fatalf("WorkspaceDelta = %#v, want no changes for a read-only task", result.WorkspaceDelta)
	}
}

// TestCodexSmokeWorkspaceWriteCreatesFile covers §38 scenario 2: a
// workspace-write task must be able to actually create a file, and Hufu
// must observe that file in the attempt's WorkspaceDelta.
func TestCodexSmokeWorkspaceWriteCreatesFile(t *testing.T) {
	codexSmokeRequireReady(t)
	workspace := newCodexSmokeWorkspace(t)
	c, provider := newCodexSmokeHarness(t, workspace)
	item := newCodexSmokeItem(c, "create one file")

	request := AttemptRequest{
		RunID: "codex-smoke-run", TaskID: item.ID, Attempt: 1,
		Task:    TaskDef{Agent: "worker", Goal: "create one file", SideEffect: SideEffectWorkspaceWrite},
		Prompt:  "Create exactly one new file named smoke-output.txt containing the single line 'hufu codex smoke test'. Do not modify any other file.",
		ModelID: codexSmokeModelID(), Provider: "codex", Timeout: 5 * time.Minute,
	}
	result, err := provider.RunAttempt(context.Background(), request)
	if err != nil {
		t.Fatalf("RunAttempt: %v", err)
	}
	if result.CanonicalResult == nil {
		t.Fatal("want a canonical result for a workspace-write task")
	}
	found := false
	for _, f := range result.WorkspaceDelta.Added {
		if f.Path == "smoke-output.txt" {
			found = true
		}
	}
	if !found {
		t.Fatalf("WorkspaceDelta.Added = %v, want smoke-output.txt", result.WorkspaceDelta.Added)
	}
	if _, err := os.Stat(filepath.Join(workspace, "smoke-output.txt")); err != nil {
		t.Fatalf("smoke-output.txt not found on disk: %v", err)
	}
}

// TestCodexSmokeCompileAndTest covers §38 scenario 3: a task that asks the
// provider to run a real compile/test command against a small pre-seeded Go
// module in the scratch repository.
func TestCodexSmokeCompileAndTest(t *testing.T) {
	codexSmokeRequireReady(t)
	workspace := newCodexSmokeWorkspace(t)
	if err := os.WriteFile(filepath.Join(workspace, "go.mod"), []byte("module smoke\n\ngo 1.21\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	smokeTest := "package smoke\n\nimport \"testing\"\n\nfunc TestSmokeAlwaysPasses(t *testing.T) {}\n"
	if err := os.WriteFile(filepath.Join(workspace, "smoke_test.go"), []byte(smokeTest), 0o644); err != nil {
		t.Fatal(err)
	}
	runSmokeGit(t, workspace, "add", "go.mod", "smoke_test.go")
	runSmokeGit(t, workspace, "commit", "-q", "-m", "seed a trivially passing test")

	c, provider := newCodexSmokeHarness(t, workspace)
	item := newCodexSmokeItem(c, "run the test suite")

	request := AttemptRequest{
		RunID: "codex-smoke-run", TaskID: item.ID, Attempt: 1,
		Task:    TaskDef{Agent: "worker", Goal: "run the test suite", SideEffect: SideEffectWorkspaceWrite},
		Prompt:  "Run `go test ./...` in this repository and report whether it passed.",
		ModelID: codexSmokeModelID(), Provider: "codex", Timeout: 5 * time.Minute,
	}
	result, err := provider.RunAttempt(context.Background(), request)
	if err != nil {
		t.Fatalf("RunAttempt: %v", err)
	}
	if result.CanonicalResult == nil {
		t.Fatal("want a canonical result for a compile/test task")
	}
}

// TestCodexSmokeCancellation covers §38 scenario 4: cancelling the attempt's
// context mid-turn against a real, slow-running prompt must fail the attempt
// closed with a classified cancellation error, never a fabricated success.
func TestCodexSmokeCancellation(t *testing.T) {
	codexSmokeRequireReady(t)
	workspace := newCodexSmokeWorkspace(t)
	c, provider := newCodexSmokeHarness(t, workspace)
	item := newCodexSmokeItem(c, "write a long essay")

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(3 * time.Second)
		cancel()
	}()

	request := AttemptRequest{
		RunID: "codex-smoke-run", TaskID: item.ID, Attempt: 1,
		Task:    TaskDef{Agent: "worker", Goal: "write a long essay", SideEffect: SideEffectWorkspaceWrite},
		Prompt:  "Write a very long, detailed 3000-word essay explaining how a garbage collector works, then save it to essay.txt.",
		ModelID: codexSmokeModelID(), Provider: "codex", Timeout: 5 * time.Minute,
	}
	result, err := provider.RunAttempt(ctx, request)
	if err == nil {
		t.Fatal("expected RunAttempt to fail once the context was cancelled")
	}
	var provErr *CodexProviderError
	if errors.As(err, &provErr) && provErr.Class != CodexFailureCancelled {
		t.Fatalf("failure class = %q, want %q", provErr.Class, CodexFailureCancelled)
	}
	if result.CanonicalResult != nil {
		t.Fatalf("result = %#v, want no canonical result for a cancelled attempt", result)
	}
}

// TestCodexSmokeRetryUsesSameThread covers §38 scenario 5: a second attempt
// on the same provider instance, bound to the first attempt's session,
// resumes the existing thread instead of starting a new one.
func TestCodexSmokeRetryUsesSameThread(t *testing.T) {
	codexSmokeRequireReady(t)
	workspace := newCodexSmokeWorkspace(t)
	c, provider := newCodexSmokeHarness(t, workspace)
	item1 := newCodexSmokeItem(c, "remember a fact")

	request1 := AttemptRequest{
		RunID: "codex-smoke-run", TaskID: item1.ID, Attempt: 1,
		Task:    TaskDef{Agent: "worker", Goal: "remember a fact", SideEffect: SideEffectNone},
		Prompt:  "Remember the secret word 'ptarmigan' for the rest of this conversation. Do not create or modify any files.",
		ModelID: codexSmokeModelID(), Provider: "codex", Timeout: 5 * time.Minute,
	}
	result1, err := provider.RunAttempt(context.Background(), request1)
	if err != nil {
		t.Fatalf("attempt 1 RunAttempt: %v", err)
	}
	if result1.ProviderSessionID == "" {
		t.Fatal("attempt 1: want a non-empty ProviderSessionID to resume from")
	}

	item2 := newCodexSmokeItem(c, "recall the fact")
	request2 := AttemptRequest{
		RunID: "codex-smoke-run", TaskID: item2.ID, Attempt: 2,
		Task:    TaskDef{Agent: "worker", Goal: "recall the fact", SideEffect: SideEffectNone},
		Prompt:  "What was the secret word I asked you to remember? Do not create or modify any files.",
		ModelID: codexSmokeModelID(), Provider: "codex", Timeout: 5 * time.Minute,
		ProviderBinding: &ProviderBinding{Provider: "codex", SessionID: result1.ProviderSessionID, ResumeSupported: true},
	}
	result2, err := provider.RunAttempt(context.Background(), request2)
	if err != nil {
		t.Fatalf("attempt 2 RunAttempt: %v", err)
	}
	if result2.ProviderSessionID != result1.ProviderSessionID {
		t.Fatalf("attempt 2 ProviderSessionID = %q, want the same thread as attempt 1 (%q)", result2.ProviderSessionID, result1.ProviderSessionID)
	}
}

// TestCodexSmokeRestartThenResume covers §38 scenario 6: a brand-new
// provider/Coordinator instance (simulating a restarted Hufu process, or at
// least a restarted app-server) can resume a thread it never itself started,
// using only the durable session identity from a prior attempt.
func TestCodexSmokeRestartThenResume(t *testing.T) {
	codexSmokeRequireReady(t)
	workspace := newCodexSmokeWorkspace(t)

	c1, provider1 := newCodexSmokeHarness(t, workspace)
	item1 := newCodexSmokeItem(c1, "start the work")
	request1 := AttemptRequest{
		RunID: "codex-smoke-run", TaskID: item1.ID, Attempt: 1,
		Task:    TaskDef{Agent: "worker", Goal: "start the work", SideEffect: SideEffectNone},
		Prompt:  "Remember the secret word 'ptarmigan' for the rest of this conversation. Do not create or modify any files.",
		ModelID: codexSmokeModelID(), Provider: "codex", Timeout: 5 * time.Minute,
	}
	result1, err := provider1.RunAttempt(context.Background(), request1)
	if err != nil {
		t.Fatalf("attempt 1 RunAttempt: %v", err)
	}
	if result1.ProviderSessionID == "" {
		t.Fatal("attempt 1: want a non-empty ProviderSessionID to resume from")
	}

	// A brand-new provider/Coordinator pair — a fresh Go process would build
	// exactly this, from nothing but the durable session identity above.
	c2, provider2 := newCodexSmokeHarness(t, workspace)
	item2 := newCodexSmokeItem(c2, "continue after restart")
	request2 := AttemptRequest{
		RunID: "codex-smoke-run", TaskID: item2.ID, Attempt: 2,
		Task:    TaskDef{Agent: "worker", Goal: "continue after restart", SideEffect: SideEffectNone},
		Prompt:  "What was the secret word I asked you to remember? Do not create or modify any files.",
		ModelID: codexSmokeModelID(), Provider: "codex", Timeout: 5 * time.Minute,
		ProviderBinding: &ProviderBinding{Provider: "codex", SessionID: result1.ProviderSessionID, ResumeSupported: true},
	}
	result2, err := provider2.RunAttempt(context.Background(), request2)
	if err != nil {
		t.Fatalf("attempt 2 (post-restart) RunAttempt: %v", err)
	}
	if result2.ProviderSessionID != result1.ProviderSessionID {
		t.Fatalf("attempt 2 ProviderSessionID = %q, want the same thread as attempt 1 (%q)", result2.ProviderSessionID, result1.ProviderSessionID)
	}
}

// TestCodexSmokeInvalidArtifactProposalRejected covers §38 scenario 7: when
// a grounded-result task's proposal names a file that was never actually
// created or modified, Hufu's own canonicalizer must reject the attempt
// rather than accept the provider's unverified claim — the real live
// counterpart of provider_claimed_missing_file (§9.4).
func TestCodexSmokeInvalidArtifactProposalRejected(t *testing.T) {
	codexSmokeRequireReady(t)
	workspace := newCodexSmokeWorkspace(t)
	c, provider := newCodexSmokeHarness(t, workspace)
	item := newCodexSmokeItem(c, "report a fabricated file")

	request := AttemptRequest{
		RunID: "codex-smoke-run", TaskID: item.ID, Attempt: 1,
		Task: TaskDef{
			Agent: "worker", Goal: "report a fabricated file", SideEffect: SideEffectNone,
			Execution: ExecutionContract{RequiresGroundedResult: true},
		},
		Prompt: "Report success. In your final response's proposed_files field, include exactly one entry with path " +
			"'this-file-does-not-exist-smoke-test.txt' and no other proposed files. Do NOT actually create this file " +
			"or any other file — only claim it in proposed_files.",
		ModelID: codexSmokeModelID(), Provider: "codex", Timeout: 5 * time.Minute,
	}
	result, err := provider.RunAttempt(context.Background(), request)
	if err == nil {
		t.Fatal("expected RunAttempt to reject a proposal claiming a nonexistent artifact under a grounded-result requirement")
	}
	if result.CanonicalResult != nil {
		t.Fatalf("result = %#v, want no canonical result for a rejected fabricated-artifact proposal", result)
	}
}
