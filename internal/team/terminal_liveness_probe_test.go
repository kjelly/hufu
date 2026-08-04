package team

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/anomalyco/hufu/internal/agent"
	"github.com/anomalyco/hufu/internal/tools"
)

// TestTerminalLivenessProbeReportsRealProcessFacts checks the adapter against a
// real session manager rather than a stub, because the whole value of the probe
// rests on one assumption about the manager: that its process watcher records
// the exit itself, so List is already current and no Reconcile is needed. If
// that stopped holding, wait_for would go back to polling dead processes.
func TestTerminalLivenessProbeReportsRealProcessFacts(t *testing.T) {
	workspace := t.TempDir()
	manager, err := NewTerminalSessionManager(workspace, func(string, string, map[string]interface{}) {})
	if err != nil {
		t.Fatal(err)
	}
	c := &Coordinator{
		session:            &TeamSession{Workspace: workspace, Config: agent.TeamConfig{Name: "team"}},
		terminalSessionMgr: manager,
		executionRunID:     "run-liveness",
	}
	probe := c.terminalLivenessProbe()
	if probe == nil {
		t.Fatal("a coordinator with a terminal manager must supply a probe")
	}

	ownerCtx := WithTerminalTaskID(context.Background(), "task-a")
	// Exits immediately and non-zero, exactly like the deploy that died on a
	// missing secret two seconds after it was started.
	session, err := manager.Start(ownerCtx, TerminalStartRequest{
		RunID: "run-liveness", OwnerTaskID: "task-a", Agent: "worker",
		Command: []string{"sh", "-c", "printf 'secret environment variable is not set\\n' >&2; exit 1"},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	completed := waitForTerminal(t, manager, session.ID, 5*time.Second)
	if completed.Running || completed.ExitCode == nil {
		t.Fatalf("precondition: session did not exit: %+v", completed)
	}

	fact, known := probe(context.Background(), session.ID)
	if !known {
		t.Fatal("the probe must know a session this manager started")
	}
	if fact.Running {
		t.Error("an exited process must not be reported as running")
	}
	if fact.State != string(TerminalSessionExited) {
		t.Errorf("state = %q, want %q", fact.State, TerminalSessionExited)
	}
	if fact.ExitCode == nil || *fact.ExitCode == 0 {
		t.Errorf("exit code = %v, want the child's non-zero status", fact.ExitCode)
	}
	if fact.ExitedAt.IsZero() {
		t.Error("ExitedAt must be set so wait_for can tell a pre-existing death from a mid-wait one")
	}

	// The probe must not consume the agent's unread output: TerminalSessionManager
	// .Read advances the session's read offset, so a probe that fetched output
	// would silently steal what terminal_read has not seen yet.
	read, err := manager.Read(ownerCtx, session.ID)
	if err != nil {
		t.Fatalf("Read after probing: %v", err)
	}
	if len(read.Output) == 0 {
		t.Error("the probe consumed the session's unread output")
	}

	if _, known := probe(context.Background(), "term-000000000000000000000000"); known {
		t.Error("an unknown session id must report no information, not a dead process")
	}
}

// TestTerminalLivenessProbeAbsentWithoutManager keeps the injection inert for a
// coordinator that has no terminal manager, so no caller needs a special case.
func TestTerminalLivenessProbeAbsentWithoutManager(t *testing.T) {
	if probe := (&Coordinator{}).terminalLivenessProbe(); probe != nil {
		t.Fatal("a coordinator with no terminal manager must supply no probe")
	}
	var nilCoordinator *Coordinator
	if probe := nilCoordinator.terminalLivenessProbe(); probe != nil {
		t.Fatal("a nil coordinator must supply no probe")
	}
	// And the tools-side attachment must tolerate a nil probe.
	ctx := tools.WithTerminalLiveness(context.Background(), nil)
	if ctx.Value(tools.TerminalLivenessKey) != nil {
		t.Error("a nil probe must not be attached to the context")
	}
}

// TestTerminalLivenessProbeIsScopedToTheRun stops a wait from being ended by a
// process fact belonging to a different run.
func TestTerminalLivenessProbeIsScopedToTheRun(t *testing.T) {
	workspace := t.TempDir()
	manager, err := NewTerminalSessionManager(workspace, func(string, string, map[string]interface{}) {})
	if err != nil {
		t.Fatal(err)
	}
	ownerCtx := WithTerminalTaskID(context.Background(), "task-a")
	session, err := manager.Start(ownerCtx, TerminalStartRequest{
		RunID: "run-one", OwnerTaskID: "task-a", Agent: "worker",
		Command: []string{"sh", "-c", "exit 0"},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitForTerminal(t, manager, session.ID, 5*time.Second)

	other := &Coordinator{
		session:            &TeamSession{Workspace: workspace, Config: agent.TeamConfig{Name: "team"}},
		terminalSessionMgr: manager,
		executionRunID:     "run-two",
	}
	if _, known := other.terminalLivenessProbe()(context.Background(), session.ID); known {
		t.Error("a session from another run must not be visible to this run's waits")
	}
}

// TestWorkerTaskContextCarriesTerminalLiveness is an architectural fitness
// check. The probe reaches wait_for only through the worker's task context, and
// nothing fails if that one line goes missing — waits would simply go back to
// polling dead processes until they time out, which is invisible in every test
// that does not measure wall-clock time. This keeps the wiring present.
func TestWorkerTaskContextCarriesTerminalLiveness(t *testing.T) {
	src, err := os.ReadFile("coordinator_task_run.go")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !strings.Contains(string(src), "tools.WithTerminalLiveness(taskCtx, c.terminalLivenessProbe())") {
		t.Fatal("the worker task context must attach c.terminalLivenessProbe() via tools.WithTerminalLiveness; without it wait_for cannot tell that the process it is waiting on has already exited")
	}
}
