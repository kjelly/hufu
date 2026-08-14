package team

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/kjelly/hufu/internal/agent"
	contextstore "github.com/kjelly/hufu/internal/context"
)

type statusProjectionSource struct {
	sessions []TerminalSession
	err      error
}

func (s statusProjectionSource) List(context.Context, string) ([]TerminalSession, error) {
	return s.sessions, s.err
}

func readProjectedStatus(t *testing.T, workspace, agent string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(workspace, statusDir, agent+".yml"))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestStatusProjectionTerminationPathsKeepCanonicalStateAndStatusInSync(t *testing.T) {
	tests := []struct {
		name     string
		status   TaskStatus
		expected string
		session  []TerminalSession
	}{
		{name: "normal termination", status: TaskDone, expected: "idle"},
		{name: "partial termination", status: TaskPending, expected: "idle"},
		{name: "error termination", status: TaskError, expected: "error"},
		{name: "cancel termination", status: TaskPaused, expected: "paused", session: []TerminalSession{{OwnerTaskID: "1", Agent: "worker", Running: true, State: TerminalSessionRunning, Controller: TerminalControllerUser}}},
		{name: "crash resume", status: TaskInProgress, expected: "error", session: []TerminalSession{{OwnerTaskID: "1", Agent: "worker", State: TerminalSessionUnknown}}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			workspace := t.TempDir()
			item := &TodoItem{ID: "1", Agent: "worker", Status: tt.status}
			if err := ReconcileAgentStatusesFromSource(context.Background(), workspace, []*TodoItem{item}, statusProjectionSource{sessions: tt.session}); err != nil {
				t.Fatal(err)
			}
			status := readProjectedStatus(t, workspace, "worker")
			if !strings.Contains(status, "status: "+tt.expected) {
				t.Fatalf("status = %q, want %q", status, tt.expected)
			}
		})
	}
}

func TestStatusProjectionListingFailureDoesNotReplaceLastKnownStatus(t *testing.T) {
	workspace := t.TempDir()
	if err := os.MkdirAll(filepath.Join(workspace, statusDir), 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(workspace, statusDir, "worker.yml")
	if err := os.WriteFile(path, []byte("status: working\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := ReconcileAgentStatusesFromSource(context.Background(), workspace, []*TodoItem{{ID: "1", Agent: "worker", Status: TaskDone}}, statusProjectionSource{err: errors.New("session store unavailable")})
	if err == nil || !strings.Contains(err.Error(), "session store unavailable") {
		t.Fatalf("listing error = %v, want propagated source error", err)
	}
	if got := readProjectedStatus(t, workspace, "worker"); got != "status: working\n" {
		t.Fatalf("status changed after listing failure: %q", got)
	}
}

func TestCanonicalTodoTransitionAutomaticallyProjectsStatus(t *testing.T) {
	workspace := t.TempDir()
	c := &Coordinator{
		session:     &TeamSession{Workspace: workspace},
		sessionData: NewSession(),
		taskTracker: NewTaskTracker(),
	}
	c.SetSessionData(c.sessionData)
	item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "canonical transition"}})[0]
	if err := c.taskTracker.TodoList().TryUpdateStatusAndOutput(item.ID, TaskInProgress, "running", ""); err != nil {
		t.Fatal(err)
	}
	if got := readProjectedStatus(t, workspace, "worker"); !strings.Contains(got, "status: working") {
		t.Fatalf("automatic projection after canonical transition = %q, want working", got)
	}
	if err := c.taskTracker.TodoList().TryUpdateStatusAndOutput(item.ID, TaskDone, "done", "output"); err != nil {
		t.Fatal(err)
	}
	if got := readProjectedStatus(t, workspace, "worker"); !strings.Contains(got, "status: idle") {
		t.Fatalf("automatic projection after terminal transition = %q, want idle", got)
	}
}

func TestRunReconcilesRestoredKilledRunStatusBeforeCoordinatorDispatch(t *testing.T) {
	workspace := t.TempDir()
	if err := os.MkdirAll(filepath.Join(workspace, statusDir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, statusDir, "worker.yml"), []byte("status: working\ndetail: stale process state\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	worker := &agent.AgentDef{Name: "worker", Role: "worker"}
	c := &Coordinator{
		session: &TeamSession{
			Dir:       workspace,
			Workspace: workspace,
			Config:    agent.TeamConfig{Name: "resume-test"},
			Agents: map[string]*agent.AgentDef{
				"coordinator": {Name: "coordinator", Role: "coordinator"},
				"worker":      worker,
			},
		},
		taskTracker:  NewTaskTracker(),
		reportStatus: func(StatusEvent) {},
		projectDir:   workspace,
		sessionTime:  time.Now(),
	}
	// This represents a killed run: the canonical checkpoint still has a live
	// task, but recovery policy says it must be blocked rather than re-driven.
	// Run must rebuild the projection, apply resume policy, and only then begin
	// its coordinator turn.
	sd := NewSession()
	sd.Tasks = []*TodoItem{{ID: "1", Agent: "worker", Desc: "interrupted work", Status: TaskInProgress, Recovery: RecoveryManual}}
	c.SetSessionData(sd)
	c.runOrchestratorOverride = func(context.Context, *agent.AgentDef, string) (string, []fantasy.StepResult, error) {
		status := readProjectedStatus(t, workspace, "worker")
		if !strings.Contains(status, "status: error") || strings.Contains(status, "stale process state") {
			t.Fatalf("status at coordinator dispatch = %q, want reconciled blocked recovery", status)
		}
		return "FINISHED: resumed", nil, nil
	}

	if _, err := c.Run(context.Background(), "resume"); err != nil && !errors.Is(err, ErrTasksUnresolved) {
		t.Fatalf("Run: %v", err)
	}
	item := c.taskTracker.TodoList().Items()[0]
	if item.Status != TaskBlocked {
		t.Fatalf("resumed task status = %s, want %s", item.Status, TaskBlocked)
	}
}

type directTerminationAgent struct {
	err   error
	steps []fantasy.StepResult
}

func (a directTerminationAgent) Stream(context.Context, fantasy.AgentStreamCall) (*fantasy.AgentResult, error) {
	if a.err != nil {
		return nil, a.err
	}
	return &fantasy.AgentResult{Response: fantasy.Response{Content: fantasy.ResponseContent{
		fantasy.TextContent{Text: "direct agent completed"},
	}}, Steps: a.steps}, nil
}

func (a directTerminationAgent) Generate(context.Context, fantasy.AgentCall) (*fantasy.AgentResult, error) {
	if a.err != nil {
		return nil, a.err
	}
	return &fantasy.AgentResult{Response: fantasy.Response{Content: fantasy.ResponseContent{
		fantasy.TextContent{Text: "direct agent completed"},
	}}, Steps: a.steps}, nil
}

func newDirectTerminationCoordinator(t *testing.T, worker fantasy.Agent) *Coordinator {
	t.Helper()
	workspace := t.TempDir()
	def := &agent.AgentDef{Name: "worker", Role: "worker"}
	return &Coordinator{
		session:      &TeamSession{Dir: workspace, Workspace: workspace, Config: agent.TeamConfig{Name: "test", Timeout: 10}},
		sessionData:  NewSession(),
		taskTracker:  NewTaskTracker(),
		agentCache:   map[string]fantasy.Agent{"worker": worker},
		agentPool:    &mockAgentPool{resolveDef: def, resolveKey: "worker"},
		reportStatus: func(StatusEvent) {},
		projectDir:   workspace,
		sessionTime:  time.Now(),
	}
}

func TestRunDirectAgentFinalizesRunResult(t *testing.T) {
	c := newDirectTerminationCoordinator(t, directTerminationAgent{})
	result, err := c.RunDirectAgent(context.Background(), "worker", "perform direct work")
	if err != nil {
		t.Fatalf("RunDirectAgent: %v", err)
	}
	// A successful direct run is a complete run boundary: it must finalize a
	// run result through the completion gate, not leave the run open.
	if last := c.LastRunResult(); last == nil {
		t.Fatal("direct run did not finalize a run result")
	}
	// With no acceptance contract configured, the completion gate marks the
	// run unverified (not completed). The direct run must surface that as an
	// error so callers never report an unverified run as success.
	if result == nil || result.Error == nil {
		t.Fatalf("direct result = %#v, want unverified-run error (no acceptance gate)", result)
	}
	if !strings.Contains(result.Error.Error(), "not accepted") {
		t.Fatalf("direct result error = %v, want not-accepted message", result.Error)
	}
	if last := c.LastRunResult(); last == nil || last.Outcome != RunOutcomeUnverified || last.GoalSatisfied {
		t.Fatalf("direct run result = %#v, want unverified non-satisfied outcome", last)
	}
}

// TestRunDirectAgentPersistsExactlyOneReliabilityObservation guards against a
// regression where a direct run registered beginExecutionRun's terminal
// teardown twice, appending the same run's production observation twice to the
// durable reliability history. One direct run must append exactly one
// observation and emit exactly one terminal run event.
func TestRunDirectAgentPersistsExactlyOneReliabilityObservation(t *testing.T) {
	workspace := t.TempDir()
	obsCount := func() int {
		report, err := loadReliabilityEvalReport(workspace)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return 0
			}
			t.Fatalf("load reliability report: %v", err)
		}
		return len(report.ProductionObservations)
	}
	if got := obsCount(); got != 0 {
		t.Fatalf("initial observation count = %d, want 0", got)
	}
	c := newDirectTerminationCoordinator(t, directTerminationAgent{})
	c.session.Config.Reliability = agent.ReliabilityConfig{Rollout: string(RolloutShadow)}
	// RunDirectAgent uses the workspace from session.Workspace; the temp
	// workspace must match the one newDirectTerminationCoordinator allocates.
	c.session.Workspace = workspace
	if _, err := c.RunDirectAgent(context.Background(), "worker", "perform direct work"); err != nil {
		t.Fatalf("RunDirectAgent: %v", err)
	}
	if got := obsCount(); got != 1 {
		t.Fatalf("production observation count after one direct run = %d, want exactly 1 (no duplicate finalization)", got)
	}
}

func TestFinalizeDirectRunRejectsCandidatesOnUnverifiedRun(t *testing.T) {
	workspace := t.TempDir()
	repo, err := contextstore.OpenSQLite(filepath.Join(workspace, "context.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	c := &Coordinator{
		contextRepo: repo, projectDir: "project", executionRunID: "run-direct",
		session:     &TeamSession{Workspace: workspace, Config: agent.TeamConfig{Name: "team"}},
		taskTracker: NewTaskTracker(),
	}
	svc := NewSharedMemoryService(repo)
	if _, err := svc.Propose(context.Background(), SharedMemoryProposal{
		Scope: c.contextScope(), Content: "direct agent lesson", Section: ltmSectionPatterns,
		Source: "memory_save", RunID: "run-direct",
	}); err != nil {
		t.Fatal(err)
	}
	// A successful direct run with no acceptance gate is unverified; its
	// run-bound candidates must be rejected, not left pending forever.
	c.finalizeDirectRun(context.Background(), "task-1", true, "done")
	items, err := repo.Query(context.Background(), contextstore.RepositoryQuery{Scope: contextstore.Scope{ProjectID: "project", TeamID: "team"}, Visibility: contextstore.VisibilityExact, IncludeCandidates: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Lifecycle != contextstore.LifecycleRejected {
		t.Fatalf("unverified direct-run candidate lifecycle = %#v, want rejected", items)
	}
}

func TestFinalizeDirectRunRejectsCandidatesOnFailure(t *testing.T) {
	workspace := t.TempDir()
	repo, err := contextstore.OpenSQLite(filepath.Join(workspace, "context.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	c := &Coordinator{
		contextRepo: repo, projectDir: "project", executionRunID: "run-direct",
		session:     &TeamSession{Workspace: workspace, Config: agent.TeamConfig{Name: "team"}},
		taskTracker: NewTaskTracker(),
	}
	svc := NewSharedMemoryService(repo)
	if _, err := svc.Propose(context.Background(), SharedMemoryProposal{
		Scope: c.contextScope(), Content: "failed direct agent lesson", Section: ltmSectionPatterns,
		Source: "memory_save", RunID: "run-direct",
	}); err != nil {
		t.Fatal(err)
	}
	c.finalizeDirectRun(context.Background(), "task-1", false, "")
	items, err := repo.Query(context.Background(), contextstore.RepositoryQuery{Scope: contextstore.Scope{ProjectID: "project", TeamID: "team"}, Visibility: contextstore.VisibilityExact, IncludeCandidates: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Lifecycle != contextstore.LifecycleRejected {
		t.Fatalf("failed direct-run candidate lifecycle = %#v, want rejected", items)
	}
}

func TestRunDirectAgentTerminationReconcilesCanonicalTodoAndStatus(t *testing.T) {
	tests := []struct {
		name       string
		worker     fantasy.Agent
		ctx        func() context.Context
		wantStatus TaskStatus
		wantFile   string
	}{
		{name: "success", worker: directTerminationAgent{}, ctx: context.Background, wantStatus: TaskDone, wantFile: "status: idle"},
		{name: "worker error", worker: directTerminationAgent{err: errors.New("worker failed")}, ctx: context.Background, wantStatus: TaskError, wantFile: "status: error"},
		{name: "cancelled", worker: directTerminationAgent{err: context.Canceled}, ctx: context.Background, wantStatus: TaskError, wantFile: "status: error"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newDirectTerminationCoordinator(t, tt.worker)
			if tt.name == "cancelled" {
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				tt.ctx = func() context.Context { return ctx }
			}
			_, _ = c.RunDirectAgent(tt.ctx(), "worker", "perform direct work")
			items := c.taskTracker.TodoList().Items()
			if len(items) != 1 || items[0].Status != tt.wantStatus {
				t.Fatalf("canonical direct-agent tasks = %+v, want status %s", items, tt.wantStatus)
			}
			status := readProjectedStatus(t, c.session.Workspace, "worker")
			if !strings.Contains(status, tt.wantFile) {
				t.Fatalf("projected direct-agent status = %q, want %q", status, tt.wantFile)
			}
		})
	}
}

func TestRunDirectAgentEnforcesNoProgressBudget(t *testing.T) {
	c := newDirectTerminationCoordinator(t, directTerminationAgent{
		steps: []fantasy.StepResult{{Response: fantasy.Response{Usage: fantasy.Usage{TotalTokens: 2}}}},
	})
	c.session.Config.Reliability = agent.ReliabilityConfig{
		MaxTokensWithoutProgress:    1,
		MaxTokensWithoutProgressSet: true,
		MaxTurnsWithoutProgressSet:  true,
		MaxTasksWithoutProgressSet:  true,
		HardEnforcement:             true,
	}

	result, err := c.RunDirectAgent(context.Background(), "worker", "exceed the direct-agent budget")
	if err != nil {
		t.Fatalf("RunDirectAgent returned top-level error: %v", err)
	}
	if result == nil || result.Error == nil {
		t.Fatalf("direct result = %#v, want terminal budget error", result)
	}
	if !strings.Contains(result.Error.Error(), "no-progress budget exhausted") && !strings.Contains(result.Error.Error(), "direct agent stopped") {
		t.Fatalf("direct result error = %v, want no-progress stop", result.Error)
	}
	if last := c.LastRunResult(); last == nil || last.Outcome != RunOutcomePartial || last.StopReason != StopReasonBudgetExceeded || last.Continuation == nil {
		t.Fatalf("direct budget result = %#v, want partial budget continuation", last)
	}
	if items := c.taskTracker.TodoList().Items(); len(items) != 1 || items[0].Status != TaskDone {
		t.Fatalf("direct task state = %+v, want completed artifact with partial run outcome", items)
	}
}

func TestRunDirectAgentSurfacesNoProgressReplan(t *testing.T) {
	c := newDirectTerminationCoordinator(t, directTerminationAgent{})
	c.session.Config.Reliability = agent.ReliabilityConfig{
		MaxTokensWithoutProgress:    0,
		MaxTokensWithoutProgressSet: true,
		MaxTurnsWithoutProgress:     0,
		MaxTurnsWithoutProgressSet:  true,
		MaxTasksWithoutProgress:     1,
		MaxTasksWithoutProgressSet:  true,
		HardEnforcement:             true,
	}

	result, err := c.RunDirectAgent(context.Background(), "worker", "reach the first no-progress threshold")
	if err != nil {
		t.Fatalf("RunDirectAgent returned top-level error: %v", err)
	}
	if result == nil || result.Error == nil || !result.ReplanRequired {
		t.Fatalf("direct result = %#v, want explicit replan-required result", result)
	}
	if !strings.Contains(result.Error.Error(), "requires replan") {
		t.Fatalf("direct replan error = %v, want replan message", result.Error)
	}
	if !c.noProgressReplanPending() || c.IsWrapUp() {
		t.Fatalf("first threshold state: replan_pending=%v wrap_up=%v, want non-terminal replan", c.noProgressReplanPending(), c.IsWrapUp())
	}
	if last := c.LastRunResult(); last != nil {
		t.Fatalf("first threshold should not create terminal result, got %#v", last)
	}
}

func TestRunDirectAgentRejectsUnacceptedRun(t *testing.T) {
	c := newDirectTerminationCoordinator(t, directTerminationAgent{})
	// A failing acceptance gate makes the completion gate mark the run
	// partial (acceptance failed). The direct run must surface that as an
	// error so automation never reports an unaccepted run as completed.
	if err := c.SetAcceptance("false"); err != nil {
		t.Fatalf("SetAcceptance: %v", err)
	}
	result, err := c.RunDirectAgent(context.Background(), "worker", "perform direct work")
	if err != nil {
		t.Fatalf("RunDirectAgent returned top-level error: %v", err)
	}
	if result == nil || result.Error == nil {
		t.Fatalf("direct result = %#v, want acceptance-failed error", result)
	}
	if !strings.Contains(result.Error.Error(), "not accepted") {
		t.Fatalf("direct result error = %v, want not-accepted message", result.Error)
	}
	if last := c.LastRunResult(); last == nil || last.Outcome != RunOutcomePartial || last.StopReason != StopReasonAcceptanceFailed {
		t.Fatalf("direct run result = %#v, want partial acceptance-failed outcome", last)
	}
}

func TestRunDirectAgentAgentCreationFailureReconcilesCanonicalTodoAndStatus(t *testing.T) {
	c := newDirectTerminationCoordinator(t, directTerminationAgent{})
	// Empty model configuration makes agent.CreateAgent return its validation
	// error. An empty cache forces RunDirectAgent through that creation branch.
	c.agentCache = map[string]fantasy.Agent{}
	providerManager, err := agent.NewProviderManager("", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	c.providerManager = providerManager

	result, err := c.RunDirectAgent(context.Background(), "worker", "perform direct work")
	if err == nil || result != nil {
		t.Fatalf("agent creation result = %#v, err=%v; want creation failure", result, err)
	}
	items := c.taskTracker.TodoList().Items()
	if len(items) != 1 || items[0].Status != TaskError {
		t.Fatalf("canonical task after agent creation failure = %+v, want error", items)
	}
	if items[0].FailureEvent == nil || items[0].FailureEvent.FailureClass != FailureExecution || items[0].FailureEvent.Phase == "" || items[0].FailureEvent.RetryDisposition == "" {
		t.Fatalf("agent creation failure event = %#v, want execution class, phase, and disposition", items[0].FailureEvent)
	}
	status := readProjectedStatus(t, c.session.Workspace, "worker")
	if !strings.Contains(status, "status: error") {
		t.Fatalf("projected status after agent creation failure = %q", status)
	}
}

func TestCoordinatorAbortTerminationReconcilesCanonicalStateAndStatus(t *testing.T) {
	c := newDirectTerminationCoordinator(t, directTerminationAgent{})
	item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "coordinator task"}})[0]
	item.Status = TaskInProgress
	c.recordRunAborted(context.Canceled)
	if got := c.taskTracker.TodoList().Items()[0].Status; got != TaskInProgress {
		t.Fatalf("recordRunAborted unexpectedly changed canonical task to %s", got)
	}
	status := readProjectedStatus(t, c.session.Workspace, "coordinator")
	if !strings.Contains(status, "status: error") || !strings.Contains(status, "aborted") {
		t.Fatalf("coordinator abort status = %q", status)
	}
}

func TestCoordinatorNormalTerminationReconcilesCanonicalStateAndStatus(t *testing.T) {
	c := newDirectTerminationCoordinator(t, directTerminationAgent{})
	c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "coordinator task"}})
	c.finalizeNormalCompletion()
	items := c.taskTracker.TodoList().Items()
	if len(items) != 1 || items[0].Status != TaskSkipped {
		t.Fatalf("normal coordinator canonical tasks = %+v, want skipped pending task", items)
	}
	workerStatus := readProjectedStatus(t, c.session.Workspace, "worker")
	if !strings.Contains(workerStatus, "status: idle") {
		t.Fatalf("normal coordinator worker status = %q, want idle", workerStatus)
	}
	coordinatorStatus := readProjectedStatus(t, c.session.Workspace, "coordinator")
	if !strings.Contains(coordinatorStatus, "status: idle") {
		t.Fatalf("normal coordinator status = %q, want idle", coordinatorStatus)
	}
}

func TestCoordinatorUnexpectedTerminationProjectsFinalizedTaskStates(t *testing.T) {
	c := newDirectTerminationCoordinator(t, directTerminationAgent{})
	c.taskTracker.TodoList().AddBatch([]TodoSpec{
		{Agent: "worker", Desc: "in-flight task"},
		{Agent: "worker", Desc: "not-started task"},
	})
	items := c.taskTracker.TodoList().Items()
	items[0].Status = TaskInProgress
	// Update through the canonical API so the projection observes the same
	// state transitions as production execution.
	c.taskTracker.TodoList().UpdateStatus(items[0].ID, TaskInProgress, "running")
	c.finalizeRemainingTasks()

	finalItems := c.taskTracker.TodoList().Items()
	for _, item := range finalItems {
		if item.Status != TaskError && item.Status != TaskSkipped {
			t.Fatalf("finalized task %s retained non-terminal status %s", item.ID, item.Status)
		}
		if item.Status == TaskError && (item.FailureEvent == nil || item.FailureEvent.FailureClass == "" || item.FailureEvent.Phase == "" || item.FailureEvent.RetryDisposition == "") {
			t.Fatalf("finalized task %s missing structured failure event: %#v", item.ID, item.FailureEvent)
		}
	}
	status := readProjectedStatus(t, c.session.Workspace, "worker")
	if !strings.Contains(status, "status: error") {
		t.Fatalf("unexpected-termination projection = %q, want error from in-flight task", status)
	}
}
