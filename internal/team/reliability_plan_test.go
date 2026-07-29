package team

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/anomalyco/hufu/internal/agent"
	"github.com/anomalyco/hufu/internal/tools"
	"github.com/anomalyco/hufu/internal/utils"
)

type failingReader struct{}

func (f failingReader) Read(p []byte) (n int, err error) {
	return 0, errors.New("simulated crypto/rand entropy outage")
}

func TestRunOutcome_SemanticsAndStats(t *testing.T) {
	if IsRunOutcomeSuccess(RunOutcomePartial) {
		t.Error("RunOutcomePartial should not be considered success")
	}
	if !IsRunOutcomeSuccess(RunOutcomeCompleted) {
		t.Error("RunOutcomeCompleted should be considered success")
	}

	items := []*TodoItem{
		{ID: "1", Agent: "worker", Status: TaskDone},
		{ID: "2", Agent: "worker", Status: TaskError, Detail: "failed build", Retries: 1},
		{ID: "3", Agent: "worker", Status: TaskPending},
	}

	stats := SummarizeRunStats(items)
	if stats.TasksTotal != 3 {
		t.Errorf("TasksTotal = %d, want 3", stats.TasksTotal)
	}
	if stats.TasksDone != 1 {
		t.Errorf("TasksDone = %d, want 1", stats.TasksDone)
	}
	if stats.TasksUnresolved != 2 {
		t.Errorf("TasksUnresolved = %d, want 2", stats.TasksUnresolved)
	}
}

func TestRunStats_CountsFailedAttemptsBeforeEventualSuccess(t *testing.T) {
	items := []*TodoItem{
		{ID: "1", Status: TaskDone, Retries: 2},
		{ID: "2", Status: TaskError, Retries: 1},
	}

	stats := SummarizeRunStats(items)
	if stats.AttemptsTotal != 5 { // 3 attempts for task 1, 2 for task 2
		t.Fatalf("AttemptsTotal = %d, want 5", stats.AttemptsTotal)
	}
	if stats.AttemptsFailed != 4 { // two retries + retry and final failed attempt
		t.Fatalf("AttemptsFailed = %d, want 4", stats.AttemptsFailed)
	}
}

func TestParseTeamConfig_MaxCoordinatorTurns(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "team.yaml"), []byte("name: bounded\nmax-coordinator-turns: 7\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := parseTeamYML(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MaxCoordinatorTurns != 7 {
		t.Fatalf("MaxCoordinatorTurns = %d, want 7", cfg.MaxCoordinatorTurns)
	}
}

func TestProtocolFailureRecoveryPolicyDoesNotReplayNonReplayableSideEffects(t *testing.T) {
	for _, class := range []SideEffectClass{SideEffectExternalWrite, SideEffectInfraMutation, SideEffectCredential} {
		if !nonReplayableSideEffect(class) {
			t.Errorf("%s must be non-replayable for protocol-only failures", class)
		}
	}
	if nonReplayableSideEffect(SideEffectWorkspaceWrite) {
		t.Error("workspace writes retain the ordinary retry policy")
	}
}

func TestExecuteTaskProtocolOnlyEmptyOutputBlocksWithoutRetry(t *testing.T) {
	workspace := t.TempDir()
	// executeTask's STM recorder is deliberately asynchronous; let it finish
	// before t.TempDir removes the workspace during a full parallel suite.
	t.Cleanup(func() { time.Sleep(100 * time.Millisecond) })
	calls := 0
	c := &Coordinator{
		session: &TeamSession{
			Workspace: workspace,
			Config:    agent.TeamConfig{Name: "protocol", Timeout: 30, MaxRetries: 3},
			Agents: map[string]*agent.AgentDef{
				"worker": {Name: "worker", Role: "worker", Generation: agent.GenerationParams{Model: "test"}},
			},
		},
		sessionTime:     time.Now(),
		taskTracker:     NewTaskTracker(),
		reportStatus:    func(StatusEvent) {},
		taskResultCache: make(map[string][]cachedTaskEntry),
	}
	// The override models a worker that performed one external side effect,
	// then stopped without output or submit_result. executeTask must classify
	// this as protocol-only and never invoke the worker a second time.
	items := c.taskTracker.TodoList().AddBatch([]TodoSpec{{
		Agent:      "worker",
		Desc:       "perform external mutation",
		SideEffect: SideEffectExternalWrite,
	}})
	_ = items
	c.workerAgentOverride = &countingEmptyAgent{calls: &calls}
	_, err := c.executeTask(context.Background(), TaskDef{Agent: "worker", Goal: "perform external mutation", SideEffect: SideEffectExternalWrite, Execution: TaskExecutionPolicy{StrictResult: true}}, "1")
	if err == nil {
		t.Fatal("expected protocol-only task failure")
	}
	if calls != 1 {
		t.Fatalf("worker invocation count = %d, want exactly one", calls)
	}
	item := c.taskTracker.TodoList().Items()[0]
	if item.Status != TaskBlocked {
		t.Fatalf("task status = %s, want blocked", item.Status)
	}
	if !strings.Contains(item.Detail, "protocol") {
		t.Fatalf("blocked detail does not preserve protocol failure: %q", item.Detail)
	}
}

type countingEmptyAgent struct{ calls *int }

func (a *countingEmptyAgent) Generate(context.Context, fantasy.AgentCall) (*fantasy.AgentResult, error) {
	*a.calls = *a.calls + 1
	return &fantasy.AgentResult{}, nil
}

func (a *countingEmptyAgent) Stream(context.Context, fantasy.AgentStreamCall) (*fantasy.AgentResult, error) {
	*a.calls = *a.calls + 1
	return &fantasy.AgentResult{}, nil
}

func TestRunResultPersistsAndRestoresWithSession(t *testing.T) {
	workspace := t.TempDir()
	session := &TeamSession{Workspace: workspace, Config: agent.TeamConfig{Name: "persist"}}
	c := &Coordinator{session: session, sessionData: NewSession(), lastRunResult: nil}
	c.SetLastRunResult(&RunResult{
		Outcome:       RunOutcomePartial,
		GoalSatisfied: false,
		Acceptance:    &AcceptanceResult{Passed: false},
		Stats:         RunStats{TasksUnresolved: 1},
	})
	saved := LoadSession(workspace)
	if saved == nil || saved.RunResult == nil || saved.RunResult.Outcome != RunOutcomePartial {
		t.Fatalf("persisted run result missing or incorrect: %#v", saved)
	}
	restored := &Coordinator{session: session, taskTracker: NewTaskTracker(), taskResultCache: make(map[string][]cachedTaskEntry)}
	restored.SetSessionData(saved)
	if got := restored.LastRunResult(); got == nil || got.Outcome != RunOutcomePartial {
		t.Fatalf("restored run result missing or incorrect: %#v", got)
	}
}

func TestRunFinishedEventUsesCanonicalOutcome(t *testing.T) {
	workspace := t.TempDir()
	c := &Coordinator{
		session:     &TeamSession{Workspace: workspace, Config: agent.TeamConfig{Name: "events"}},
		sessionData: NewSession(),
		taskTracker: NewTaskTracker(),
	}
	end := c.beginExecutionRun()
	c.SetLastRunResult(&RunResult{
		Outcome:       RunOutcomeBlocked,
		GoalSatisfied: false,
		Stats:         RunStats{TasksUnresolved: 1, AttemptsTotal: 2, AttemptsFailed: 2},
	})
	end()
	es, err := OpenEventStore(workspace)
	if err != nil {
		t.Fatal(err)
	}
	events, err := es.ReadEvents()
	_ = es.Close()
	if err != nil {
		t.Fatal(err)
	}
	var found RunEvent
	for _, event := range events {
		if event.Type == "run_finished" {
			found = event
		}
	}
	if found.Type == "" {
		t.Fatal("run_finished event not found")
	}
	var payload map[string]any
	if err := json.Unmarshal(found.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if payload["outcome"] != string(RunOutcomeBlocked) || payload["goal_satisfied"] != false {
		t.Fatalf("unexpected run outcome payload: %#v", payload)
	}
	stats, ok := payload["stats"].(map[string]any)
	if !ok || stats["tasks_unresolved"] != float64(1) {
		t.Fatalf("canonical stats missing from event payload: %#v", payload)
	}
}

func TestEnsureFinishedContinuationHonorsLimitAndCancellation(t *testing.T) {
	workspace := t.TempDir()
	calls := 0
	c := &Coordinator{
		session:      &TeamSession{Workspace: workspace, Config: agent.TeamConfig{Name: "continuation", MaxCoordinatorTurns: 2}},
		sessionData:  NewSession(),
		taskTracker:  NewTaskTracker(),
		reportStatus: func(StatusEvent) {},
	}
	c.runOrchestratorOverride = func(context.Context, *agent.AgentDef, string) (string, []fantasy.StepResult, error) {
		calls++
		return "progress", nil, nil
	}
	result, _ := c.ensureFinished(context.Background(), &agent.AgentDef{Name: "coordinator"}, "initial", nil)
	if result != "progress" {
		t.Fatalf("continuation result = %q, want progress", result)
	}
	if calls != 3 { // two bounded continuations plus one forced wrap-up
		t.Fatalf("orchestrator calls = %d, want 3", calls)
	}
	last := c.LastRunResult()
	if last == nil || last.Continuation == nil || last.Continuation.MaxTurns != 2 {
		t.Fatalf("continuation metadata missing or incorrect: %#v", last)
	}

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	calls = 0
	_, _ = c.ensureFinished(cancelled, &agent.AgentDef{Name: "coordinator"}, "initial", nil)
	if calls != 0 {
		t.Fatalf("cancelled continuation invoked orchestrator %d times", calls)
	}
}

func TestEnsureFinishedBudgetProducesPartialOutcome(t *testing.T) {
	c := newBudgetCoordinator(t)
	c.session.Workspace = t.TempDir()
	c.sessionData = NewSession()
	c.SetBudget(0, 1)
	c.tokensUsed.Store(1)
	calls := 0
	c.runOrchestratorOverride = func(context.Context, *agent.AgentDef, string) (string, []fantasy.StepResult, error) {
		calls++
		return "wrap-up", nil, nil
	}
	_, _ = c.ensureFinished(context.Background(), &agent.AgentDef{Name: "coordinator"}, "progress", nil)
	result := c.LastRunResult()
	if calls != 1 || result == nil || result.Outcome != RunOutcomePartial || result.GoalSatisfied || result.Reason == "" {
		t.Fatalf("budget continuation calls=%d result=%#v, want one wrap-up and partial outcome", calls, result)
	}
}

func TestFinishGate_UnresolvedTasks_ReturnsPartial(t *testing.T) {
	c := newBudgetCoordinator(t)
	c.taskTracker.TodoList().AddBatch([]TodoSpec{
		{Agent: "worker", Desc: "task 1"},
	})
	items := c.taskTracker.TodoList().Items()
	c.taskTracker.TodoList().UpdateStatusAndOutput(items[0].ID, TaskError, "something went wrong", "failed output")

	finish := &finishTool{coordinator: c}

	// 1. Without acknowledge_failed_tasks -> error response
	resp, err := finish.Run(context.Background(), fantasy.ToolCall{Input: `{"response":"all done"}`})
	if err != nil {
		t.Fatalf("finish.Run unexpected error: %v", err)
	}
	if resp.IsError != true {
		t.Error("expected error response when acknowledging failed tasks is false")
	}

	// 2. With acknowledge_failed_tasks=true -> produces partial result
	resp, err = finish.Run(context.Background(), fantasy.ToolCall{Input: `{"response":"partial work done", "acknowledge_failed_tasks": true}`})
	if err != nil {
		t.Fatalf("finish.Run unexpected error: %v", err)
	}
	if resp.IsError {
		t.Fatalf("finish.Run error response: %v", resp.Content)
	}

	lastRes := c.LastRunResult()
	if lastRes == nil {
		t.Fatal("expected LastRunResult to be set")
	}
	if lastRes.Outcome != RunOutcomePartial {
		t.Errorf("Outcome = %s, want %s", lastRes.Outcome, RunOutcomePartial)
	}
	if lastRes.GoalSatisfied {
		t.Error("GoalSatisfied should be false for partial outcome")
	}
	if len(lastRes.UnresolvedTasks) != 1 {
		t.Errorf("UnresolvedTasks len = %d, want 1", len(lastRes.UnresolvedTasks))
	}
}

func TestFinishGate_AcceptanceSpecAndRequiredArtifacts(t *testing.T) {
	c := newBudgetCoordinator(t)
	tmpDir := t.TempDir()
	artPath := filepath.Join(tmpDir, "hosts.yml")

	spec := AcceptanceSpec{
		RequiredArtifacts:        []string{artPath},
		RequireNoUnresolvedTasks: true,
	}
	c.SetAcceptanceSpec(spec)

	finish := &finishTool{coordinator: c}

	// Artifact missing -> finish returns partial/failed acceptance
	_, err := finish.Run(context.Background(), fantasy.ToolCall{Input: `{"response":"all done"}`})
	if err != nil {
		t.Fatalf("finish.Run unexpected error: %v", err)
	}

	lastRes := c.LastRunResult()
	if lastRes == nil {
		t.Fatal("expected LastRunResult to be set")
	}
	if lastRes.Outcome == RunOutcomeCompleted {
		t.Error("Outcome must not be completed when required artifact is missing")
	}
	if lastRes.GoalSatisfied {
		t.Error("GoalSatisfied must be false when required artifact is missing")
	}

	// Create required artifact
	if err := os.WriteFile(artPath, []byte("127.0.0.1 localhost\n"), 0644); err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}

	// Re-run finish -> now completes successfully
	c.finishCalled.Store(false)
	_, err = finish.Run(context.Background(), fantasy.ToolCall{Input: `{"response":"all done"}`})
	if err != nil {
		t.Fatalf("finish.Run unexpected error: %v", err)
	}

	lastRes = c.LastRunResult()
	if lastRes.Outcome != RunOutcomeCompleted {
		t.Errorf("Outcome = %s, want %s", lastRes.Outcome, RunOutcomeCompleted)
	}
	if !lastRes.GoalSatisfied {
		t.Error("GoalSatisfied should be true when required artifact exists and 0 unresolved tasks")
	}
}

func TestNoFinishPath_AcceptanceNotRun_ProducesPartialOutcome(t *testing.T) {
	c := newBudgetCoordinator(t)
	c.SetAcceptance("exit 1") // Acceptance configured to fail

	c.taskTracker.TodoList().AddBatch([]TodoSpec{
		{Agent: "worker", Desc: "task 1"},
	})
	items := c.taskTracker.TodoList().Items()
	c.taskTracker.TodoList().UpdateStatusAndOutput(items[0].ID, TaskDone, "done", "success")

	orchDef := c.GetOrchestratorDef()
	if orchDef == nil {
		orchDef = &agent.AgentDef{Name: "coordinator"}
	}

	// Call ensureFinished when finish tool was NEVER called by LLM
	res, _ := c.ensureFinished(context.Background(), orchDef, "narration text", nil)
	if res == "" {
		t.Error("expected non-empty result")
	}

	lastRes := c.LastRunResult()
	if lastRes == nil {
		t.Fatal("expected LastRunResult to be synthesized in ensureFinished")
	}
	if lastRes.Outcome == RunOutcomeCompleted {
		t.Error("Outcome MUST NOT be completed when acceptance fails or is not passed")
	}
	if lastRes.GoalSatisfied {
		t.Error("GoalSatisfied MUST be false when acceptance fails")
	}
	if lastRes.Acceptance == nil || lastRes.Acceptance.Passed {
		t.Error("Acceptance result must indicate failure")
	}
}

func TestReconcileTask_Success(t *testing.T) {
	c := newBudgetCoordinator(t)
	c.taskTracker.TodoList().AddBatch([]TodoSpec{
		{Agent: "worker", Desc: "failed task A"},
		{Agent: "worker", Desc: "repair task B"},
	})
	items := c.taskTracker.TodoList().Items()
	taskA := items[0]
	taskB := items[1]

	c.taskTracker.TodoList().UpdateStatusAndOutput(taskA.ID, TaskError, "failed initial attempt", "failed")
	c.taskTracker.TodoList().UpdateStatusAndOutput(taskB.ID, TaskDone, "fixed by task B", "success")
	_ = c.taskTracker.TodoList().SetVerificationResult(taskB.ID, &VerificationResult{Command: "test -f ok", ExitCode: 0})

	// Verify failedTodoItems includes task A
	failed := failedTodoItems(c.taskTracker.TodoList().Items())
	if len(failed) != 1 {
		t.Fatalf("failedTodoItems len = %d, want 1", len(failed))
	}

	// Reconcile task A using reconcile_task tool
	tool := &reconcileTaskTool{coordinator: c}
	toolInput := `{"task_id":"` + taskA.ID + `", "status":"reconciled", "resolved_by":"` + taskB.ID + `", "reason":"task B fixed configuration", "evidence":[{"type":"command","description":"test -f ok","value":"pass"}]}`
	resp, err := tool.Run(context.Background(), fantasy.ToolCall{Input: toolInput})
	if err != nil || resp.IsError {
		t.Fatalf("reconcile_task failed: %v, content: %s", err, resp.Content)
	}

	// Verify failedTodoItems no longer includes task A
	failedAfter := failedTodoItems(c.taskTracker.TodoList().Items())
	if len(failedAfter) != 0 {
		t.Errorf("failedTodoItems len after reconcile = %d, want 0", len(failedAfter))
	}

	// Verify finish gate passes
	finish := &finishTool{coordinator: c}
	_, err = finish.Run(context.Background(), fantasy.ToolCall{Input: `{"response":"all done"}`})
	if err != nil {
		t.Fatalf("finish.Run unexpected error: %v", err)
	}
	if c.LastRunResult().Outcome != RunOutcomeCompleted {
		t.Errorf("Outcome = %s, want %s", c.LastRunResult().Outcome, RunOutcomeCompleted)
	}
}

func TestSystemSecret_FailClosedOnRNGError(t *testing.T) {
	// Override randReader with failing reader
	oldReader := randReader
	oldKey, oldErr := systemSecretKey, systemSecretErr
	defer func() {
		randReader = oldReader
		systemSecretOnce = sync.Once{}
		systemSecretKey, systemSecretErr = oldKey, oldErr
	}()

	randReader = failingReader{}
	systemSecretOnce = sync.Once{}
	systemSecretKey = ""
	systemSecretErr = nil

	sec, err := GetSystemSecret()
	if err == nil {
		t.Fatalf("expected GetSystemSecret to fail when crypto/rand fails, got key: %q", sec)
	}
	if !strings.Contains(err.Error(), "failed to generate cryptographically secure secret") {
		t.Errorf("unexpected error message: %v", err)
	}
	if strings.Contains(sec, "fallback-secret") {
		t.Error("GetSystemSecret MUST NOT return a predictable time-based fallback secret")
	}

	// Verify transcript signing error propagation when GetSystemSecret fails
	res := &TaskResult{}
	transcript, _ := newTaskTranscript(t.TempDir(), "test-task", "run-test")
	_, transcriptErr := finalizeVerbatimTaskResult(transcript, res)
	if transcriptErr == nil {
		t.Error("expected finalizeVerbatimTaskResult to propagate RNG error from GetSystemSecret instead of swallowing it")
	}
}

func TestSubprocessEnvironment_HUFU_HMAC_SECRET_Isolation(t *testing.T) {
	os.Setenv("HUFU_HMAC_SECRET", "leak-attempt-secret")
	systemSecretOnce = sync.Once{}
	systemSecretKey = ""
	systemSecretErr = nil

	sec, err := GetSystemSecret()
	if err != nil {
		t.Fatalf("GetSystemSecret unexpected error: %v", err)
	}
	if sec == "leak-attempt-secret" {
		t.Error("GetSystemSecret MUST NOT read secret from environment variable HUFU_HMAC_SECRET")
	}
	if os.Getenv("HUFU_HMAC_SECRET") != "" {
		t.Error("HUFU_HMAC_SECRET MUST be scrubbed from environment upon initialization")
	}

	// Test SanitizeSubprocessEnv strips HUFU_HMAC_SECRET across tool and team packages
	dirtyEnv := []string{"PATH=/bin", "HUFU_HMAC_SECRET=leak", "USER=worker"}
	cleanEnv := tools.SanitizeSubprocessEnv(dirtyEnv)
	for _, e := range cleanEnv {
		if strings.HasPrefix(e, "HUFU_HMAC_SECRET=") {
			t.Errorf("SanitizeSubprocessEnv failed to strip HUFU_HMAC_SECRET: %v", cleanEnv)
		}
	}

	cleanUtilsEnv := utils.SanitizeSubprocessEnv(dirtyEnv)
	for _, e := range cleanUtilsEnv {
		if strings.HasPrefix(e, "HUFU_HMAC_SECRET=") {
			t.Errorf("utils.SanitizeSubprocessEnv failed to strip HUFU_HMAC_SECRET: %v", cleanUtilsEnv)
		}
	}
}

func TestReplayPreventionAcrossTasksAndRuns(t *testing.T) {
	secret, err := GetSystemSecret()
	if err != nil || secret == "" {
		t.Fatalf("expected GetSystemSecret to return valid secret key, got: %v", err)
	}

	// Sign evidence bound to task-A and run-1
	refA := EvidenceRef{
		TaskID:      "task-A",
		RunID:       "run-1",
		Type:        "transcript",
		Description: "output proof",
		Value:       "verified",
	}
	signedRefA := SignEvidence(refA, secret)

	// 1. Verify matching task-A and run-1 passes
	if !VerifyEvidenceSignature(signedRefA, secret, "task-A", "run-1") {
		t.Error("valid signature bound to task-A and run-1 MUST pass verification")
	}

	// 2. Replay attempt on task-B (TaskID mismatch) MUST fail
	if VerifyEvidenceSignature(signedRefA, secret, "task-B", "run-1") {
		t.Error("replayed evidence signature on different task-B MUST FAIL verification")
	}

	// 3. Replay attempt on run-2 (RunID mismatch) MUST fail
	if VerifyEvidenceSignature(signedRefA, secret, "task-A", "run-2") {
		t.Error("replayed evidence signature on different run-2 MUST FAIL verification")
	}

	// 4. Test submit_result tool strips model-submitted system_hmac
	c := newBudgetCoordinator(t)
	c.taskTracker.TodoList().AddBatch([]TodoSpec{
		{Agent: "worker", Desc: "subtask 1"},
	})
	task1 := c.taskTracker.TodoList().items[0]

	submitTool := &submitResultTool{coordinator: c, todoID: task1.ID}
	forgedMAC := fmt.Sprintf("%x", sha256.Sum256([]byte("forged")))
	submitInput := `{"status":"success", "summary":"done", "evidence":[{"task_id":"` + task1.ID + `", "type":"claim", "value":"proof", "system_hmac":"` + forgedMAC + `"}]}`
	_, submitErr := submitTool.Run(context.Background(), fantasy.ToolCall{Input: submitInput})
	if submitErr != nil {
		t.Fatalf("submit_result unexpected error: %v", submitErr)
	}

	submittedResult := c.GetTaskResult(task1.ID)
	if submittedResult == nil || len(submittedResult.Evidence) == 0 {
		t.Fatal("expected submitted task result to be stored")
	}
	if submittedResult.Evidence[0].SystemHMAC != "" {
		t.Error("submit_result MUST strip model-supplied system_hmac signatures")
	}

	// 5. Test SetTypedResult also strips model-supplied system_hmac
	_ = c.taskTracker.TodoList().SetTypedResult(task1.ID, &TaskResult{
		Status:   "success",
		Evidence: []EvidenceRef{{TaskID: task1.ID, Type: "claim", SystemHMAC: forgedMAC}},
	})
	updatedItem := c.taskTracker.TodoList().items[0]
	if updatedItem.TypedResult != nil && len(updatedItem.TypedResult.Evidence) > 0 {
		if updatedItem.TypedResult.Evidence[0].SystemHMAC != "" {
			t.Error("SetTypedResult MUST strip model-supplied system_hmac signatures")
		}
	}
}

func TestEvidenceCryptographicSignatureVerification(t *testing.T) {
	secret, err := GetSystemSecret()
	if err != nil || secret == "" {
		t.Fatalf("expected GetSystemSecret to return valid secret key, got: %v", err)
	}

	c := newBudgetCoordinator(t)
	c.taskTracker.TodoList().AddBatch([]TodoSpec{
		{Agent: "worker", Desc: "failed A"},
		{Agent: "worker", Desc: "done B"},
	})
	items := c.taskTracker.TodoList().items
	taskA := items[0]
	taskB := items[1]

	unsignedRef := EvidenceRef{TaskID: taskB.ID, RunID: "run-B", Type: "test", Description: "unverified claim", Value: "fixed"}
	if VerifyEvidenceSignature(unsignedRef, secret, taskB.ID, "") {
		t.Error("unsigned evidence reference MUST NOT pass HMAC signature verification")
	}

	signedRef := SignEvidence(unsignedRef, secret)
	if !VerifyEvidenceSignature(signedRef, secret, taskB.ID, "") {
		t.Error("system-signed evidence reference MUST pass HMAC signature verification")
	}

	// Alter evidence content after signing -> signature MUST fail
	tamperedRef := signedRef
	tamperedRef.Value = "tampered value"
	if VerifyEvidenceSignature(tamperedRef, secret, taskB.ID, "") {
		t.Error("tampered evidence reference MUST NOT pass HMAC signature verification")
	}

	taskA.Status = TaskError
	taskB.Status = TaskDone
	taskB.TypedResult = &TaskResult{
		Status:   "success",
		Evidence: []EvidenceRef{unsignedRef}, // Unsigned evidence
	}

	err = ValidateResolution(&TaskResolution{Status: "reconciled", ResolvedBy: taskB.ID}, taskA.ID, items, "")
	if err == nil {
		t.Error("expected ValidateResolution to reject un-signed evidence on resolver TypedResult")
	}

	// Now sign evidence for taskB -> ValidateResolution MUST accept
	taskB.TypedResult.Evidence = []EvidenceRef{signedRef}
	err = ValidateResolution(&TaskResolution{Status: "reconciled", ResolvedBy: taskB.ID}, taskA.ID, items, "")
	if err != nil {
		t.Errorf("expected ValidateResolution to accept system-signed evidence, got error: %v", err)
	}

	// Test reconcile_task tool strips model-submitted HMAC signatures AND fails when resolver is unverified
	tool := &reconcileTaskTool{coordinator: c}
	// Task B is reset to unverified (no VerifyResult, no signed evidence)
	taskB.VerifyResult = nil
	taskB.TypedResult = nil

	forgedMAC := fmt.Sprintf("%x", sha256.Sum256([]byte("forged")))
	toolInput := `{"task_id":"` + taskA.ID + `", "status":"reconciled", "resolved_by":"` + taskB.ID + `", "reason":"model claim", "evidence":[{"type":"claim","value":"fixed", "system_hmac":"` + forgedMAC + `"}]}`
	resp, toolErr := tool.Run(context.Background(), fantasy.ToolCall{Input: toolInput})
	if toolErr != nil {
		t.Fatalf("reconcile_task unexpected error: %v", toolErr)
	}
	if !resp.IsError {
		t.Error("expected reconcile_task to fail when model supplies forged MAC for unverified resolver")
	}
	if !strings.Contains(resp.Content, "lacks objective verification evidence") {
		t.Errorf("expected error message about missing objective verification evidence, got: %s", resp.Content)
	}

	// Verify task A remains in failedTodoItems
	failed := failedTodoItems(c.taskTracker.TodoList().Items())
	if len(failed) != 1 {
		t.Errorf("failedTodoItems len after rejected reconcile = %d, want 1", len(failed))
	}
}

func TestReconcileTask_ValidationRules(t *testing.T) {
	c := newBudgetCoordinator(t)
	c.taskTracker.TodoList().AddBatch([]TodoSpec{
		{Agent: "worker", Desc: "failed task A"},
		{Agent: "worker", Desc: "pending task B"},
		{Agent: "worker", Desc: "done task C (unverified)"},
		{Agent: "worker", Desc: "failed task D"},
	})
	items := c.taskTracker.TodoList().Items()
	taskA := items[0]
	taskB := items[1]
	taskC := items[2]

	c.taskTracker.TodoList().UpdateStatusAndOutput(taskA.ID, TaskError, "failed A", "")
	c.taskTracker.TodoList().UpdateStatusAndOutput(taskC.ID, TaskDone, "done C", "")

	// 1. Reconciling pending task -> should fail
	err := c.taskTracker.TodoList().SetTaskResolution(taskB.ID, &TaskResolution{Status: "reconciled", ResolvedBy: taskC.ID, Reason: "text"})
	if err == nil {
		t.Error("expected error when attempting to reconcile a pending task")
	}

	// 2. Resolver task is not done -> should fail
	err = c.taskTracker.TodoList().SetTaskResolution(taskA.ID, &TaskResolution{Status: "reconciled", ResolvedBy: taskB.ID, Reason: "text"})
	if err == nil {
		t.Error("expected error when resolver task is not done")
	}

	// 3. Resolver task lacks verification evidence -> should fail
	err = c.taskTracker.TodoList().SetTaskResolution(taskA.ID, &TaskResolution{Status: "reconciled", ResolvedBy: taskC.ID, Reason: "text"})
	if err == nil {
		t.Error("expected error when resolver task has no objective verification evidence")
	}

	// 4. Model-supplied evidence alone without passing VerifyResult/TypedResult -> MUST fail
	tool := &reconcileTaskTool{coordinator: c}
	toolInput := `{"task_id":"` + taskA.ID + `", "status":"reconciled", "resolved_by":"` + taskC.ID + `", "reason":"model claim", "evidence":[{"type":"claim","value":"fixed"}]}`
	resp, err := tool.Run(context.Background(), fantasy.ToolCall{Input: toolInput})
	if err == nil && !resp.IsError {
		t.Error("expected reconcile_task to fail when resolver task C lacks objective verification evidence")
	}

	// 5. Reachable 3-Node Cycle (A -> B -> C -> A) where A, B, C targets are in TaskError status and resolvers are TaskDone + verified
	c.taskTracker.TodoList().items = nil
	c.taskTracker.TodoList().AddBatch([]TodoSpec{
		{Agent: "worker", Desc: "task A"},
		{Agent: "worker", Desc: "task B"},
		{Agent: "worker", Desc: "task C"},
	})
	all := c.taskTracker.TodoList().items
	taskA = all[0]
	taskB = all[1]
	taskC = all[2]

	taskA.Status = TaskError
	taskB.Status = TaskError
	taskC.Status = TaskError

	// Add verification results so status/verification check passes
	taskA.VerifyResult = &VerificationResult{ExitCode: 0}
	taskB.VerifyResult = &VerificationResult{ExitCode: 0}
	taskC.VerifyResult = &VerificationResult{ExitCode: 0}

	// Set A resolved by B, B resolved by C
	taskA.Resolution = &TaskResolution{Status: "reconciled", ResolvedBy: taskB.ID}
	taskB.Resolution = &TaskResolution{Status: "reconciled", ResolvedBy: taskC.ID}

	// Attempt C resolved by A (creates 3-node cycle A->B->C->A)
	// For cycle validation on C being resolved by A:
	// Target C MUST be in TaskError status (rule #1), while resolvers A and B are TaskDone (rule #2).
	taskA.Status = TaskDone
	taskB.Status = TaskDone
	taskC.Status = TaskError

	err = ValidateResolution(&TaskResolution{Status: "reconciled", ResolvedBy: taskA.ID}, taskC.ID, all, "")
	if err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Errorf("expected cycle error for 3-node cycle A->B->C->A, got: %v", err)
	}
}

func TestAcceptanceContract_StructuredAuditEventAndPersistence(t *testing.T) {
	tmpDir := t.TempDir()
	c := newBudgetCoordinator(t)
	c.session = &TeamSession{
		Workspace: tmpDir,
		Config:    agent.TeamConfig{Name: "test-team"},
	}

	var reportedEvents []StatusEvent
	c.SetStatusReporter(func(e StatusEvent) {
		reportedEvents = append(reportedEvents, e)
	})

	initialSpec := AcceptanceSpec{Commands: []string{"test -f initial"}}
	c.SetAcceptanceSpec(initialSpec)

	newSpec := AcceptanceSpec{Commands: []string{"test -f updated"}}
	c.SetAcceptanceSpecWithReason(newSpec, "audit_test_reason")

	var modEvent *StatusEvent
	for _, e := range reportedEvents {
		if e.Type == "acceptance_contract_modified" {
			modEvent = &e
			break
		}
	}

	if modEvent == nil {
		t.Fatal("expected acceptance_contract_modified event to be reported")
	}

	if modEvent.Data == nil {
		t.Fatal("expected event Data to contain structured audit payload")
	}

	reason, _ := modEvent.Data["reason"].(string)
	if reason != "audit_test_reason" {
		t.Errorf("event reason = %q, want audit_test_reason", reason)
	}

	// Verify acceptance audit record was persisted to disk in workspace logs
	auditFile := filepath.Join(tmpDir, "logs", "acceptance_audit.jsonl")
	data, err := os.ReadFile(auditFile)
	if err != nil {
		t.Fatalf("failed to read persisted audit file: %v", err)
	}
	content := string(data)
	if !strings.Contains(content, `"event":"acceptance_contract_modified"`) || !strings.Contains(content, `"reason":"audit_test_reason"`) {
		t.Errorf("persisted audit content missing required fields, got: %s", content)
	}
}

func TestParseTeamConfig_InvalidAcceptance_ReturnsError(t *testing.T) {
	tmpDir := t.TempDir()
	yamlContent := `
name: invalid-team
acceptance:
  commands: 42
`
	if err := os.WriteFile(filepath.Join(tmpDir, "team.yml"), []byte(yamlContent), 0644); err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}

	_, err := parseTeamYML(tmpDir, nil)
	if err == nil {
		t.Error("expected parseTeamYML to return error for malformed acceptance spec format (commands: 42)")
	}
}

func TestSection14_PilotRegressionScenario(t *testing.T) {
	c := newBudgetCoordinator(t)
	tmpDir := t.TempDir()
	c.projectDir = tmpDir
	hostsFile := filepath.Join(tmpDir, "hosts.yml")

	c.SetAcceptanceSpec(AcceptanceSpec{
		RequiredArtifacts: []string{hostsFile},
	})

	c.taskTracker.TodoList().AddBatch([]TodoSpec{
		{Agent: "deployer", Desc: "create hosts.yml"},
		{Agent: "checker", Desc: "check hosts.yml exists"},
	})
	items := c.taskTracker.TodoList().Items()
	task1 := items[0]
	task2 := items[1]

	c.taskTracker.TodoList().UpdateStatusAndOutput(task1.ID, TaskError, "script failed exit code 1", "")
	c.taskTracker.TodoList().UpdateStatusAndOutput(task2.ID, TaskDone, "verified hosts.yml does not exist", "hosts.yml missing")

	updatedItems := c.taskTracker.TodoList().Items()
	task2 = updatedItems[1]

	if task2.Status != TaskDone {
		t.Errorf("task2 status = %s, want done", task2.Status)
	}

	finish := &finishTool{coordinator: c}
	_, err := finish.Run(context.Background(), fantasy.ToolCall{Input: `{"response":"applied fix, but hosts.yml not created", "acknowledge_failed_tasks": true}`})
	if err != nil {
		t.Fatalf("finish.Run unexpected error: %v", err)
	}

	lastRes := c.LastRunResult()
	if lastRes == nil {
		t.Fatal("expected LastRunResult to be set")
	}

	if lastRes.GoalSatisfied != false {
		t.Error("GoalSatisfied must be false")
	}
	if lastRes.Outcome != RunOutcomePartial {
		t.Errorf("Outcome = %s, want %s", lastRes.Outcome, RunOutcomePartial)
	}
	if len(lastRes.UnresolvedTasks) != 1 {
		t.Errorf("UnresolvedTasks len = %d, want 1", len(lastRes.UnresolvedTasks))
	}
	if lastRes.Acceptance == nil || lastRes.Acceptance.Passed {
		t.Error("Acceptance must be recorded as failed due to missing required artifact")
	}
}

func TestBeginExecutionRun_EventLoggerFailure_PreservesRunID(t *testing.T) {
	c := newBudgetCoordinator(t)
	// Invalid workspace path so event logger creation fails
	c.session = &TeamSession{
		Workspace: "/dev/null/invalid-dir-path",
		Config:    agent.TeamConfig{Name: "logger-fail-team"},
	}

	endRun := c.beginExecutionRun()
	defer endRun()

	if c.executionRunID == "" {
		t.Error("c.executionRunID MUST be initialized even if event logger creation fails")
	}
	if c.taskTracker.TodoList().RunID() == "" {
		t.Error("TodoList.RunID() MUST be initialized even if event logger creation fails")
	}
}
