package team

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kjelly/hufu/internal/agent"
)

// This file drives Codex through the FULL production dispatch path —
// Coordinator.executeTask -> SubagentRegistry().Resolve ->
// CodexSubagentProvider.RunAttempt -> coordinator_task_run.go's own
// verification/completion-gate logic — rather than calling RunAttempt
// directly the way subagent_codex_test.go's Phase 5/6 tests do. It exists
// specifically to close spec.md §37's last two mandatory-matrix gaps, both
// of which are only observable through this full path: whether a task
// reaches TaskDone or retries lives entirely in coordinator_task_run.go,
// which never runs at all when a test calls RunAttempt on its own.

// newCodexE2ECoordinatorBase builds a real, EventStore-backed Coordinator
// with "codex" registered as a SubagentProvider (via the fake app-server
// fixture), but admits no task — callers that need a durable, pre-admitted
// occurrence (coordinated dispatch) use newCodexEndToEndCoordinator below;
// callers that need RunDirectAgent to perform its own admission (the direct
// fast path) use this directly.
func newCodexE2ECoordinatorBase(t *testing.T, workspace string, steps []fakeCodexStep, worker *agent.AgentDef) *Coordinator {
	t.Helper()
	scriptDir := t.TempDir()
	scriptPath := filepath.Join(scriptDir, "script.json")
	rawLogPath := filepath.Join(scriptDir, "raw_calls.log")
	// RawLogPath is what lets a second real attempt's fresh subprocess
	// continue consuming this script where the first attempt's process left
	// off, instead of restarting from step 0 — see codex_fake_server_test.go.
	data, err := json.Marshal(fakeCodexScript{Steps: steps, RawLogPath: rawLogPath})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(scriptPath, data, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv(fakeCodexServerScriptEnvVar, scriptPath)

	runID := "codex-e2e-run"
	store, err := NewEventStore(workspace, runID, "codex-e2e-session")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	session := &TeamSession{
		Dir: workspace, Workspace: workspace,
		Config: agent.TeamConfig{
			Name: "codex-e2e",
			SubagentProviders: map[string]agent.SubagentProviderConfig{
				"codex": {
					Type: codexAppServerProviderType, Command: []string{os.Args[0]},
					InheritEnv:     []string{fakeCodexServerScriptEnvVar},
					StartupTimeout: "5s", InterruptGrace: "200ms", ShutdownGrace: "200ms",
				},
			},
		},
		Agents: map[string]*agent.AgentDef{worker.Name: worker},
	}
	return &Coordinator{
		session: session, projectDir: workspace,
		taskTracker: NewTaskTracker(), sessionData: NewSession(), sessionTime: time.Now(),
		eventStore: store, executionRunID: runID, reportStatus: func(StatusEvent) {},
	}
}

// newCodexEndToEndCoordinator additionally admits worker durably through
// admitCodexE2ETask below — so the resulting Todo is a genuine durable task
// occurrence, not an ephemeral in-memory one, ready for a direct
// c.executeTask call.
func newCodexEndToEndCoordinator(t *testing.T, workspace string, steps []fakeCodexStep, worker *agent.AgentDef, verify *VerificationSpec) (*Coordinator, *TodoItem) {
	t.Helper()
	c := newCodexE2ECoordinatorBase(t, workspace, steps, worker)
	item := admitCodexE2ETask(t, c, worker, verify)
	return c, item
}

// admitCodexE2ETask durably admits a task the same way admitSubagentBaselineTask
// does (spec.md §37's "genuine durable task occurrence", not an ephemeral
// in-memory one), but with SideEffect explicitly workspace_write: every
// fake-server script in this file scripts a workspace-write sandbox
// (matching subagent_codex_test.go's own convention), and
// codexStartOrResumeThread fails closed on any sandbox mismatch (§14.1) — an
// admitted task whose SideEffect defaults to none/read-only would never
// reach a scripted turn at all.
func admitCodexE2ETask(t *testing.T, c *Coordinator, worker *agent.AgentDef, verify *VerificationSpec) *TodoItem {
	t.Helper()
	ids := c.taskTracker.TodoList().ReserveIDs(1)
	resolvedModel := c.resolveAgentModel(worker, "")
	canonical, err := c.canonicalizeTaskOccurrence(TaskDef{Agent: worker.Name}, worker, resolvedModel)
	if err != nil {
		t.Fatalf("canonicalizeTaskOccurrence: %v", err)
	}
	spec := TodoSpec{
		Agent: worker.Name, Desc: "baseline worker task", Goal: "baseline worker task",
		Model: canonical.Model, ModelTopology: initialTaskModelTopology(worker, canonical.Model),
		ExecutionTarget: canonical.ResolvedExecutionTarget, ExecutionTopology: canonical.ExecutionTopology,
		Source: TaskSourceCoordinator, Recovery: RecoveryRetry, SideEffect: SideEffectWorkspaceWrite,
		Execution:        ExecutionContract{RequiresResult: true},
		VerifySpec:       verify,
		SubagentProvider: canonical.SubagentProvider,
	}
	projection, err := taskOccurrenceProjectionFromSpec(spec, ids[0])
	if err != nil {
		t.Fatalf("taskOccurrenceProjectionFromSpec: %v", err)
	}
	if _, err := c.admitTaskOccurrence(context.Background(), projection, ids[0], 1); err != nil {
		t.Fatalf("admitTaskOccurrence: %v", err)
	}
	items, err := c.CommitTaskCreationResolved(context.Background(), []TodoSpec{spec}, ids)
	if err != nil {
		t.Fatalf("CommitTaskCreationResolved: %v", err)
	}
	return items[0]
}

// TestCodexSuccessStillNeedsCompletionGate proves §21/§40: even when Codex's
// own proposal claims success, a task only reaches TaskDone if it also
// passes Hufu's own objective verification — Codex's self-report is never a
// shortcut past the completion gate. A single attempt is enough to prove
// this (MaxRetries: 0), since the point is that Hufu's own gate ran at all,
// not what happens after it fails.
func TestCodexSuccessStillNeedsCompletionGate(t *testing.T) {
	workspace := newCodexWorkspace(t)
	worker := &agent.AgentDef{
		Name: "worker", Role: "worker", SubagentProvider: "codex", MaxRetries: 0,
		Generation: agent.GenerationParams{Model: "gpt-5-codex"},
	}
	verify := &VerificationSpec{Type: VerifyCommandExit, Command: "false"}
	c, item := newCodexEndToEndCoordinator(t, workspace, []fakeCodexStep{
		codexInitializeStep(t),
		codexThreadStartStep(t, "thread-gate", workspace),
		fakeCodexTurnCompletedStep(t, "turn-1", validProposalJSON("")),
	}, worker, verify)

	task := TaskDef{
		Agent: worker.Name, Goal: "baseline worker task", SideEffect: SideEffectWorkspaceWrite,
		Execution: ExecutionContract{RequiresResult: true}, VerifySpec: verify,
	}
	if _, err := c.executeTask(context.Background(), task, item.ID); err == nil {
		t.Fatal("expected executeTask to fail when Hufu's own verification rejects a Codex-reported success")
	}
	if got := c.taskTracker.TodoList().Items()[0].Status; got == TaskDone {
		t.Fatalf("task status = %s, want anything but done — Codex's self-reported success must not bypass Hufu's verify gate", got)
	}
}

// TestFailedCodexVerificationRetriesThroughHufu proves §22.1/§37: when a
// Codex-driven attempt fails Hufu's own verification, the decision to retry
// — and the retry itself — is Hufu's, not Codex's. The fake server script
// spans two real, separate app-server processes (one per attempt, per
// §13.1); the second must resume the same durable thread rather than start
// fresh (§22.2), and only the retry's own passing verification result must
// reach TaskDone.
func TestFailedCodexVerificationRetriesThroughHufu(t *testing.T) {
	workspace := newCodexWorkspace(t)
	worker := &agent.AgentDef{
		Name: "worker", Role: "worker", SubagentProvider: "codex", MaxRetries: 1,
		Generation: agent.GenerationParams{Model: "gpt-5-codex"},
	}
	verify := &VerificationSpec{
		Type: VerifyTaskResultAssert,
		TaskResultAssertions: []agent.TaskResultAssertion{
			{Pointer: "/summary", Op: "equals", Value: "RETRY_OK"},
		},
	}
	firstAttempt := fakeCodexTurnCompletedStep(t, "turn-1", `{"status":"success","summary":"attempt one, still wrong"}`)
	secondAttempt := fakeCodexTurnCompletedStep(t, "turn-2", `{"status":"success","summary":"RETRY_OK"}`)
	c, item := newCodexEndToEndCoordinator(t, workspace, []fakeCodexStep{
		codexInitializeStep(t),
		codexThreadStartStep(t, "thread-retry", workspace),
		firstAttempt,
		codexInitializeStep(t),
		codexThreadResumeStep(t, "thread-retry", workspace, "workspace-write"),
		secondAttempt,
	}, worker, verify)

	task := TaskDef{
		Agent: worker.Name, Goal: "baseline worker task", SideEffect: SideEffectWorkspaceWrite,
		Execution: ExecutionContract{RequiresResult: true}, VerifySpec: verify,
	}
	if _, err := c.executeTask(context.Background(), task, item.ID); err != nil {
		t.Fatalf("executeTask: %v", err)
	}
	if got := c.taskTracker.TodoList().Items()[0].Status; got != TaskDone {
		t.Fatalf("task status = %s, want done after the retry's own passing result", got)
	}
	if got := c.todoItemByID(item.ID).ProviderBinding; got == nil || got.SessionID != "thread-retry" {
		t.Fatalf("durable ProviderBinding = %#v, want the same thread-retry session reused across the retry", got)
	}
}

// TestCodexPartialStatusStaysIncompleteWithoutVerifySpec proves §21's own
// example directly: "Codex says partial + tests pass -> task remains
// incomplete because provider did not claim terminal completion." With no
// task.VerifySpec/Verify at all — so the only thing that can catch this is
// Hufu's own completion-gate classification of the canonicalized result,
// not an independent verifier — a Codex proposal reporting "partial" must
// still not let the task reach TaskDone on that attempt; Hufu retries on
// its own authority instead. This is the scenario isSubmittedResultSource
// exists for: without recognizing ExternalProviderProposalSource as a
// genuine structured handoff, validateCompletedTaskResult's non-terminal-
// status check never runs for a Codex-driven attempt at all, and a
// "partial" would have silently reached TaskDone on the first attempt.
func TestCodexPartialStatusStaysIncompleteWithoutVerifySpec(t *testing.T) {
	workspace := newCodexWorkspace(t)
	worker := &agent.AgentDef{
		Name: "worker", Role: "worker", SubagentProvider: "codex", MaxRetries: 1,
		Generation: agent.GenerationParams{Model: "gpt-5-codex"},
	}
	firstAttempt := fakeCodexTurnCompletedStep(t, "turn-1", `{"status":"partial","summary":"not done yet"}`)
	secondAttempt := fakeCodexTurnCompletedStep(t, "turn-2", `{"status":"success","summary":"done now"}`)
	c, item := newCodexEndToEndCoordinator(t, workspace, []fakeCodexStep{
		codexInitializeStep(t),
		codexThreadStartStep(t, "thread-partial", workspace),
		firstAttempt,
		codexInitializeStep(t),
		codexThreadResumeStep(t, "thread-partial", workspace, "workspace-write"),
		secondAttempt,
	}, worker, nil)

	task := TaskDef{
		Agent: worker.Name, Goal: "baseline worker task", SideEffect: SideEffectWorkspaceWrite,
		Execution: ExecutionContract{RequiresResult: true},
	}
	if _, err := c.executeTask(context.Background(), task, item.ID); err != nil {
		t.Fatalf("executeTask: %v", err)
	}
	if got := c.taskTracker.TodoList().Items()[0].Status; got != TaskDone {
		t.Fatalf("task status = %s, want done after the retry reported success", got)
	}
	if got := c.GetTaskResult(item.ID); got == nil || got.Summary != "done now" {
		t.Fatalf("final TaskResult = %#v, want the retry's own successful result, not attempt 1's partial one", got)
	}
}

// TestDirectCodexWorkerPreservesNormalTaskSemantics is spec.md §36 Phase 7
// PR-15's named test: a Codex-backed worker invoked through the direct fast
// path (RunDirectAgent) must exhibit the same observable semantics — durable
// provider binding, canonicalized result, receipt provider identity (§20) —
// as the same worker dispatched through the normal coordinator task path.
// It does, because RunDirectAgent now escalates a non-hufu-local provider to
// the exact same executeTask dispatch (§28); this test drives a real fake
// app-server subprocess through that escalated path to prove it end to end,
// not just that the escalation branch is reached.
func TestDirectCodexWorkerPreservesNormalTaskSemantics(t *testing.T) {
	workspace := newCodexWorkspace(t)
	worker := &agent.AgentDef{
		Name: "worker", Role: "worker", SubagentProvider: "codex", SideEffect: "workspace_write",
		Generation: agent.GenerationParams{Model: "gpt-5-codex"},
	}
	c := newCodexE2ECoordinatorBase(t, workspace, []fakeCodexStep{
		codexInitializeStep(t),
		codexThreadStartStep(t, "thread-direct", workspace),
		fakeCodexTurnCompletedStep(t, "turn-1", validProposalJSON("")),
	}, worker)

	result, err := c.RunDirectAgent(context.Background(), "worker", "do the work")
	if err != nil {
		t.Fatalf("RunDirectAgent: %v", err)
	}
	if result == nil || result.AgentName != "worker" {
		t.Fatalf("RunDirectAgent result = %#v", result)
	}
	items := c.taskTracker.TodoList().Items()
	if len(items) != 1 {
		t.Fatalf("todo items = %#v, want exactly one durable task occurrence", items)
	}
	item := items[0]
	if item.Status != TaskDone {
		t.Fatalf("task status = %s, want done", item.Status)
	}
	if item.ProviderBinding == nil || item.ProviderBinding.SessionID != "thread-direct" {
		t.Fatalf("ProviderBinding = %#v, want the durable Codex session — same as the coordinated path persists (§7.4)", item.ProviderBinding)
	}
	typedRes := c.GetTaskResult(item.ID)
	if typedRes == nil || typedRes.Source != ExternalProviderProposalSource || typedRes.Status != TaskResultStatusSuccess {
		t.Fatalf("TaskResult = %#v, want the canonicalized external-provider result (§9.3), same as the coordinated path produces", typedRes)
	}
	if item.ExecutionReceipt == nil || item.ExecutionReceipt.SubagentProvider != "codex" || item.ExecutionReceipt.ProviderSessionID != "thread-direct" {
		t.Fatalf("ExecutionReceipt = %#v, want provider identity recorded (§20) exactly as the coordinated path records it", item.ExecutionReceipt)
	}
}

// TestCodexReviewWorksetStyleTaskSatisfiesFilesReadVerifySpec proves the
// §38/§9.1 files_read extension closes the gap that same section documented:
// a task shaped like hufu-code-review's review-workset — task_result_assert
// verify-spec requiring /files_read with at least one item — can now reach
// TaskDone through a Codex-routed attempt, driven through the full
// production dispatch path (not just the canonicalizer in isolation).
func TestCodexReviewWorksetStyleTaskSatisfiesFilesReadVerifySpec(t *testing.T) {
	workspace := newCodexWorkspace(t)
	worker := &agent.AgentDef{
		Name: "worker", Role: "worker", SubagentProvider: "codex", MaxRetries: 0,
		Generation: agent.GenerationParams{Model: "gpt-5-codex"},
	}
	verify := &VerificationSpec{Type: VerifyTaskResultAssert, TaskResultAssertions: []TaskResultAssertion{
		{Pointer: "/summary", Op: "non_empty"},
		{Pointer: "/files_read", Op: "min_items", Value: 1},
	}}
	turnStep := fakeCodexTurnCompletedStep(t, "turn-1", validProposalJSON(`"files_read":["reviewed.go"]`))
	turnStep.WriteFile, turnStep.WriteFileContent = "reviewed.go", "package main\n"
	c, item := newCodexEndToEndCoordinator(t, workspace, []fakeCodexStep{
		codexInitializeStep(t),
		codexThreadStartStep(t, "thread-files-read", workspace),
		turnStep,
	}, worker, verify)

	task := TaskDef{
		Agent: worker.Name, Goal: "baseline worker task", SideEffect: SideEffectWorkspaceWrite,
		Execution: ExecutionContract{RequiresResult: true}, VerifySpec: verify,
	}
	if _, err := c.executeTask(context.Background(), task, item.ID); err != nil {
		t.Fatalf("executeTask: %v", err)
	}
	got := c.taskTracker.TodoList().Items()[0]
	if got.Status != TaskDone {
		t.Fatalf("task status = %s, want done", got.Status)
	}
	typedRes := c.GetTaskResult(item.ID)
	if typedRes == nil || len(typedRes.FilesRead) != 1 || typedRes.FilesRead[0].Path != "reviewed.go" {
		t.Fatalf("TaskResult.FilesRead = %#v, want exactly reviewed.go carried through from the Codex proposal", typedRes)
	}
}
