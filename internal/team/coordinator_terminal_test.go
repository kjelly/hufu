package team

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/anomalyco/hufu/internal/agent"
)

func TestTerminalHandoffPausesAndResumesTaskRound(t *testing.T) {
	tracker := NewTaskTracker()
	item := tracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "interactive task"}})[0]
	tracker.TodoList().UpdateStatus(item.ID, TaskInProgress, "")
	coord := &Coordinator{session: &TeamSession{}, taskTracker: tracker, reportStatus: func(StatusEvent) {}}

	cancelled := make(chan struct{})
	coord.registerTerminalRound(item.ID, func() { close(cancelled) })
	session := TerminalSession{OwnerTaskID: item.ID}
	coord.pauseTerminalTask(session)
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("takeover did not cancel active model round")
	}
	if got := tracker.TodoList().Items()[0].Status; got != TaskPaused {
		t.Fatalf("status after takeover = %s, want %s", got, TaskPaused)
	}

	resumed := make(chan bool, 1)
	go func() { resumed <- coord.waitForTerminalResume(context.Background(), item.ID) }()
	select {
	case <-resumed:
		t.Fatal("model round resumed before terminal was released")
	case <-time.After(25 * time.Millisecond):
	}

	coord.resumeTerminalTask(session)
	select {
	case ok := <-resumed:
		if !ok {
			t.Fatal("waitForTerminalResume returned false")
		}
	case <-time.After(time.Second):
		t.Fatal("model round did not resume after terminal release")
	}
	if got := tracker.TodoList().Items()[0].Status; got != TaskInProgress {
		t.Fatalf("status after release = %s, want %s", got, TaskInProgress)
	}
}

func TestCoordinatorFinalizeTaskTerminalResourcesContainsLeakAndPreservesTaskError(t *testing.T) {
	manager, err := NewTerminalSessionManager(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	coord := &Coordinator{terminalSessionMgr: manager}
	manager.SetActiveTaskRoundChecker(coord.isTerminalRoundActive)
	owner := WithTerminalTaskID(context.Background(), "task-leak")
	if _, err := manager.Start(owner, TerminalStartRequest{
		RunID: "run-leak", OwnerTaskID: "task-leak", Command: []string{"sh", "-c", "sleep 30"},
	}); err != nil {
		t.Fatal(err)
	}

	original := errors.New("worker failed after starting terminal")
	got, blocked := coord.finalizeTaskTerminalResources(context.Background(), "task-leak", original)
	if blocked {
		t.Fatalf("contained terminal unexpectedly blocked task: %v", got)
	}
	if !errors.Is(got, original) {
		t.Fatalf("finalization error %v did not preserve original %v", got, original)
	}
	if err := manager.RequireTaskClosed("task-leak"); err != nil {
		t.Fatalf("contained terminal remained a retry gate: %v", err)
	}
	sessions, err := manager.List(context.Background(), "run-leak")
	if err != nil || len(sessions) != 1 {
		t.Fatalf("list sessions = %#v, err=%v", sessions, err)
	}
	if sessions[0].CleanupState != TerminalCleanupCompleted {
		t.Fatalf("cleanup state = %q, want completed", sessions[0].CleanupState)
	}
}

func TestCoordinatorTerminalTaskFailureOverridesWorkerSuccess(t *testing.T) {
	manager, err := NewTerminalSessionManager(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	coord := &Coordinator{terminalSessionMgr: manager, executionRunID: "run-evidence"}
	manager.SetActiveTaskRoundChecker(coord.isTerminalRoundActive)
	owner := WithTerminalTaskID(context.Background(), "task-evidence")
	if _, err := manager.Start(owner, TerminalStartRequest{
		RunID: "run-evidence", OwnerTaskID: "task-evidence",
		Command: []string{"sh", "-c", "exit 17"},
	}); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if err := coord.terminalTaskFailure(context.Background(), "task-evidence"); err != nil {
			if !strings.Contains(err.Error(), "status 17") {
				t.Fatalf("terminal failure = %v, want exit status evidence", err)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("terminalTaskFailure did not observe the non-zero child exit")
}

func TestCoordinatorTerminalTaskFailureIgnoresSupersededSession(t *testing.T) {
	manager, err := NewTerminalSessionManager(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	coord := &Coordinator{terminalSessionMgr: manager, executionRunID: "run-superseded"}
	manager.SetActiveTaskRoundChecker(coord.isTerminalRoundActive)
	owner := WithTerminalTaskID(context.Background(), "task-superseded")

	// The worker opens a session that fails (wrong TTY mode, a probe killed by
	// its own timeout, ...), abandons it, and later completes the same goal
	// through a second, successful session. The first, abandoned session must
	// not veto the second, genuinely successful one.
	if _, err := manager.Start(owner, TerminalStartRequest{
		RunID: "run-superseded", OwnerTaskID: "task-superseded",
		Command: []string{"sh", "-c", "exit 17"},
	}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		sessions, err := manager.List(context.Background(), "run-superseded")
		if err != nil {
			t.Fatal(err)
		}
		if len(sessions) == 1 && sessions[0].ExitCode != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("first session never reported its exit code")
		}
		time.Sleep(10 * time.Millisecond)
	}

	if _, err := manager.Start(owner, TerminalStartRequest{
		RunID: "run-superseded", OwnerTaskID: "task-superseded",
		Command: []string{"sh", "-c", "exit 0"},
	}); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(2 * time.Second)
	for {
		sessions, err := manager.List(context.Background(), "run-superseded")
		if err != nil {
			t.Fatal(err)
		}
		allObserved := len(sessions) == 2
		for _, s := range sessions {
			if s.ExitCode == nil {
				allObserved = false
			}
		}
		if allObserved {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("second session never reported its exit code")
		}
		time.Sleep(10 * time.Millisecond)
	}

	if err := coord.terminalTaskFailure(context.Background(), "task-superseded"); err != nil {
		t.Fatalf("terminalTaskFailure = %v, want nil (only the most recent session should count)", err)
	}
}

func TestCoordinatorCleanupRunTerminalResourcesContainsAllSessions(t *testing.T) {
	manager, err := NewTerminalSessionManager(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	coord := &Coordinator{terminalSessionMgr: manager, executionRunID: "run-stop"}
	manager.SetActiveTaskRoundChecker(coord.isTerminalRoundActive)
	for _, taskID := range []string{"task-one", "task-two"} {
		owner := WithTerminalTaskID(context.Background(), taskID)
		if _, err := manager.Start(owner, TerminalStartRequest{
			RunID: "run-stop", OwnerTaskID: taskID, Command: []string{"sh", "-c", "sleep 30"},
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := coord.cleanupRunTerminalResources(TerminalCleanupRunCancelled); err != nil {
		t.Fatal(err)
	}
	if err := manager.RequireNoLeaks("run-stop"); err != nil {
		t.Fatalf("run cleanup left a terminal leak: %v", err)
	}
}

func TestCoordinatorCleanupRunTerminalResourcesCancelsAndWaitsForActiveRound(t *testing.T) {
	manager, err := NewTerminalSessionManager(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	tracker := NewTaskTracker()
	item := tracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "terminal task"}})[0]
	tracker.TodoList().UpdateStatus(item.ID, TaskInProgress, "")
	coord := &Coordinator{terminalSessionMgr: manager, taskTracker: tracker, executionRunID: "run-active"}
	manager.SetActiveTaskRoundChecker(coord.isTerminalRoundActive)
	owner := WithTerminalTaskID(context.Background(), item.ID)
	if _, err := manager.Start(owner, TerminalStartRequest{
		RunID: "run-active", OwnerTaskID: item.ID, Command: []string{"sh", "-c", "sleep 30"},
	}); err != nil {
		t.Fatal(err)
	}
	roundCancelled := make(chan struct{})
	coord.registerTerminalRound(item.ID, func() { close(roundCancelled) })
	go func() {
		<-roundCancelled
		coord.unregisterTerminalRound(item.ID)
	}()
	if err := coord.cleanupRunTerminalResources(TerminalCleanupRunCancelled); err != nil {
		t.Fatalf("cleanup after cancelling active round: %v", err)
	}
	if coord.isTerminalRoundActive(item.ID) {
		t.Fatal("terminal round remained registered after shutdown cancellation")
	}
	if err := manager.RequireNoLeaks("run-active"); err != nil {
		t.Fatalf("active-round shutdown left a terminal leak: %v", err)
	}
}

func TestCoordinatorCleanupRunTerminalResourcesContainsTerminalAfterRoundTimeout(t *testing.T) {
	manager, err := NewTerminalSessionManager(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	tracker := NewTaskTracker()
	item := tracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "non-cooperative terminal task"}})[0]
	tracker.TodoList().UpdateStatus(item.ID, TaskInProgress, "")
	coord := &Coordinator{
		terminalSessionMgr:           manager,
		taskTracker:                  tracker,
		executionRunID:               "run-timeout",
		terminalRoundShutdownTimeout: 25 * time.Millisecond,
	}
	manager.SetActiveTaskRoundChecker(coord.isTerminalRoundActive)
	owner := WithTerminalTaskID(context.Background(), item.ID)
	if _, err := manager.Start(owner, TerminalStartRequest{
		RunID: "run-timeout", OwnerTaskID: item.ID, Command: []string{"sh", "-c", "sleep 30"},
	}); err != nil {
		t.Fatal(err)
	}
	cancelled := make(chan struct{})
	// Deliberately never unregister: this simulates a cancelled model round
	// that fails to cooperate with shutdown before the bounded wait expires.
	coord.registerTerminalRound(item.ID, func() { close(cancelled) })
	if err := coord.cleanupRunTerminalResources(TerminalCleanupRunCancelled); err != nil {
		t.Fatalf("timeout cleanup did not contain terminal: %v", err)
	}
	select {
	case <-cancelled:
	default:
		t.Fatal("shutdown did not cancel the active terminal round")
	}
	if err := manager.RequireNoLeaks("run-timeout"); err != nil {
		t.Fatalf("non-cooperative round left terminal leak: %v", err)
	}
	if err := manager.Write(owner, managerSessionID(t, manager, "run-timeout"), TerminalInput{OwnerTaskID: item.ID, Data: []byte("late write\n")}); err == nil {
		t.Fatal("late owner terminal write succeeded after timeout cleanup custody transfer")
	}
	if got := tracker.TodoList().Items()[0].Status; got != TaskBlocked {
		t.Fatalf("timeout owner status = %s, want %s", got, TaskBlocked)
	}
	// The simulated model never returns, so clean the test-only round record
	// after containment has asserted the timeout behavior.
	coord.unregisterTerminalRound(item.ID)
}

func managerSessionID(t *testing.T, manager *TerminalSessionManager, runID string) string {
	t.Helper()
	sessions, err := manager.List(context.Background(), runID)
	if err != nil || len(sessions) != 1 {
		t.Fatalf("list terminal sessions = %#v, err=%v", sessions, err)
	}
	return sessions[0].ID
}

// submittingWorkerAgent simulates a worker that calls submit_result itself,
// mirroring mockRepairAgent's onSubmit pattern (protocol_repair_test.go) but
// for the plain worker path instead of the protocol-repair path.
type submittingWorkerAgent struct {
	onSubmit func()
}

func (m *submittingWorkerAgent) result() *fantasy.AgentResult {
	if m.onSubmit != nil {
		m.onSubmit()
	}
	return &fantasy.AgentResult{Response: fantasy.Response{Content: fantasy.ResponseContent{
		fantasy.TextContent{Text: "submitted a result"},
	}}}
}

func (m *submittingWorkerAgent) Generate(context.Context, fantasy.AgentCall) (*fantasy.AgentResult, error) {
	return m.result(), nil
}

func (m *submittingWorkerAgent) Stream(context.Context, fantasy.AgentStreamCall) (*fantasy.AgentResult, error) {
	return m.result(), nil
}

// TestExecuteTaskFailsOnPartialSubmittedResultWithoutConsultingTerminalEvidence
// pins the claim-aware fix: a worker that submits status="partial" has
// already told the coordinator the task is not complete, so the attempt must
// fail on that honest report — not on an unrelated, superseded terminal
// session that happens to have a non-zero exit code. Before this fix,
// terminalTaskFailure ran unconditionally at round end and produced a
// misleading "terminal command ... exited with status N" error instead of
// surfacing what the worker actually said.
func TestExecuteTaskFailsOnPartialSubmittedResultWithoutConsultingTerminalEvidence(t *testing.T) {
	workspace := t.TempDir()
	// executeTask fires autoWriteSTMASync/persistReflexionLessonAsync as
	// fire-and-forget goroutines that write into workspace; give them a beat
	// to finish before TempDir cleanup removes it out from under them (see
	// the identical guard in TestProtocolRepair_ProgressNotFinalIsExecutionFailure).
	t.Cleanup(func() { time.Sleep(100 * time.Millisecond) })
	manager, err := NewTerminalSessionManager(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}

	c := &Coordinator{
		session: &TeamSession{
			Workspace: workspace,
			Config:    agent.TeamConfig{Name: "claim-gate", Timeout: 30, MaxRetries: 2},
			Agents: map[string]*agent.AgentDef{
				"worker": {Name: "worker", Role: "worker", MaxRetries: 2, Generation: agent.GenerationParams{Model: "test"}},
			},
		},
		sessionTime:        time.Now(),
		taskTracker:        NewTaskTracker(),
		reportStatus:       func(StatusEvent) {},
		taskResultCache:    make(map[string][]cachedTaskEntry),
		executionRunID:     "run-claim-gate",
		terminalSessionMgr: manager,
	}
	manager.SetActiveTaskRoundChecker(c.isTerminalRoundActive)
	item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "partial with stale terminal evidence"}})[0]

	// An unrelated, abandoned terminal session with a non-zero exit sits on
	// this task before the worker even runs, exactly as it would after an
	// exploratory probe the worker itself gave up on.
	owner := WithTerminalTaskID(context.Background(), item.ID)
	if _, err := manager.Start(owner, TerminalStartRequest{
		RunID: "run-claim-gate", OwnerTaskID: item.ID, Command: []string{"sh", "-c", "exit 9"},
	}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		sessions, err := manager.List(context.Background(), "run-claim-gate")
		if err != nil {
			t.Fatal(err)
		}
		if len(sessions) == 1 && sessions[0].ExitCode != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("terminal session never reported its exit code")
		}
		time.Sleep(10 * time.Millisecond)
	}

	c.workerAgentOverride = &submittingWorkerAgent{onSubmit: func() {
		c.storeSubmittedTaskResult(item.ID, &TaskResult{
			TaskID: item.ID, Agent: "worker", Status: "partial", Source: "submitted",
			Summary: "hosts.yml built by hand; wizard navigation still needs fixing",
		})
	}}

	_, err = c.executeTask(context.Background(), TaskDef{
		Agent: "worker", Goal: "partial with stale terminal evidence", Recovery: RecoveryRetry,
		SideEffect: SideEffectExternalWrite,
	}, item.ID)
	if err == nil {
		t.Fatal("expected the attempt to fail on the worker's own partial report")
	}
	if !strings.Contains(err.Error(), `worker reported task status "partial"`) {
		t.Fatalf("error = %q, want the worker's own reported status, not terminal evidence", err)
	}
	if strings.Contains(err.Error(), "terminal command") {
		t.Fatalf("error = %q, must not consult unrelated terminal evidence for a non-success claim", err)
	}
}

// TestExecuteTaskAcceptsCompletedWithGaps pins the distinction between an
// incomplete task and completed analysis that discovered an external gap. The
// latter must permit downstream work to use the evidence instead of being
// misclassified as a protocol or execution failure.
func TestExecuteTaskAcceptsCompletedWithGaps(t *testing.T) {
	workspace := t.TempDir()
	c := &Coordinator{
		session: &TeamSession{
			Workspace: workspace,
			Config:    agent.TeamConfig{Name: "completed-with-gaps", Timeout: 30, MaxRetries: 2},
			Agents: map[string]*agent.AgentDef{
				"analyst": {Name: "analyst", Role: "worker", MaxRetries: 2, Generation: agent.GenerationParams{Model: "test"}},
			},
		},
		sessionTime:     time.Now(),
		taskTracker:     NewTaskTracker(),
		reportStatus:    func(StatusEvent) {},
		taskResultCache: make(map[string][]cachedTaskEntry),
		executionRunID:  "run-completed-with-gaps",
	}
	item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "analyst", Desc: "survey target capability"}})[0]
	c.workerAgentOverride = &submittingWorkerAgent{onSubmit: func() {
		c.storeSubmittedTaskResult(item.ID, &TaskResult{
			TaskID: item.ID, Agent: "analyst", Status: TaskResultStatusCompletedWithGaps, Source: "submitted",
			Summary:  "Survey complete; target capability is unavailable.",
			Findings: []Finding{{Category: "capability_gap", Summary: "required action is interactive only"}},
		})
	}}

	if _, err := c.executeTask(context.Background(), TaskDef{Agent: "analyst", Goal: "survey target capability", Recovery: RecoveryRetry}, item.ID); err != nil {
		t.Fatalf("completed_with_gaps must complete the assigned task: %v", err)
	}
	updated := c.taskTracker.TodoList().Items()[0]
	if updated.Status != TaskDone {
		t.Fatalf("task status = %s, want %s", updated.Status, TaskDone)
	}
	if got := LoadSTM(workspace); !strings.Contains(got, "submitted a result") {
		t.Fatalf("completed task returned before its STM receipt was durable: %q", got)
	}
}
