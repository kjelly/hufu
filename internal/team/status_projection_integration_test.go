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
	"github.com/anomalyco/hufu/internal/agent"
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

type directTerminationAgent struct{ err error }

func (a directTerminationAgent) Stream(context.Context, fantasy.AgentStreamCall) (*fantasy.AgentResult, error) {
	if a.err != nil {
		return nil, a.err
	}
	return &fantasy.AgentResult{Response: fantasy.Response{Content: fantasy.ResponseContent{
		fantasy.TextContent{Text: "direct agent completed"},
	}}}, nil
}

func (a directTerminationAgent) Generate(context.Context, fantasy.AgentCall) (*fantasy.AgentResult, error) {
	if a.err != nil {
		return nil, a.err
	}
	return &fantasy.AgentResult{Response: fantasy.Response{Content: fantasy.ResponseContent{
		fantasy.TextContent{Text: "direct agent completed"},
	}}}, nil
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
	}
	status := readProjectedStatus(t, c.session.Workspace, "worker")
	if !strings.Contains(status, "status: error") {
		t.Fatalf("unexpected-termination projection = %q, want error from in-flight task", status)
	}
}
