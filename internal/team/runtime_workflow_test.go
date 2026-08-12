package team

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"charm.land/fantasy"
	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/mcp"
	"github.com/kjelly/hufu/internal/tools"
)

func workflowTestSession(t *testing.T) *TeamSession {
	t.Helper()
	return &TeamSession{
		Workspace: t.TempDir(),
		Config: agent.TeamConfig{
			Name:         "generic-workflow",
			Workflow:     agent.WorkflowConfig{Phases: []string{"prepare", "audit", "execute", "verify"}},
			Capabilities: agent.CapabilityConfig{Required: []string{"structured-actions"}},
			Delegation: agent.DelegationPolicy{
				BindTaskGoalContracts: true,
			},
		},
		Agents: map[string]*agent.AgentDef{
			"preparer": {Name: "preparer", Role: "worker"},
			"auditor":  {Name: "auditor", Role: "worker"},
			"executor": {Name: "executor", Role: "worker"},
			"verifier": {Name: "verifier", Role: "worker"},
		},
		ContractTasks: []TaskDef{
			{ID: "prepare", Agent: "preparer", WhenGoalContains: "prepare", Phase: PhasePrepare},
			{ID: "audit", Agent: "auditor", WhenGoalContains: "audit", Phase: PhaseAudit},
			{ID: "execute", Agent: "executor", WhenGoalContains: "execute", Phase: PhaseExecute},
			{ID: "verify", Agent: "verifier", WhenGoalContains: "verify", Phase: PhaseVerify},
		},
	}
}

type mockProvider struct{}

func (mockProvider) Validate(action Action) error { return nil }
func (mockProvider) Execute(ctx context.Context, action Action) (interface{}, error) {
	return nil, nil
}

func workflowTestRegistry() *ProviderRegistry {
	r := NewProviderRegistry()
	r.Register("structured-actions", mockProvider{})
	return r
}

func TestRuntimeWorkflowAdvancesOnlyAfterEveryPhaseSucceeds(t *testing.T) {
	session := workflowTestSession(t)
	if err := validateRuntimeWorkflowTeam(session, workflowTestRegistry()); err != nil {
		t.Fatalf("validateRuntimeWorkflowTeam: %v", err)
	}
	w, err := newRuntimeWorkflow(session)
	if err != nil {
		t.Fatalf("newRuntimeWorkflow: %v", err)
	}
	if err := w.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if got := w.executionContext().Capabilities.Required; len(got) != 1 || got[0] != "structured-actions" {
		t.Fatalf("execution context capabilities = %#v", got)
	}
	if got := w.State(); got != PhasePrepare {
		t.Fatalf("state after start = %s, want PREPARE", got)
	}

	items := []*TodoItem{
		{Agent: "preparer", ContractID: "prepare", Phase: PhasePrepare, Status: TaskDone},
		{Agent: "auditor", ContractID: "audit", Phase: PhaseAudit, Status: TaskDone},
		{Agent: "executor", ContractID: "execute", Phase: PhaseExecute, Status: TaskDone},
		{Agent: "verifier", ContractID: "verify", Phase: PhaseVerify, Status: TaskDone},
	}
	for _, want := range []Phase{PhaseAudit, PhaseExecute, PhaseVerify, PhaseDone} {
		if err := w.observe(items); err != nil {
			t.Fatalf("observe: %v", err)
		}
		if got := w.State(); got != want {
			t.Fatalf("state = %s, want %s", got, want)
		}
	}
	if err := w.requireFinished(); err != nil {
		t.Fatalf("verify gate unexpectedly rejected DONE: %v", err)
	}
	if _, err := w.workspace.ArtifactPath("report.json"); err != nil {
		t.Fatalf("ArtifactPath: %v", err)
	}
	if _, err := w.workspace.ArtifactPath("../escape"); err == nil {
		t.Fatal("ArtifactPath allowed path traversal")
	}
	if _, err := filepath.Glob(filepath.Join(session.Workspace, "runtime", "artifacts")); err != nil {
		t.Fatalf("runtime workspace was not created: %v", err)
	}
}

func TestRuntimeWorkflowRejectsCrossPhaseAndFailedTask(t *testing.T) {
	session := workflowTestSession(t)
	w, err := newRuntimeWorkflow(session)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Start(); err != nil {
		t.Fatal(err)
	}
	if err := w.validateTasks([]TaskDef{{Agent: "auditor", ContractID: "audit", Phase: PhaseAudit}}); err == nil || !strings.Contains(err.Error(), "PREPARE") {
		t.Fatalf("cross-phase task error = %v, want PREPARE rejection", err)
	}
	if err := w.observe([]*TodoItem{{Agent: "preparer", ContractID: "prepare", Phase: PhasePrepare, Status: TaskError, Detail: "check failed"}}); err == nil {
		t.Fatal("failed task did not fail workflow")
	}
	if got := w.State(); got != PhaseFailed {
		t.Fatalf("state = %s, want FAILED", got)
	}
	if err := w.requireFinished(); err == nil {
		t.Fatal("finish gate accepted failed workflow")
	}
}

func TestRuntimeWorkflowRequiresEveryStaticContractAndRestoresCheckpoint(t *testing.T) {
	session := workflowTestSession(t)
	// Two PREPARE contracts demonstrate that completion is tracked by immutable
	// contract ID, not merely by worker name.
	session.ContractTasks = append(session.ContractTasks, TaskDef{ID: "prepare-receipt", Agent: "preparer", WhenGoalContains: "receipt", Phase: PhasePrepare})
	if err := validateRuntimeWorkflowTeam(session, workflowTestRegistry()); err != nil {
		t.Fatal(err)
	}
	w, err := newRuntimeWorkflow(session)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Start(); err != nil {
		t.Fatal(err)
	}
	if err := w.validateTasks([]TaskDef{{Agent: "preparer", ContractID: "prepare", Phase: PhasePrepare}}); err == nil || !strings.Contains(err.Error(), "prepare-receipt") {
		t.Fatalf("partial phase dispatch error = %v, want missing contract", err)
	}
	state, results, root, retryState := w.snapshot()
	restored, err := newRuntimeWorkflow(session)
	if err != nil {
		t.Fatal(err)
	}
	if err := restored.restore(state, results, root, retryState); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if got := restored.State(); got != PhasePrepare {
		t.Fatalf("restored state = %s, want PREPARE", got)
	}
}

type recordingActionProvider struct {
	validated int
	executed  int
	result    interface{}
	err       error
}

func (p *recordingActionProvider) Validate(action Action) error {
	p.validated++
	if action.Type == "" {
		return fmt.Errorf("missing type")
	}
	return nil
}

func (p *recordingActionProvider) Execute(context.Context, Action) (interface{}, error) {
	p.executed++
	return p.result, p.err
}

func TestRuntimeWorkflowExecutesStaticActionOnlyInExecutePhase(t *testing.T) {
	session := workflowTestSession(t)
	provider := &recordingActionProvider{result: map[string]string{"status": "ok"}}
	registry := NewProviderRegistry()
	registry.Register("structured-actions", provider)
	session.ProviderRegistry = registry
	w, err := newRuntimeWorkflow(session)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Start(); err != nil {
		t.Fatal(err)
	}
	action := Action{Capability: "structured-actions", Type: "apply", Payload: `{"target":"example"}`}
	if _, err := w.executeAction(context.Background(), action); err == nil {
		t.Fatal("action before EXECUTE phase unexpectedly succeeded")
	}
	if provider.executed != 0 {
		t.Fatalf("provider executed %d times before EXECUTE", provider.executed)
	}
	// Complete PREPARE and AUDIT to move the runtime boundary to EXECUTE.
	if err := w.observe([]*TodoItem{{Agent: "preparer", ContractID: "prepare", Phase: PhasePrepare, Status: TaskDone}}); err != nil {
		t.Fatal(err)
	}
	if err := w.observe([]*TodoItem{{Agent: "auditor", ContractID: "audit", Phase: PhaseAudit, Status: TaskDone}}); err != nil {
		t.Fatal(err)
	}
	got, err := w.executeAction(context.Background(), action)
	if err != nil {
		t.Fatalf("executeAction: %v", err)
	}
	if got != `{"status":"ok"}` || provider.validated != 1 || provider.executed != 1 {
		t.Fatalf("action result/calls = %q, validate=%d execute=%d", got, provider.validated, provider.executed)
	}
}

func TestCoordinatorRuntimeActionCompletesTodoWithoutUsingCache(t *testing.T) {
	session := workflowTestSession(t)
	provider := &recordingActionProvider{result: "applied"}
	registry := NewProviderRegistry()
	registry.Register("structured-actions", provider)
	session.ProviderRegistry = registry
	w, err := newRuntimeWorkflow(session)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Start(); err != nil {
		t.Fatal(err)
	}
	if err := w.observe([]*TodoItem{{Agent: "preparer", ContractID: "prepare", Phase: PhasePrepare, Status: TaskDone}}); err != nil {
		t.Fatal(err)
	}
	if err := w.observe([]*TodoItem{{Agent: "auditor", ContractID: "audit", Phase: PhaseAudit, Status: TaskDone}}); err != nil {
		t.Fatal(err)
	}
	tracker := NewTaskTracker()
	task := TaskDef{ID: "execute", Agent: "executor", Goal: "apply", Phase: PhaseExecute, Action: &Action{Capability: "structured-actions", Type: "apply"}}
	item := tracker.TodoList().AddBatch([]TodoSpec{{PlanTaskID: task.ID, Phase: task.Phase, ContractID: task.ID, Action: task.Action, Agent: task.Agent, Desc: task.Goal}})[0]
	c := &Coordinator{session: session, taskTracker: tracker, phaseWorkflow: w}
	output, err := c.executeTask(context.Background(), task, item.ID)
	if err != nil {
		t.Fatalf("executeTask: %v", err)
	}
	if output != "applied" || provider.executed != 1 {
		t.Fatalf("action output/calls = %q/%d", output, provider.executed)
	}
	got := tracker.TodoList().Items()[0]
	if got.Status != TaskDone || got.Output != "applied" {
		t.Fatalf("action todo lifecycle = %#v", got)
	}
}

func TestCoordinatorRuntimeActionPreservesProviderFailureForPhaseResult(t *testing.T) {
	session := workflowTestSession(t)
	provider := &recordingActionProvider{err: fmt.Errorf("adapter unavailable")}
	registry := NewProviderRegistry()
	registry.Register("structured-actions", provider)
	session.ProviderRegistry = registry
	w, err := newRuntimeWorkflow(session)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Start(); err != nil {
		t.Fatal(err)
	}
	if err := w.observe([]*TodoItem{{Agent: "preparer", ContractID: "prepare", Phase: PhasePrepare, Status: TaskDone}}); err != nil {
		t.Fatal(err)
	}
	if err := w.observe([]*TodoItem{{Agent: "auditor", ContractID: "audit", Phase: PhaseAudit, Status: TaskDone}}); err != nil {
		t.Fatal(err)
	}
	tracker := NewTaskTracker()
	task := TaskDef{ID: "execute", Agent: "executor", Goal: "apply", Phase: PhaseExecute, Action: &Action{Capability: "structured-actions", Type: "apply"}}
	item := tracker.TodoList().AddBatch([]TodoSpec{{PlanTaskID: task.ID, Phase: task.Phase, ContractID: task.ID, Action: task.Action, Agent: task.Agent, Desc: task.Goal}})[0]
	c := &Coordinator{session: session, taskTracker: tracker, phaseWorkflow: w}
	if _, err := c.executeTask(context.Background(), task, item.ID); err == nil {
		t.Fatal("provider failure unexpectedly succeeded")
	}
	failed := tracker.TodoList().Items()[0]
	if failed.RuntimeError == nil || failed.RuntimeError.Source != "structured-actions" || failed.RuntimeError.Category != CategoryProviderFailure {
		t.Fatalf("runtime failure = %#v", failed.RuntimeError)
	}
	if err := w.observe([]*TodoItem{failed}); err == nil {
		t.Fatal("failed provider action did not fail workflow")
	}
	state, results, _, _ := w.snapshot()
	if state != PhaseFailed || results[PhaseExecute].Errors[0].Source != "structured-actions" || results[PhaseExecute].Errors[0].Category != CategoryProviderFailure {
		t.Fatalf("phase result lost provider failure metadata: state=%s results=%#v", state, results[PhaseExecute])
	}
}

func TestRuntimeWorkflowRetriesProviderFailureBySignatureAndRestoresIt(t *testing.T) {
	session := workflowTestSession(t)
	w, err := newRuntimeWorkflow(session)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Start(); err != nil {
		t.Fatal(err)
	}
	// The retry identity is phase-sensitive, so move to EXECUTE before recording.
	if err := w.observe([]*TodoItem{{Agent: "preparer", ContractID: "prepare", Phase: PhasePrepare, Status: TaskDone}}); err != nil {
		t.Fatal(err)
	}
	if err := w.observe([]*TodoItem{{Agent: "auditor", ContractID: "audit", Phase: PhaseAudit, Status: TaskDone}}); err != nil {
		t.Fatal(err)
	}
	task := TaskDef{Agent: "executor", MaxRetries: 2, Action: &Action{Capability: "structured-actions", Type: "apply"}}
	err = ActionProviderError{Capability: "structured-actions", Cause: fmt.Errorf("temporary unavailable request abcdef123456")}
	if !w.permitActionRetry(task, err) || !w.permitActionRetry(task, err) {
		t.Fatal("first two signature-limited retries should be allowed")
	}
	state, results, root, retryState := w.snapshot()
	if err := SaveSession(session.Workspace, &SessionData{
		WorkflowState: state, PhaseResults: results, RuntimeWorkspace: root, RetryState: retryState,
	}); err != nil {
		t.Fatalf("SaveSession: %v", err)
	}
	// Prove the saved checkpoint owns its retry map instead of retaining the
	// caller's pointer after checkpointing.
	for key := range retryState.Attempts {
		retryState.Attempts[key] = 99
	}
	loaded := LoadSession(session.Workspace)
	if loaded == nil || loaded.RetryState == nil || len(loaded.RetryState.Attempts) != 1 {
		t.Fatalf("saved retry checkpoint = %#v", loaded)
	}
	restored, restoreErr := newRuntimeWorkflow(session)
	if restoreErr != nil {
		t.Fatal(restoreErr)
	}
	coordinator := &Coordinator{session: session, taskTracker: NewTaskTracker(), phaseWorkflow: restored}
	coordinator.SetSessionData(loaded)
	if restored.permitActionRetry(task, err) {
		t.Fatal("restored retry state allowed retry beyond signature limit")
	}
}

func TestRuntimeWorkflowRetryPoliciesKeepSignaturesAndPermanentFailuresDistinct(t *testing.T) {
	session := workflowTestSession(t)
	session.Config.Retry.Transient.MaxAttempts = 1
	session.Config.Retry.Repair.MaxAttemptsPerFailureSignature = 1
	w, err := newRuntimeWorkflow(session)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Start(); err != nil {
		t.Fatal(err)
	}
	if err := w.observe([]*TodoItem{{Agent: "preparer", ContractID: "prepare", Phase: PhasePrepare, Status: TaskDone}}); err != nil {
		t.Fatal(err)
	}
	if err := w.observe([]*TodoItem{{Agent: "auditor", ContractID: "audit", Phase: PhaseAudit, Status: TaskDone}}); err != nil {
		t.Fatal(err)
	}
	action := TaskDef{Agent: "executor", Action: &Action{Capability: "structured-actions", Type: "apply"}}
	first := ActionProviderError{Capability: "structured-actions", Cause: fmt.Errorf("connection reset aaaabbbbcccc")}
	second := ActionProviderError{Capability: "structured-actions", Cause: fmt.Errorf("connection reset dddd11112222")}
	if !w.permitActionRetry(action, first) {
		t.Fatal("first transient retry should be allowed")
	}
	if w.permitActionRetry(action, second) {
		t.Fatal("normalized transient IDs should share one exhausted retry budget")
	}
	// A different generic source/action type is a different signature.
	action.Action.Type = "reconcile"
	if !w.permitActionRetry(action, first) {
		t.Fatal("distinct action signature should retain its own transient budget")
	}
	before := len(w.retryState.Attempts)
	if w.permitActionRetry(action, ActionValidationError{Capability: "structured-actions", Cause: fmt.Errorf("invalid action")}) {
		t.Fatal("validation failure must not be retried")
	}
	if len(w.retryState.Attempts) != before {
		t.Fatal("permanent validation failure consumed retry state")
	}
}

func TestRuntimeWorkflowAppliesConfiguredRepairLimitByFailureSignature(t *testing.T) {
	session := workflowTestSession(t)
	session.Config.Retry.Repair.MaxAttemptsPerFailureSignature = 1
	w, err := newRuntimeWorkflow(session)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Start(); err != nil {
		t.Fatal(err)
	}
	if err := w.observe([]*TodoItem{{Agent: "preparer", ContractID: "prepare", Phase: PhasePrepare, Status: TaskDone}}); err != nil {
		t.Fatal(err)
	}
	if err := w.observe([]*TodoItem{{Agent: "auditor", ContractID: "audit", Phase: PhaseAudit, Status: TaskDone}}); err != nil {
		t.Fatal(err)
	}
	task := TaskDef{ID: "execute", Agent: "executor", MaxRetries: 9}
	failure := fmt.Errorf("tool connection reset 123456789abcdef")
	if !w.permitRepairRetry(task, failure) {
		t.Fatal("first configured repair attempt should be allowed")
	}
	if w.permitRepairRetry(task, failure) {
		t.Fatal("configured repair signature limit was not enforced")
	}
}

func TestRuntimeWorkflowRequiresObjectiveVerifyContractWhenConfigured(t *testing.T) {
	session := workflowTestSession(t)
	session.Config.Verification.Required = true
	if err := validateRuntimeWorkflowTeam(session, workflowTestRegistry()); err == nil {
		t.Fatal("required verification without objective verify contract was accepted")
	}
	session.ContractTasks[3].Verify = "test -f runtime/artifacts/result.json"
	if err := validateRuntimeWorkflowTeam(session, workflowTestRegistry()); err != nil {
		t.Fatalf("objective verify contract rejected: %v", err)
	}
}

func TestRuntimeWorkflowRequiresSuccessfulVerifyEvidenceBeforeDone(t *testing.T) {
	session := workflowTestSession(t)
	session.Config.Verification.Required = true
	session.ContractTasks[3].Verify = "test -f runtime/artifacts/result.json"
	w, err := newRuntimeWorkflow(session)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Start(); err != nil {
		t.Fatal(err)
	}
	for _, item := range []*TodoItem{
		{Agent: "preparer", ContractID: "prepare", Phase: PhasePrepare, Status: TaskDone},
		{Agent: "auditor", ContractID: "audit", Phase: PhaseAudit, Status: TaskDone},
		{Agent: "executor", ContractID: "execute", Phase: PhaseExecute, Status: TaskDone},
	} {
		if err := w.observe([]*TodoItem{item}); err != nil {
			t.Fatal(err)
		}
	}
	missingEvidence := &TodoItem{Agent: "verifier", ContractID: "verify", Phase: PhaseVerify, Status: TaskDone}
	if err := w.observe([]*TodoItem{missingEvidence}); err == nil {
		t.Fatal("verify phase completed without objective evidence")
	}
}

func TestRuntimeWorkflowRejectsSkippedConfiguredPhase(t *testing.T) {
	session := workflowTestSession(t)
	session.Config.Workflow.Phases = []string{"prepare", "execute", "verify"}
	if _, err := newRuntimeWorkflow(session); err == nil || !strings.Contains(err.Error(), "skipping") {
		t.Fatalf("newRuntimeWorkflow error = %v, want skipped phase rejection", err)
	}
}

func TestRuntimeWorkflowRejectsCompetingInitialBatchPolicy(t *testing.T) {
	session := workflowTestSession(t)
	session.Config.Delegation.RequireExactInitialBatch = true
	if err := validateRuntimeWorkflowTeam(session, workflowTestRegistry()); err == nil || !strings.Contains(err.Error(), "initial-batch") {
		t.Fatalf("validateRuntimeWorkflowTeam error = %v, want initial batch conflict", err)
	}
}

func TestRuntimeWorkflowAddsControlledWorkspacePath(t *testing.T) {
	session := workflowTestSession(t)
	w, err := newRuntimeWorkflow(session)
	if err != nil {
		t.Fatal(err)
	}
	c := &Coordinator{phaseWorkflow: w}
	paths := c.runtimeAllowedPaths([]string{"/project/source"})
	if len(paths) != 2 || paths[1] != filepath.Join(session.Workspace, "runtime") {
		t.Fatalf("runtime allowed paths = %#v", paths)
	}
}

func TestRuntimeWorkflowWriteIsolationIntegration(t *testing.T) {
	session := workflowTestSession(t)
	session.Agents["preparer"].Tools = "all"
	w, err := newRuntimeWorkflow(session)
	if err != nil {
		t.Fatal(err)
	}
	// The test needs to run execution tools (bash, lua, write), which are now restricted
	// to the EXECUTE phase by runtime enforcement. Transition the workflow to EXECUTE.
	w.state = PhaseExecute
	c := &Coordinator{phaseWorkflow: w, session: session}

	ctx := context.Background()
	ctx = c.withEffectiveToolsAllowed(ctx, session.Agents["preparer"], []string{"bash", "sudo", "lua", "create_skill", "write"})
	ctx = context.WithValue(ctx, tools.AgentAllowedWritePathsKey, c.runtimeAllowedPaths(nil))

	cwd, _ := os.Getwd()
	builtTools := agent.BuildAllAgentTools(cwd)

	tmpBash := filepath.Join(t.TempDir(), "workflow-escape-bash")
	tmpLua := filepath.Join(t.TempDir(), "workflow-escape-lua")
	randomSuffix := filepath.Base(t.TempDir())
	cwdBash := filepath.Join(cwd, "workflow-escape-bash-test-"+randomSuffix)
	cwdLua := filepath.Join(cwd, "workflow-escape-lua-test-"+randomSuffix)
	cwdSkill := filepath.Join(cwd, "skills", "workflow-escape-skill-"+randomSuffix)
	defer os.RemoveAll(cwdBash)
	defer os.RemoveAll(cwdLua)
	defer os.RemoveAll(cwdSkill)

	tests := []struct {
		testName    string
		toolName    string
		input       string
		expectError bool
		checkPath   string
	}{
		{"bash tmp", "bash", `{"command": "printf x > ` + tmpBash + `"}`, true, tmpBash},
		{"bash cwd", "bash", `{"command": "printf x > ` + cwdBash + `"}`, true, cwdBash},
		{"lua tmp", "lua", `{"code": "assert(io.open('` + tmpLua + `', 'w'))"}`, true, tmpLua},
		{"lua cwd", "lua", `{"code": "assert(io.open('` + cwdLua + `', 'w'))"}`, true, cwdLua},
		{"lua output tmp", "lua", `{"code": "io.output('` + tmpLua + `'); io.write('x')"}`, true, tmpLua},
		{"lua output cwd", "lua", `{"code": "io.output('` + cwdLua + `'); io.write('x')"}`, true, cwdLua},
		{"create_skill cwd", "create_skill", `{"name": "workflow-escape-skill-` + randomSuffix + `", "description": "d", "content": "c"}`, true, cwdSkill},
		{"write allowed", "write", `{"file_path": "` + filepath.Join(session.Workspace, "runtime", "allowed.txt") + `", "content": "allowed"}`, false, filepath.Join(session.Workspace, "runtime", "allowed.txt")},
		{"lua open allowed", "lua", `{"code": "assert(io.open('` + filepath.Join(session.Workspace, "runtime", "allowed.lua") + `', 'w'))"}`, false, filepath.Join(session.Workspace, "runtime", "allowed.lua")},
	}

	for _, tt := range tests {
		t.Run(tt.testName, func(t *testing.T) {
			var tool fantasy.AgentTool
			for _, t := range builtTools {
				if t.Info().Name == tt.toolName {
					tool = t
					break
				}
			}
			if tool == nil {
				t.Fatalf("tool %s not found", tt.toolName)
			}

			if tt.checkPath != "" {
				if _, err := os.Stat(tt.checkPath); err == nil {
					t.Fatalf("test precondition failed: path already exists: %s", tt.checkPath)
				}
			}
			if !tt.expectError {
				os.MkdirAll(filepath.Dir(filepath.Join(session.Workspace, "runtime", "allowed.txt")), 0o755)
			}

			resp, err := tool.Run(ctx, fantasy.ToolCall{Input: tt.input})
			if err != nil {
				t.Fatalf("unexpected system err: %v", err)
			}

			if tt.expectError && !resp.IsError {
				t.Errorf("expected tool %s to fail due to write isolation, got success: %v", tt.toolName, resp)
			} else if !tt.expectError && resp.IsError {
				t.Errorf("expected tool %s to succeed, got error: %v", tt.toolName, resp)
			}

			if tt.checkPath != "" {
				_, err := os.Stat(tt.checkPath)
				if tt.expectError && err == nil {
					t.Errorf("file was created despite isolation: %s", tt.checkPath)
				} else if !tt.expectError && os.IsNotExist(err) {
					t.Errorf("expected file to be created, but it was not: %s", tt.checkPath)
				}
			}
		})
	}
}

func TestPhaseCapabilityExecutionBlockIntegration(t *testing.T) {
	session := workflowTestSession(t)
	session.Agents["preparer"].Tools = "all"
	w, err := newRuntimeWorkflow(session)
	if err != nil {
		t.Fatal(err)
	}
	// Leave state as PhasePrepare (which is what it defaults to, or just explicitly set it)
	w.state = PhasePrepare
	c := &Coordinator{phaseWorkflow: w, session: session}

	ctx := context.Background()
	// Ask for all execution tools. The PhaseCapability logic should filter them out in PhasePrepare.
	ctx = c.withEffectiveToolsAllowed(ctx, session.Agents["preparer"], []string{"bash", "terminal", "terminal_start", "wait_for", "download", "terminal_wait"})

	cwd, _ := os.Getwd()
	builtTools := agent.BuildAllAgentTools(cwd)
	builtTools = append(builtTools, &terminalTool{coordinator: c}, &terminalWaitTool{coordinator: c})

	tests := []struct {
		testName string
		toolName string
		input    string
	}{
		{"terminal start blocked", "terminal", `{"action":"start","command":["sh","-c","echo 1"]}`},
		{"wait_for blocked", "wait_for", `{"command":"sleep 1","timeout_seconds":5}`},
		{"bash blocked", "bash", `{"command":"echo 1"}`},
		{"download blocked", "download", `{"url":"http://example.com","file_path":"dl.txt"}`},
		{"terminal_wait blocked", "terminal_wait", `{"id":"session-1"}`},
	}

	for _, tt := range tests {
		t.Run(tt.testName, func(t *testing.T) {
			var tool fantasy.AgentTool
			for _, t := range builtTools {
				if t.Info().Name == tt.toolName {
					tool = t
					break
				}
			}
			if tool == nil {
				t.Fatalf("tool %s not found", tt.toolName)
			}

			resp, err := tool.Run(ctx, fantasy.ToolCall{Input: tt.input})
			if err != nil {
				t.Fatalf("unexpected system err: %v", err)
			}

			if !resp.IsError || !strings.Contains(resp.Content, "not permitted") {
				t.Errorf("expected tool %s to fail with not permitted, got response: %v", tt.toolName, resp.Content)
			}
		})
	}
}

func TestPhaseCapabilityMCPBlockIntegration(t *testing.T) {
	session := workflowTestSession(t)
	session.Agents["preparer"].MCPTools = map[string]agent.MCPToolConfig{
		"destructive_mcp": {Cmd: "echo 1"},
	}
	session.Agents["preparer"].Tools = "destructive_mcp,all"

	w, err := newRuntimeWorkflow(session)
	if err != nil {
		t.Fatal(err)
	}
	w.state = PhasePrepare
	c := &Coordinator{phaseWorkflow: w, session: session, mcpManager: mcp.NewMCPToolManager("", "")}

	ctx := context.Background()
	ctx = c.withEffectiveToolsAllowedForTask(ctx, session.Agents["preparer"], []string{}, TaskDef{})
	allowed := tools.GetToolsAllowed(ctx)
	if slices.Contains(allowed, "destructive_mcp") {
		t.Fatalf("MCP tool was permitted in PREPARE phase runtime allowlist")
	}

	w.state = PhaseExecute
	ctx = c.withEffectiveToolsAllowedForTask(ctx, session.Agents["preparer"], []string{}, TaskDef{})
	allowed = tools.GetToolsAllowed(ctx)
	if !slices.Contains(allowed, "destructive_mcp") {
		t.Fatalf("MCP tool was incorrectly blocked in EXECUTE phase runtime allowlist: %v", allowed)
	}
}

func TestPhaseCapabilityMCPBlockModelVisible(t *testing.T) {
	session := workflowTestSession(t)
	session.Agents["preparer"].MCPTools = map[string]agent.MCPToolConfig{
		"destructive_mcp": {Cmd: "echo 1"},
	}
	session.Agents["preparer"].Tools = "all"
	session.Agents["preparer"].Generation.Model = "test-model"

	w, err := newRuntimeWorkflow(session)
	if err != nil {
		t.Fatal(err)
	}
	w.state = PhasePrepare
	pm, _ := agent.NewProviderManager("", "", nil)
	c := &Coordinator{phaseWorkflow: w, session: session, providerManager: pm, mcpManager: mcp.NewMCPToolManager("", "")}

	_, toolNames, err := c.createTaskAgentWithResultTool(context.Background(), session.Agents["preparer"], "", nil, TaskDef{})
	if err != nil && !strings.Contains(err.Error(), "no exact tokenizer") {
		t.Fatalf("unexpected PREPARE err: %v", err)
	}
	if slices.Contains(toolNames, "destructive_mcp") {
		t.Fatalf("MCP tool was exposed to model in PREPARE phase")
	}

	w.state = PhaseExecute
	_, toolNames, err = c.createTaskAgentWithResultTool(context.Background(), session.Agents["preparer"], "", nil, TaskDef{})
	if err != nil && !strings.Contains(err.Error(), "no exact tokenizer") && !strings.Contains(err.Error(), "load MCP server") {
		t.Fatalf("err: %v", err)
	}
	if !slices.Contains(toolNames, "destructive_mcp") {
		t.Fatalf("MCP tool was blocked from model in EXECUTE phase: %v, err: %v", toolNames, err)
	}
}
