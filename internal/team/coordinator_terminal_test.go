package team

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/sidecar"
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

func TestCancelStalledRoundCancelsOnlyCurrentRound(t *testing.T) {
	coord := &Coordinator{current: atomic.Pointer[currentSnapshot]{}}
	coord.current.Store(&currentSnapshot{TodoID: "stalled"})
	stalled := make(chan struct{})
	other := make(chan struct{})
	coord.registerTerminalRound("stalled", func() { close(stalled) })
	coord.registerTerminalRound("other", func() { close(other) })
	coord.cancelStalledRound()
	select {
	case <-stalled:
	case <-time.After(time.Second):
		t.Fatal("stalled round was not cancelled")
	}
	select {
	case <-other:
		t.Fatal("non-current round was cancelled")
	default:
	}
}

func TestInvocationWatchdogCancelsWholeInvocationWithStableCause(t *testing.T) {
	coord := &Coordinator{
		session:           &TeamSession{Workspace: t.TempDir(), Config: agent.TeamConfig{Name: "watchdog"}},
		stallThreshold:    10 * time.Millisecond,
		stallPollInterval: time.Millisecond,
		reportStatus:      func(StatusEvent) {},
	}
	ctx, end := coord.beginInvocationExecutionRun(context.Background())
	defer end()
	select {
	case <-ctx.Done():
		if !errors.Is(context.Cause(ctx), ErrInvocationStalled) {
			t.Fatalf("watchdog cause = %v, want %v", context.Cause(ctx), ErrInvocationStalled)
		}
	case <-time.After(time.Second):
		t.Fatal("invocation watchdog did not cancel stalled invocation")
	}
}

func TestAgentStreamUsesInvocationCauseOverConnectionReset(t *testing.T) {
	coord := &Coordinator{
		session:      &TeamSession{Workspace: t.TempDir(), Config: agent.TeamConfig{Name: "cause-authority"}},
		reportStatus: func(StatusEvent) {},
	}
	ctx, cancel := context.WithCancelCause(context.Background())
	cancel(context.Canceled)
	_, _, err := coord.runAgentWithStatusAndHistory(ctx, connectionResetAgent{}, "worker", "prompt", nil, &taskTiming{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("stream error = %v, want context.Canceled", err)
	}
	if strings.Contains(err.Error(), "connection reset") {
		t.Fatalf("transport error replaced authoritative cancellation: %v", err)
	}
}

func TestInvocationWatchdogStateIsIndependentPerRun(t *testing.T) {
	coord := &Coordinator{stallThreshold: time.Hour, stallPollInterval: time.Millisecond}
	first, endFirst := coord.beginInvocationExecutionRun(context.Background())
	firstWatchdog := coord.invocationWatchdog.Load()
	endFirst()
	second, endSecond := coord.beginInvocationExecutionRun(context.Background())
	defer endSecond()
	secondWatchdog := coord.invocationWatchdog.Load()
	if first == second || firstWatchdog == secondWatchdog || firstWatchdog == nil || secondWatchdog == nil {
		t.Fatal("invocation watchdog state was reused across runs")
	}
}

func TestInvocationWatchdogIsJoinedBeforeInvocationEndReturns(t *testing.T) {
	coord := &Coordinator{stallThreshold: time.Hour, stallPollInterval: time.Millisecond}
	_, end := coord.beginInvocationExecutionRun(context.Background())
	watchdog := coord.invocationWatchdog.Load()
	if watchdog == nil {
		t.Fatal("invocation watchdog was not installed")
	}
	end()
	select {
	case <-watchdog.done:
	default:
		t.Fatal("invocation returned before watchdog completion")
	}
	if coord.invocationWatchdog.Load() != nil {
		t.Fatal("watchdog pointer remained installed after invocation end")
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

func TestCoordinatorFinalizeTaskTerminalResourcesClosesLeakAfterAcceptedTerminalResult(t *testing.T) {
	manager, err := NewTerminalSessionManager(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	coord := &Coordinator{terminalSessionMgr: manager}
	manager.SetActiveTaskRoundChecker(coord.isTerminalRoundActive)
	owner := WithTerminalTaskID(context.Background(), "task-accepted-result-leak")
	if _, err := manager.Start(owner, TerminalStartRequest{
		RunID: "run-accepted-result-leak", OwnerTaskID: "task-accepted-result-leak", Command: []string{"sh", "-c", "sleep 30"},
	}); err != nil {
		t.Fatal(err)
	}

	got, blocked := coord.finalizeTaskTerminalResources(context.Background(), "task-accepted-result-leak", nil)
	if blocked {
		t.Fatalf("contained terminal unexpectedly requires manual intervention: %v", got)
	}
	if got == nil || !strings.Contains(got.Error(), "unclosed terminal session") {
		t.Fatalf("accepted result with live terminal = %v, want terminal boundary failure", got)
	}
	if err := manager.RequireTaskClosed("task-accepted-result-leak"); err != nil {
		t.Fatalf("terminal remained open after accepted-result finalization: %v", err)
	}
	sessions, err := manager.List(context.Background(), "run-accepted-result-leak")
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
	calls    *int
	text     string
}

type connectionResetAgent struct{}

func (connectionResetAgent) Generate(context.Context, fantasy.AgentCall) (*fantasy.AgentResult, error) {
	return nil, errors.New("connection reset by peer")
}

func (connectionResetAgent) Stream(context.Context, fantasy.AgentStreamCall) (*fantasy.AgentResult, error) {
	return nil, errors.New("connection reset by peer")
}

func (m *submittingWorkerAgent) result() *fantasy.AgentResult {
	if m.calls != nil {
		*m.calls++
	}
	if m.onSubmit != nil {
		m.onSubmit()
	}
	text := m.text
	if text == "" {
		text = "submitted a result"
	}
	return &fantasy.AgentResult{Response: fantasy.Response{Content: fantasy.ResponseContent{
		fantasy.TextContent{Text: text},
	}}}
}

func (m *submittingWorkerAgent) Generate(context.Context, fantasy.AgentCall) (*fantasy.AgentResult, error) {
	return m.result(), nil
}

func (m *submittingWorkerAgent) Stream(context.Context, fantasy.AgentStreamCall) (*fantasy.AgentResult, error) {
	return m.result(), nil
}

// TestExecuteTaskRetainsPartialSubmittedResultWithoutConsultingTerminalEvidence
// pins the semantic-status boundary: partial is a valid typed handoff but not
// terminal success. A non-replayable task must retain that handoff and stop
// through normal recovery, rather than letting unrelated terminal evidence
// replace the worker's honest report.
func TestExecuteTaskRetainsPartialSubmittedResultWithoutConsultingTerminalEvidence(t *testing.T) {
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
	if !strings.Contains(err.Error(), `worker reported incomplete task status "partial"`) {
		t.Fatalf("error = %q, want the worker's own reported status, not terminal evidence", err)
	}
	if strings.Contains(err.Error(), "terminal command") {
		t.Fatalf("error = %q, must not consult unrelated terminal evidence for a non-success claim", err)
	}
	updated := c.taskTracker.TodoList().Items()[0]
	if updated.Status == TaskDone || (updated.TypedResult != nil && (updated.TypedResult.Status == TaskResultStatusSuccess || updated.TypedResult.Status == TaskResultStatusCompletedWithGaps)) {
		t.Fatalf("partial handoff became terminal success: status=%s result=%#v", updated.Status, updated.TypedResult)
	}
	if updated.ExecutionReceipt == nil || updated.ExecutionReceipt.ExitCode == nil || *updated.ExecutionReceipt.ExitCode == 0 {
		t.Fatalf("partial task receipt = %#v, want non-zero runtime-owned exit code", updated.ExecutionReceipt)
	}
	if updated.ExecutionReceipt.SubmittedResult == nil || updated.ExecutionReceipt.SubmittedResult.Status != TaskResultStatusPartial {
		t.Fatalf("partial task receipt did not retain the typed handoff: %#v", updated.ExecutionReceipt)
	}
	if len(updated.ExecutionReceipts) != 1 {
		t.Fatalf("non-replayable partial task ran %d attempts, want 1", len(updated.ExecutionReceipts))
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
	if got := LoadSTM(workspace); !strings.Contains(got, "Survey complete; target capability is unavailable.") || strings.Contains(got, "submitted a result") {
		t.Fatalf("completed task did not durably expose its typed result: %q", got)
	}
}

func TestExecuteTaskUsesSubmittedResultWithEmptyDetailsForCompletionAndProjections(t *testing.T) {
	workspace := t.TempDir()
	t.Cleanup(func() { time.Sleep(100 * time.Millisecond) })
	c := &Coordinator{
		session: &TeamSession{
			Workspace: workspace,
			Config:    agent.TeamConfig{Name: "typed-empty-details", Timeout: 30, MaxRetries: 0},
			Agents: map[string]*agent.AgentDef{
				"worker": {Name: "worker", Role: "worker", Generation: agent.GenerationParams{Model: "test"}},
			},
		},
		projectDir:      workspace,
		sessionTime:     time.Now(),
		taskTracker:     NewTaskTracker(),
		reportStatus:    func(StatusEvent) {},
		taskResultCache: make(map[string][]cachedTaskEntry),
		executionRunID:  "run-typed-empty-details",
	}
	item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "complete typed handoff"}})[0]
	const fallback = "Now let me continue with the remaining work."
	c.workerAgentOverride = &submittingWorkerAgent{text: fallback, onSubmit: func() {
		c.storeSubmittedTaskResult(item.ID, &TaskResult{
			TaskID: item.ID, Agent: "worker", Status: TaskResultStatusSuccess, Source: "submitted",
			Summary:   "typed summary survives empty details",
			FilesRead: []FileRef{{Path: "assigned.md"}},
		})
	}}

	output, err := c.executeTask(context.Background(), TaskDef{
		Agent: "worker", Goal: "complete typed handoff",
		Execution: ExecutionContract{RequiresResult: true},
	}, item.ID)
	if err != nil {
		t.Fatalf("typed result should complete despite fallback prose: %v", err)
	}
	if !strings.Contains(output, "typed summary survives empty details") || strings.Contains(output, fallback) {
		t.Fatalf("canonical output = %q, want typed summary without fallback", output)
	}
	if item.Status != TaskDone || item.Output != output {
		t.Fatalf("todo projection = status %s output %q, want done and canonical output %q", item.Status, item.Output, output)
	}
	summary := c.summaryFromTodos(errors.New("later coordinator failure"))
	if !strings.Contains(summary, "typed summary survives empty details") || strings.Contains(summary, fallback) {
		t.Fatalf("deterministic summary = %q, want typed summary without fallback", summary)
	}
	taskFiles, err := filepath.Glob(filepath.Join(workspace, tasksDir, "typed-empty-details", "worker", "*.md"))
	if err != nil || len(taskFiles) != 1 {
		t.Fatalf("done task files = %v, err=%v; want one file", taskFiles, err)
	}
	taskFile, err := os.ReadFile(taskFiles[0])
	if err != nil {
		t.Fatalf("read done task file: %v", err)
	}
	if !strings.Contains(string(taskFile), "typed summary survives empty details") || strings.Contains(string(taskFile), fallback) {
		t.Fatalf("done task file = %q, want typed summary without fallback", taskFile)
	}
}

func TestExecuteTaskAdversarialVerifyUsesCanonicalSubmittedResult(t *testing.T) {
	workspace := t.TempDir()
	const modelID = "skeptic-canonical-result-test"
	GlobalModelSpecRegistry().RegisterSpec(ModelContextSpec{
		ModelID: modelID, ContextWindow: 8192, MaxOutputTokens: 128, SafetyMarginTokens: 32,
	})

	var skepticRequest string
	server := newIPv4TestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, fmt.Sprintf("read request: %v", err), http.StatusBadRequest)
			return
		}
		skepticRequest = string(body)
		w.Header().Set("Content-Type", "text/event-stream")
		vote := `{"refuted":false,"reason":"canonical typed result accepted"}`
		fmt.Fprintf(w, "data: {\"id\":\"skeptic\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":%q,\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":%q},\"finish_reason\":null}]}\n\n", modelID, vote)
		fmt.Fprint(w, "data: {\"id\":\"skeptic\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"skeptic\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer server.Close()
	provider, err := agent.NewOpenAICompatibleProvider(server.URL, "", "local")
	if err != nil {
		t.Fatal(err)
	}
	skeptic, err := sidecar.NewSidecar(t.Context(), provider, modelID)
	if err != nil {
		t.Fatalf("create skeptic sidecar: %v", err)
	}

	c := &Coordinator{
		session: &TeamSession{
			Workspace: workspace,
			Config:    agent.TeamConfig{Name: "skeptic-canonical", Timeout: 30, MaxRetries: 0},
			Agents: map[string]*agent.AgentDef{
				"worker": {Name: "worker", Role: "worker", Generation: agent.GenerationParams{Model: "test"}},
			},
		},
		projectDir:      workspace,
		sessionTime:     time.Now(),
		taskTracker:     NewTaskTracker(),
		reportStatus:    func(StatusEvent) {},
		taskResultCache: make(map[string][]cachedTaskEntry),
		executionRunID:  "run-skeptic-canonical",
		sidecarModel:    modelID,
		sidecarInst:     skeptic,
		sidecarInit:     true,
	}
	item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "verify typed handoff"}})[0]
	const fallback = "Now let me continue with the remaining work."
	const typedSummary = "typed summary is the authoritative skeptic claim"
	c.workerAgentOverride = &submittingWorkerAgent{text: fallback, onSubmit: func() {
		c.storeSubmittedTaskResult(item.ID, &TaskResult{
			TaskID: item.ID, Agent: "worker", Status: TaskResultStatusSuccess, Source: "submitted",
			Summary: typedSummary, FilesRead: []FileRef{{Path: "assigned.md"}},
		})
	}}

	output, err := c.executeTask(context.Background(), TaskDef{
		Agent: "worker", Goal: "verify typed handoff", AdversarialVerify: 1,
		Execution: ExecutionContract{RequiresResult: true},
	}, item.ID)
	if err != nil {
		t.Fatalf("typed result should pass adversarial verification: %v", err)
	}
	if !strings.Contains(output, typedSummary) || strings.Contains(output, fallback) {
		t.Fatalf("canonical task output = %q, want typed summary without fallback", output)
	}
	if !strings.Contains(skepticRequest, typedSummary) || strings.Contains(skepticRequest, fallback) {
		t.Fatalf("skeptic request = %q, want canonical typed result without fallback", skepticRequest)
	}
}

func TestExecuteTaskVerbatimFinalizationUpdatesCanonicalResult(t *testing.T) {
	workspace := t.TempDir()
	t.Cleanup(func() { time.Sleep(100 * time.Millisecond) })
	c := &Coordinator{
		session: &TeamSession{
			Workspace: workspace,
			Config:    agent.TeamConfig{Name: "verbatim-finalization", Timeout: 30, MaxRetries: 0},
			Agents: map[string]*agent.AgentDef{
				"worker": {Name: "worker", Role: "worker", Generation: agent.GenerationParams{Model: "test"}},
			},
		},
		sessionTime:     time.Now(),
		taskTracker:     NewTaskTracker(),
		reportStatus:    func(StatusEvent) {},
		taskResultCache: make(map[string][]cachedTaskEntry),
		executionRunID:  "run-verbatim-finalization",
	}
	item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "capture complete transcript"}})[0]
	c.workerAgentOverride = &submittingWorkerAgent{onSubmit: func() {
		c.storeSubmittedTaskResult(item.ID, &TaskResult{
			TaskID: item.ID, Agent: "worker", Status: TaskResultStatusSuccess, Source: "submitted",
			Summary: "transcript captured",
		})
	}}
	task := TaskDef{
		Agent: "worker", Goal: "capture complete transcript", OutputMode: TaskOutputModeVerbatim,
		Execution: ExecutionContract{RequiresResult: true},
		VerifySpec: &VerificationSpec{Type: VerifyTaskResultAssert, TaskResultAssertions: []TaskResultAssertion{
			{Pointer: "/raw_output_ref/id", Op: "exists"},
			{Pointer: "/outputs/raw_transcript/artifact/id", Op: "exists"},
			{Pointer: "/evidence", Op: "min_items", Value: 1},
		}},
	}

	if _, err := c.executeTask(context.Background(), task, item.ID); err != nil {
		t.Fatalf("verbatim task execution: %v", err)
	}
	if item.Status != TaskDone {
		t.Fatalf("task status = %s, want %s", item.Status, TaskDone)
	}
	canonical := c.GetTaskResult(item.ID)
	hasTranscriptEvidence := func(result *TaskResult) bool {
		if result == nil || result.RawOutputRef == nil {
			return false
		}
		for _, evidence := range result.Evidence {
			if evidence.Type == "task_transcript" && evidence.Value == result.RawOutputRef.ID {
				return true
			}
		}
		return false
	}
	if canonical == nil || canonical.RawOutputRef == nil || canonical.RawOutputRef.ID == "" || canonical.Outputs[rawTranscriptOutputName].Artifact == nil || !hasTranscriptEvidence(canonical) {
		t.Fatalf("canonical verbatim result = %#v, want sealed transcript fields", canonical)
	}
	if item.TypedResult == nil || item.TypedResult.RawOutputRef == nil || item.TypedResult.RawOutputRef.ID == "" || item.TypedResult.Outputs[rawTranscriptOutputName].Artifact == nil || !hasTranscriptEvidence(item.TypedResult) {
		t.Fatalf("todo typed result = %#v, want sealed transcript fields", item.TypedResult)
	}
}

// TestCharacterizationChildVerificationFailurePreservesTheSubmittedResult
// uses the in-process worker seam rather than a live model. It fixes the
// current ordering at the protocol boundary: a child can submit a valid
// success result, yet its objective verifier still prevents task completion.
func TestCharacterizationChildVerificationFailurePreservesTheSubmittedResult(t *testing.T) {
	workspace := t.TempDir()
	c := &Coordinator{
		session: &TeamSession{
			Workspace: workspace,
			Config:    agent.TeamConfig{Timeout: 30, MaxRetries: 1},
			Agents: map[string]*agent.AgentDef{
				"worker": {Name: "worker", Role: "worker", MaxRetries: 1, Generation: agent.GenerationParams{Model: "test"}},
			},
		},
		projectDir:      workspace,
		sessionTime:     time.Now(),
		taskTracker:     NewTaskTracker(),
		reportStatus:    func(StatusEvent) {},
		taskResultCache: make(map[string][]cachedTaskEntry),
		executionRunID:  "run-characterization",
	}
	item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "process gamma"}})[0]
	var calls int
	c.workerAgentOverride = &submittingWorkerAgent{calls: &calls, onSubmit: func() {
		c.storeSubmittedTaskResult(item.ID, &TaskResult{
			TaskID: item.ID, Agent: "worker", Status: TaskResultStatusSuccess,
			Summary: "processed gamma", Source: "submitted",
		})
	}}

	_, err := c.executeTask(context.Background(), TaskDef{
		Agent: "worker", Goal: "process gamma", Recovery: RecoveryRetry,
		VerifySpec: &VerificationSpec{Type: VerifyFileExists, Path: "outputs/gamma.txt"},
	}, item.ID)
	if err == nil {
		t.Fatal("missing child artifact unexpectedly passed verification")
	}
	if calls != 2 {
		t.Fatalf("worker calls = %d, want initial attempt plus MaxRetries retry", calls)
	}
	updated := c.taskTracker.TodoList().Items()[0]
	if updated.Status == TaskDone {
		t.Fatalf("failed verification marked child done: %#v", updated)
	}
	if updated.TypedResult == nil || updated.TypedResult.Status != TaskResultStatusSuccess {
		t.Fatalf("submitted result was not retained for retry/diagnosis: %#v", updated.TypedResult)
	}
	if updated.VerifyResult == nil || updated.VerifyResult.ExitCode == 0 {
		t.Fatalf("failed verification evidence missing: %#v", updated.VerifyResult)
	}
	if len(updated.ExecutionReceipts) != calls {
		t.Fatalf("execution receipts = %d, want one per attempt (%d)", len(updated.ExecutionReceipts), calls)
	}
	verifiedReceipts := 0
	for _, receipt := range updated.ExecutionReceipts {
		if receipt.TaskID != item.ID {
			t.Fatalf("retry changed receipt task ID: %#v", receipt)
		}
		if receipt.VerifyResult != nil {
			if receipt.VerifyResult.ExitCode == 0 {
				t.Fatalf("receipt contains successful verification evidence for a failed task: %#v", receipt)
			}
			verifiedReceipts++
		}
	}
	if verifiedReceipts == 0 {
		t.Fatalf("failed verification evidence missing from all attempt receipts: %#v", updated.ExecutionReceipts)
	}
}

// Budget rejection is intentionally checked before Todo creation. The items
// are generic so this remains a runtime characterization, not a consumer
// fixture.
func TestCharacterizationBudgetCancellationCreatesNoGenericChildren(t *testing.T) {
	workspace := t.TempDir()
	writeCharacterizationWorkset(t, workspace, characterizationWorksetItems)
	var budgetEvents []StatusEvent
	c := &Coordinator{
		session: &TeamSession{
			Workspace: workspace,
			Config:    agent.TeamConfig{},
			Agents: map[string]*agent.AgentDef{
				"worker": {Name: "worker", Role: "worker", Generation: agent.GenerationParams{Model: "test"}},
			},
		},
		projectDir:  workspace,
		sessionTime: time.Now().Add(-2 * time.Second),
		taskTracker: NewTaskTracker(),
		reportStatus: func(event StatusEvent) {
			budgetEvents = append(budgetEvents, event)
		},
	}
	var calls int
	c.workerAgentOverride = &submittingWorkerAgent{calls: &calls}
	c.SetBudget(1, 0)

	_, err := c.ExecuteTasks(context.Background(), []TaskDef{{
		Agent:  "worker",
		FanOut: &FanOutSpec{Source: "inputs/items.tsv", GoalTemplate: "process {item}"},
	}})
	if err == nil || !strings.Contains(err.Error(), "budget exceeded") {
		t.Fatalf("budget cancellation error = %v", err)
	}
	if got := len(c.taskTracker.TodoList().Items()); got != 0 {
		t.Fatalf("budget-cancelled dispatch created %d children, want none", got)
	}
	if calls != 0 {
		t.Fatalf("budget-cancelled dispatch invoked the worker %d times, want zero", calls)
	}
	for _, event := range budgetEvents {
		if event.Type == "budget_exceeded" {
			return
		}
	}
	t.Fatalf("budget cancellation emitted no budget_exceeded event: %#v", budgetEvents)
}

// Crash-resume keeps the original Todo identity and re-drives the unfinished
// generic item. WP-0 records this baseline before workset bindings introduce
// an additional parent/item identity to preserve.
func TestCharacterizationResumeReusesGenericChildIdentity(t *testing.T) {
	workspace := t.TempDir()
	first := &Coordinator{
		session: &TeamSession{
			Workspace: workspace,
			Config:    agent.TeamConfig{Timeout: 30, MaxRetries: 1},
			Agents: map[string]*agent.AgentDef{
				"worker": {Name: "worker", Role: "worker", MaxRetries: 1, Generation: agent.GenerationParams{Model: "test"}},
			},
		},
		projectDir:      workspace,
		sessionTime:     time.Now(),
		taskTracker:     NewTaskTracker(),
		reportStatus:    func(StatusEvent) {},
		taskResultCache: make(map[string][]cachedTaskEntry),
		executionRunID:  "run-resume-before-checkpoint",
	}
	items := first.taskTracker.TodoList().AddBatch([]TodoSpec{
		{Agent: "worker", Desc: "process alpha", Recovery: RecoveryRetry},
		{Agent: "worker", Desc: "process beta", Recovery: RecoveryRetry},
	})
	first.taskTracker.TodoList().UpdateStatus(items[0].ID, TaskDone, "completed before interruption")
	first.taskTracker.TodoList().UpdateStatus(items[1].ID, TaskInProgress, "interrupted")
	if err := SaveSession(workspace, &SessionData{Tasks: first.taskTracker.TodoList().Items()}); err != nil {
		t.Fatalf("save checkpoint: %v", err)
	}

	second := &Coordinator{
		session:         first.session,
		projectDir:      workspace,
		sessionTime:     time.Now(),
		taskTracker:     NewTaskTracker(),
		reportStatus:    func(StatusEvent) {},
		taskResultCache: make(map[string][]cachedTaskEntry),
		executionRunID:  "run-resume-after-checkpoint",
	}
	second.SetSessionData(LoadSession(workspace))
	var calls int
	second.workerAgentOverride = &submittingWorkerAgent{calls: &calls, onSubmit: func() {
		second.storeSubmittedTaskResult(items[1].ID, &TaskResult{
			TaskID: items[1].ID, Agent: "worker", Status: TaskResultStatusSuccess,
			Summary: "processed beta", Source: "submitted",
		})
	}}

	resumed, err := second.ResumeInterruptedTasks(context.Background())
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if resumed != 1 {
		t.Fatalf("resumed = %d, want 1", resumed)
	}
	if calls != 1 {
		t.Fatalf("durable resume replayed %d workers, want only the interrupted child", calls)
	}
	updated := second.taskTracker.TodoList().Items()
	if len(updated) != 2 || updated[0].ID != items[0].ID || updated[0].Status != TaskDone || updated[1].ID != items[1].ID || updated[1].Status != TaskDone {
		t.Fatalf("resume did not preserve sibling state and original child identity: %#v", updated)
	}
}
