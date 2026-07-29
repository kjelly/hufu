package team

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/anomalyco/hufu/internal/agent"
	"github.com/anomalyco/hufu/internal/tools"
)

func TestTerminalSessionManager_OwnerLifecycleAndOutputArtifact(t *testing.T) {
	workspace := t.TempDir()
	var events []string
	var eventsMu sync.Mutex
	manager, err := NewTerminalSessionManager(workspace, func(eventType, _ string, _ map[string]interface{}) {
		eventsMu.Lock()
		defer eventsMu.Unlock()
		events = append(events, eventType)
	})
	if err != nil {
		t.Fatal(err)
	}
	ownerCtx := WithTerminalTaskID(context.Background(), "task-a")
	session, err := manager.Start(ownerCtx, TerminalStartRequest{
		RunID: "run-a", OwnerTaskID: "task-a", Agent: "worker",
		Command: []string{"sh", "-c", "printf ready; read line; printf :$line"},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if session.ID == "" || session.ID == "task-a" {
		t.Fatalf("unexpected generated session ID %q", session.ID)
	}
	if err := manager.Write(WithTerminalTaskID(context.Background(), "task-b"), session.ID, TerminalInput{Data: []byte("nope\n")}); err == nil || !strings.Contains(err.Error(), "belongs to task") {
		t.Fatalf("non-owner Write error = %v, want ownership rejection", err)
	}
	if err := manager.Write(ownerCtx, session.ID, TerminalInput{Data: []byte("done\n")}); err != nil {
		t.Fatalf("owner Write: %v", err)
	}

	completed := waitForTerminal(t, manager, session.ID, time.Second)
	if completed.State != TerminalSessionExited || completed.Running || completed.ExitCode == nil || *completed.ExitCode != 0 {
		t.Fatalf("terminal completion = %+v, want exited with code 0", completed)
	}
	read, err := manager.Read(ownerCtx, session.ID)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got := string(read.Output); !strings.Contains(got, "ready:done") {
		t.Fatalf("terminal output = %q, want complete output", got)
	}
	if len(completed.OutputRefs) != 1 {
		t.Fatalf("output refs = %+v, want one artifact", completed.OutputRefs)
	}
	artifact, err := os.ReadFile(filepath.Join(workspace, completed.OutputRefs[0].Path))
	if err != nil {
		t.Fatalf("read output artifact: %v", err)
	}
	if got := string(artifact); !strings.Contains(got, "ready:done") {
		t.Fatalf("artifact = %q", got)
	}
	if err := manager.RequireTaskClosed("task-a"); err != nil {
		t.Fatalf("exited session should close task gate: %v", err)
	}
	eventsMu.Lock()
	defer eventsMu.Unlock()
	if !containsTerminalEvent(events, "terminal_session_started") || !containsTerminalEvent(events, "terminal_session_exited") {
		t.Fatalf("lifecycle events = %v", events)
	}
}

func TestTerminalSessionLifecycleFactsAndExplicitWaitTargets(t *testing.T) {
	workspace := t.TempDir()
	var events []string
	var mu sync.Mutex
	manager, err := NewTerminalSessionManager(workspace, func(eventType, _ string, _ map[string]interface{}) {
		mu.Lock()
		defer mu.Unlock()
		events = append(events, eventType)
	})
	if err != nil {
		t.Fatal(err)
	}
	waiter := NewTerminalSessionWaiter(manager)

	ctx := WithTerminalTaskID(context.Background(), "task-lifecycle")
	session, err := manager.Start(ctx, TerminalStartRequest{
		RunID: "run-lifecycle", OwnerTaskID: "task-lifecycle",
		Command: []string{"sh", "-c", "printf observed; sleep 0.08"},
	})
	if err != nil {
		t.Fatal(err)
	}
	// The terminal output artifact exists immediately, but it must not satisfy
	// an exit wait for this process invocation.
	shortCtx, cancel := context.WithTimeout(context.Background(), 15*time.Millisecond)
	defer cancel()
	if _, err := waiter.Wait(shortCtx, TerminalWaitRequest{SessionID: session.ID, Target: TerminalWaitExit}); err == nil {
		t.Fatal("existing output artifact must not satisfy an exit wait")
	}
	if _, err := manager.Read(ctx, session.ID); err != nil {
		t.Fatalf("Read: %v", err)
	}
	if _, err := waiter.Wait(context.Background(), TerminalWaitRequest{SessionID: session.ID, Target: TerminalWaitExit}); err != nil {
		t.Fatalf("Wait(exit): %v", err)
	}
	if _, err := waiter.Wait(context.Background(), TerminalWaitRequest{SessionID: session.ID, Target: TerminalWaitResourceReleased}); err != nil {
		t.Fatalf("Wait(resource_released): %v", err)
	}
	if _, err := waiter.Wait(context.Background(), TerminalWaitRequest{SessionID: session.ID, Target: TerminalWaitArtifactVerified}); err == nil || !strings.Contains(err.Error(), "ArtifactVerifier") {
		t.Fatalf("Wait(artifact_verified) error = %v, want verifier-bound rejection", err)
	}
	if _, err := manager.Reconcile(context.Background(), session.ID); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	sessions, err := manager.List(context.Background(), "run-lifecycle")
	if err != nil || len(sessions) != 1 {
		t.Fatalf("List = %+v, %v", sessions, err)
	}
	got := sessions[0]
	if got.ObservedAt.IsZero() || got.ExitedAt.IsZero() || got.ReleasedAt.IsZero() || got.ReconciledAt.IsZero() {
		t.Fatalf("lifecycle timestamps were not persisted: %+v", got)
	}
	mu.Lock()
	defer mu.Unlock()
	want := []string{string(TerminalProcessStarted), string(TerminalProcessObserved), string(TerminalProcessExited), string(TerminalProcessReconciled)}
	last := -1
	for _, event := range want {
		index := -1
		for i, gotEvent := range events {
			if gotEvent == event {
				index = i
				break
			}
		}
		if index <= last {
			t.Fatalf("lifecycle event %q missing or out of order in %v", event, events)
		}
		last = index
	}
}

func TestTerminalSessionManager_EmptyReadDoesNotObserveOutput(t *testing.T) {
	manager, err := NewTerminalSessionManager(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx := WithTerminalTaskID(context.Background(), "task-empty-read")
	session, err := manager.Start(ctx, TerminalStartRequest{
		RunID: "run-empty-read", OwnerTaskID: "task-empty-read",
		Command: []string{"sh", "-c", "sleep 0.06; printf later"},
	})
	if err != nil {
		t.Fatal(err)
	}
	first, err := manager.Read(ctx, session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Output) != 0 || !first.Session.ObservedAt.IsZero() {
		t.Fatalf("empty read = %+v; it must not claim process output was observed", first)
	}
	completed := waitForTerminal(t, manager, session.ID, time.Second)
	second, err := manager.Read(ctx, session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if string(second.Output) != "later" || second.Session.ObservedAt.IsZero() || completed.ExitedAt.IsZero() {
		t.Fatalf("non-empty read = %+v; it must establish output observation", second)
	}
}

type fakeTerminalSessionSource struct {
	mu       sync.Mutex
	sessions []TerminalSession
	calls    int
	onList   func(*fakeTerminalSessionSource)
}

func (f *fakeTerminalSessionSource) List(_ context.Context, _ string) ([]TerminalSession, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.onList != nil {
		f.onList(f)
	}
	result := make([]TerminalSession, len(f.sessions))
	copy(result, f.sessions)
	return result, nil
}

func TestTerminalSessionWaiterUsesLifecycleFactsNotOutputExistence(t *testing.T) {
	now := time.Now().UTC()
	source := &fakeTerminalSessionSource{sessions: []TerminalSession{{
		ID:         "session-1",
		State:      TerminalSessionRunning,
		Running:    true,
		OutputRefs: []ArtifactRef{{Path: "previous-output.log", Type: "terminal_output"}},
	}}}
	source.onList = func(f *fakeTerminalSessionSource) {
		if f.calls == 2 {
			f.sessions[0].Running = false
			f.sessions[0].State = TerminalSessionExited
			f.sessions[0].ExitedAt = now
			f.sessions[0].ReleasedAt = now
		}
	}
	waiter := NewTerminalSessionWaiter(source)

	result, err := waiter.Wait(context.Background(), TerminalWaitRequest{
		SessionID: "session-1", Target: TerminalWaitExit, PollInterval: time.Millisecond,
	})
	if err != nil {
		t.Fatalf("Wait(exit): %v", err)
	}
	if result.Session.ExitedAt.IsZero() || source.calls < 2 {
		t.Fatalf("wait result = %+v after %d source reads; output existence must not satisfy exit", result, source.calls)
	}
	if _, err := waiter.Wait(context.Background(), TerminalWaitRequest{SessionID: "session-1", Target: TerminalWaitArtifactVerified}); err == nil {
		t.Fatal("terminal waiter must delegate artifact verification to ArtifactVerifier")
	}

	unknown := NewTerminalSessionWaiter(&fakeTerminalSessionSource{sessions: []TerminalSession{{ID: "unknown", State: TerminalSessionUnknown}}})
	if _, err := unknown.Wait(context.Background(), TerminalWaitRequest{SessionID: "unknown", Target: TerminalWaitExit}); err == nil || !strings.Contains(err.Error(), "reconcile") {
		t.Fatalf("unknown session wait error = %v, want reconciliation requirement", err)
	}
}

func TestTerminalExitWaitDoesNotMarkTaskDoneBeforeVerification(t *testing.T) {
	manager, err := NewTerminalSessionManager(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	tracker := NewTaskTracker()
	item := tracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "requires verification"}})[0]
	tracker.TodoList().UpdateStatus(item.ID, TaskInProgress, "")
	ctx := WithTerminalTaskID(context.Background(), item.ID)
	session, err := manager.Start(ctx, TerminalStartRequest{
		RunID: "run-unverified", OwnerTaskID: item.ID, Command: []string{"sh", "-c", "true"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewTerminalSessionWaiter(manager).Wait(context.Background(), TerminalWaitRequest{SessionID: session.ID, Target: TerminalWaitExit}); err != nil {
		t.Fatal(err)
	}
	var current *TodoItem
	for _, candidate := range tracker.TodoList().Items() {
		if candidate.ID == item.ID {
			current = candidate
			break
		}
	}
	if current == nil || current.Status != TaskInProgress {
		t.Fatalf("terminal exit wait must not decide task completion; task = %+v", current)
	}
}

func TestTerminalSessionManager_ResumeMarksRunningSessionUnknownAndBlocksGates(t *testing.T) {
	workspace := t.TempDir()
	manager, err := NewTerminalSessionManager(workspace, nil)
	if err != nil {
		t.Fatal(err)
	}
	ownerCtx := WithTerminalTaskID(context.Background(), "task-a")
	session, err := manager.Start(ownerCtx, TerminalStartRequest{
		RunID: "run-a", OwnerTaskID: "task-a", Command: []string{"sh", "-c", "sleep 5"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = manager.Close(ownerCtx, session.ID) }()

	restored, err := NewTerminalSessionManager(workspace, nil)
	if err != nil {
		t.Fatal(err)
	}
	got, err := restored.Reconcile(context.Background(), session.ID)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if got.State != TerminalSessionUnknown || got.Running {
		t.Fatalf("restored state = %+v, want unknown/non-running", got)
	}
	if err := restored.RequireTaskClosed("task-a"); err == nil {
		t.Fatal("unknown session must block task retry/completion")
	}
	if err := restored.RequireNoLeaks("run-a"); err == nil {
		t.Fatal("unknown session must block final acceptance")
	}
}

func TestTerminalSessionManager_ReconcileRestoredDeadProcessRecordsExitBeforeReconciliation(t *testing.T) {
	workspace := t.TempDir()
	logsPath := filepath.Join(workspace, logsDir)
	if err := os.MkdirAll(logsPath, 0o755); err != nil {
		t.Fatal(err)
	}
	persisted := []TerminalSession{{
		ID: "restored-dead", RunID: "run-restored", OwnerTaskID: "task-restored",
		StartedAt: time.Now().Add(-time.Minute), Running: true, State: TerminalSessionRunning,
		PID: 99999999, ProcessIdentity: &ProcessIdentity{PID: 99999999},
	}}
	data, err := json.Marshal(persisted)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(logsPath, terminalSessionsFile), data, 0o644); err != nil {
		t.Fatal(err)
	}

	var events []string
	manager, err := NewTerminalSessionManager(workspace, func(eventType, _ string, _ map[string]interface{}) {
		events = append(events, eventType)
	})
	if err != nil {
		t.Fatal(err)
	}
	before, err := manager.List(context.Background(), "run-restored")
	if err != nil || len(before) != 1 || before[0].State != TerminalSessionUnknown {
		t.Fatalf("restored session = %+v, %v; want unknown", before, err)
	}
	if containsTerminalEvent(events, string(TerminalProcessReconciled)) {
		t.Fatalf("restart alone must not claim reconciliation: %v", events)
	}

	got, err := manager.Reconcile(context.Background(), "restored-dead")
	if err != nil {
		t.Fatal(err)
	}
	if got.State != TerminalSessionExited || got.ExitedAt.IsZero() || got.ReleasedAt.IsZero() || got.ReconciledAt.IsZero() {
		t.Fatalf("reconciled session = %+v, want exited lifecycle timestamps", got)
	}
	assertTerminalEventOrder(t, events, []string{
		"terminal_session_unknown",
		string(TerminalProcessExited),
		string(TerminalResourceReleased),
		string(TerminalProcessReconciled),
	})
}

func TestTerminalSessionManager_CloseRestoredSessionEstablishesExitFact(t *testing.T) {
	for _, tc := range []struct {
		name string
		live bool
	}{
		{name: "already dead"},
		{name: "verified live process is terminated", live: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			workspace := t.TempDir()
			pid := 99999999
			identity := &ProcessIdentity{PID: pid}
			var cmd *exec.Cmd
			if tc.live {
				cmd = exec.Command("sleep", "5")
				if err := cmd.Start(); err != nil {
					t.Fatal(err)
				}
				reaped := make(chan struct{})
				go func() {
					_ = cmd.Wait()
					close(reaped)
				}()
				defer func() {
					if cmd.Process != nil {
						_ = cmd.Process.Kill()
					}
					select {
					case <-reaped:
					case <-time.After(time.Second):
						t.Error("test child was not reaped")
					}
				}()
				pid = cmd.Process.Pid
				var err error
				identity, err = getProcessIdentity(pid)
				if err != nil || identity == nil {
					t.Fatalf("getProcessIdentity(%d) = %v, %v", pid, identity, err)
				}
			}
			logsPath := filepath.Join(workspace, logsDir)
			if err := os.MkdirAll(logsPath, 0o755); err != nil {
				t.Fatal(err)
			}
			data, err := json.Marshal([]TerminalSession{{
				ID: "restored-close", RunID: "run-restored-close", OwnerTaskID: "task-restored-close",
				StartedAt: time.Now().Add(-time.Minute), Running: true, State: TerminalSessionRunning,
				PID: pid, ProcessIdentity: identity,
			}})
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(logsPath, terminalSessionsFile), data, 0o644); err != nil {
				t.Fatal(err)
			}
			var events []string
			manager, err := NewTerminalSessionManager(workspace, func(eventType, _ string, _ map[string]interface{}) {
				events = append(events, eventType)
			})
			if err != nil {
				t.Fatal(err)
			}
			ctx := WithTerminalTaskID(context.Background(), "task-restored-close")
			if err := manager.Close(ctx, "restored-close"); err != nil {
				t.Fatal(err)
			}
			waitCtx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			result, err := NewTerminalSessionWaiter(manager).Wait(waitCtx, TerminalWaitRequest{SessionID: "restored-close", Target: TerminalWaitExit})
			if err != nil || result.Session.ExitedAt.IsZero() {
				t.Fatalf("exit wait after restored close = %+v, %v", result, err)
			}
			assertTerminalEventOrder(t, events, []string{string(TerminalProcessExited), string(TerminalResourceReleased)})
		})
	}
}

func assertTerminalEventOrder(t *testing.T, events, want []string) {
	t.Helper()
	last := -1
	for _, event := range want {
		index := -1
		for i, got := range events {
			if got == event {
				index = i
				break
			}
		}
		if index <= last {
			t.Fatalf("lifecycle event %q missing or out of order in %v", event, events)
		}
		last = index
	}
}

func TestTerminalSessionManager_ChildTimeoutIsIndependent(t *testing.T) {
	manager, err := NewTerminalSessionManager(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx := WithTerminalTaskID(context.Background(), "task-timeout")
	session, err := manager.Start(ctx, TerminalStartRequest{
		RunID: "run-timeout", OwnerTaskID: "task-timeout", Command: []string{"sh", "-c", "sleep 1"}, ChildTimeout: 25 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	completed := waitForTerminal(t, manager, session.ID, time.Second)
	if completed.State != TerminalSessionExited || completed.ExitCode == nil || *completed.ExitCode == 0 {
		t.Fatalf("child timeout completion = %+v, want non-zero exited child", completed)
	}
}

func TestTerminalFinishGateRejectsLeakedSession(t *testing.T) {
	manager, err := NewTerminalSessionManager(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx := WithTerminalTaskID(context.Background(), "task-a")
	session, err := manager.Start(ctx, TerminalStartRequest{RunID: "run-prior", OwnerTaskID: "task-a", Command: []string{"sh", "-c", "sleep 5"}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = manager.Close(ctx, session.ID) }()

	tool := &finishTool{coordinator: &Coordinator{taskTracker: NewTaskTracker(), terminalSessionMgr: manager, executionRunID: "run-current"}}
	response, err := tool.Run(context.Background(), fantasy.ToolCall{Input: `{"response":"done"}`})
	if err != nil {
		t.Fatalf("finish tool error: %v", err)
	}
	if !strings.Contains(response.Content, "terminal sessions remain unresolved") {
		t.Fatalf("finish response = %q, want leaked-session rejection", response.Content)
	}
}

func TestTerminalSessionManagerWritesLifecycleEventsToEventStore(t *testing.T) {
	workspace := t.TempDir()
	store, err := NewEventStore(workspace, "run-events", "session-events")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	coordinator := &Coordinator{eventStore: store}
	manager, err := NewTerminalSessionManager(workspace, func(eventType, taskID string, payload map[string]interface{}) {
		coordinator.emitEvent(eventType, "terminal", taskID, payload)
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := WithTerminalTaskID(context.Background(), "task-events")
	session, err := manager.Start(ctx, TerminalStartRequest{RunID: "run-events", OwnerTaskID: "task-events", Command: []string{"sh", "-c", "printf event"}})
	if err != nil {
		t.Fatal(err)
	}
	_ = waitForTerminal(t, manager, session.ID, time.Second)
	events, err := store.ReadEvents()
	if err != nil {
		t.Fatal(err)
	}
	if !containsRunEvent(events, "terminal_session_started") || !containsRunEvent(events, "terminal_session_exited") {
		t.Fatalf("event-store lifecycle events = %+v", events)
	}
}

func TestTerminalSession_ProcessGroupKillDescendants(t *testing.T) {
	workspace := t.TempDir()
	manager, err := NewTerminalSessionManager(workspace, nil)
	if err != nil {
		t.Fatal(err)
	}

	pidFile := filepath.Join(workspace, "child.pid")
	cmdStr := fmt.Sprintf("sh -c 'sleep 30 & echo $! > %s; wait'", pidFile)

	ctx := WithTerminalTaskID(context.Background(), "task-pgroup")
	session, err := manager.Start(ctx, TerminalStartRequest{
		RunID: "run-pgroup", OwnerTaskID: "task-pgroup", Agent: "worker",
		Command: []string{"sh", "-c", cmdStr},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Wait for child PID to be written to pidFile
	var childPID int
	for i := 0; i < 50; i++ {
		data, err := os.ReadFile(pidFile)
		if err == nil && len(strings.TrimSpace(string(data))) > 0 {
			childPID, _ = strconv.Atoi(strings.TrimSpace(string(data)))
			if childPID > 0 {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	if childPID <= 0 {
		t.Fatalf("child PID file was not written in time")
	}

	if !isPIDAlive(childPID) {
		t.Fatalf("child process %d should be alive before Close", childPID)
	}

	// Close session (kills process group)
	if err := manager.Close(ctx, session.ID); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Wait up to 500ms for OS to clean up process
	dead := false
	for i := 0; i < 25; i++ {
		if !isPIDAlive(childPID) {
			dead = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Verify descendant child PID is killed and no longer alive
	if !dead {
		t.Fatalf("descendant child process %d survived parent termination (session PID %d)", childPID, session.PID)
	}
}

func TestTerminalSession_ProcessGroupKillDescendants_NetworkBlock(t *testing.T) {
	workspace := t.TempDir()
	manager, err := NewTerminalSessionManager(workspace, nil)
	if err != nil {
		t.Fatal(err)
	}

	pidFile := filepath.Join(workspace, "child_net.pid")
	cmdStr := fmt.Sprintf("sh -c 'sleep 30 & echo $! > %s; wait'", pidFile)

	ctx := WithTerminalTaskID(context.Background(), "task-pgroup-net")
	session, err := manager.Start(ctx, TerminalStartRequest{
		RunID: "run-pgroup-net", OwnerTaskID: "task-pgroup-net", Agent: "worker",
		Command:      []string{"sh", "-c", cmdStr},
		NetworkBlock: true,
	})
	if err != nil {
		t.Skipf("Skipping NetworkBlock test due to system/environment limitation: %v", err)
		return
	}

	// Wait for child PID to be written to pidFile
	var childPID int
	for i := 0; i < 50; i++ {
		data, err := os.ReadFile(pidFile)
		if err == nil && len(strings.TrimSpace(string(data))) > 0 {
			childPID, _ = strconv.Atoi(strings.TrimSpace(string(data)))
			if childPID > 0 {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	if childPID <= 0 {
		t.Fatalf("child PID file was not written in time")
	}

	if !isPIDAlive(childPID) {
		t.Fatalf("child process %d should be alive before Close", childPID)
	}

	// Close session (kills process group with network blocking enabled)
	if err := manager.Close(ctx, session.ID); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Wait up to 500ms for OS to clean up process
	dead := false
	for i := 0; i < 25; i++ {
		if !isPIDAlive(childPID) {
			dead = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Verify descendant child PID is killed and no longer alive even with NetworkBlock: true
	if !dead {
		t.Fatalf("descendant child process %d survived parent termination in NetworkBlock session (session PID %d)", childPID, session.PID)
	}
}

func TestTerminalSession_ChildTimeout_ProcessGroupKillDescendants_NetworkBlock(t *testing.T) {
	workspace := t.TempDir()
	manager, err := NewTerminalSessionManager(workspace, nil)
	if err != nil {
		t.Fatal(err)
	}

	pidFile := filepath.Join(workspace, "child_timeout_net.pid")
	cmdStr := fmt.Sprintf("sh -c 'sleep 30 & echo $! > %s; wait'", pidFile)

	ctx := WithTerminalTaskID(context.Background(), "task-pgroup-timeout-net")
	session, err := manager.Start(ctx, TerminalStartRequest{
		RunID: "run-pgroup-timeout-net", OwnerTaskID: "task-pgroup-timeout-net", Agent: "worker",
		Command:      []string{"sh", "-c", cmdStr},
		NetworkBlock: true,
		ChildTimeout: 50 * time.Millisecond,
	})
	if err != nil {
		t.Skipf("Skipping NetworkBlock test due to system/environment limitation: %v", err)
		return
	}

	// Wait for child PID to be written to pidFile
	var childPID int
	for i := 0; i < 50; i++ {
		data, err := os.ReadFile(pidFile)
		if err == nil && len(strings.TrimSpace(string(data))) > 0 {
			childPID, _ = strconv.Atoi(strings.TrimSpace(string(data)))
			if childPID > 0 {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	if childPID <= 0 {
		t.Fatalf("child PID file was not written in time")
	}

	// Wait for session exit due to ChildTimeout
	completed := waitForTerminal(t, manager, session.ID, 2*time.Second)
	if completed.Running {
		t.Fatalf("session still running after ChildTimeout")
	}

	// Wait up to 500ms for OS to clean up process
	dead := false
	for i := 0; i < 25; i++ {
		if !isPIDAlive(childPID) {
			dead = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Verify descendant child PID is killed on timeout even with NetworkBlock: true
	if !dead {
		t.Fatalf("descendant child process %d survived ChildTimeout in NetworkBlock session (session PID %d)", childPID, session.PID)
	}
}

func TestTerminalSession_RestoredUnknownCloseGate(t *testing.T) {
	workspace := t.TempDir()
	manager, err := NewTerminalSessionManager(workspace, nil)
	if err != nil {
		t.Fatal(err)
	}

	ctx := WithTerminalTaskID(context.Background(), "task-unknown")
	session, err := manager.Start(ctx, TerminalStartRequest{
		RunID: "run-unknown", OwnerTaskID: "task-unknown", Agent: "worker",
		Command: []string{"sh", "-c", "sleep 30"},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Restore manager state to simulate restart (session -> state unknown)
	var events []map[string]interface{}
	var eventsMu sync.Mutex
	restored, err := NewTerminalSessionManager(workspace, func(eventType, taskID string, payload map[string]interface{}) {
		eventsMu.Lock()
		defer eventsMu.Unlock()
		events = append(events, payload)
	})
	if err != nil {
		t.Fatalf("NewTerminalSessionManager restore: %v", err)
	}

	// Verify restored session is in unknown state
	reconciled, err := restored.Reconcile(context.Background(), session.ID)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if reconciled.State != TerminalSessionUnknown && isPIDAlive(session.PID) {
		t.Fatalf("restored session state = %s, want unknown while process alive", reconciled.State)
	}

	// Calling Close on restored unknown session should verify PID, terminate process, and record evidence
	if err := restored.Close(ctx, session.ID); err != nil {
		t.Fatalf("Close on restored unknown session: %v", err)
	}

	// Process must be dead
	if isPIDAlive(session.PID) {
		t.Fatalf("restored process %d is still alive after Close", session.PID)
	}

	// Task closed gate must now pass
	if err := restored.RequireTaskClosed("task-unknown"); err != nil {
		t.Fatalf("RequireTaskClosed failed after restored session closed: %v", err)
	}

	eventsMu.Lock()
	defer eventsMu.Unlock()
	foundCloseEvidence := false
	for _, p := range events {
		if p["session_id"] == session.ID && p["reconciled"] == true {
			foundCloseEvidence = true
			break
		}
	}
	if !foundCloseEvidence {
		t.Fatalf("expected evidence payload in close event for restored session, got events: %+v", events)
	}
}

func TestTerminalSession_EventPayloadIdentity(t *testing.T) {
	workspace := t.TempDir()
	var emittedPayloads []map[string]interface{}
	var mu sync.Mutex

	manager, err := NewTerminalSessionManager(workspace, func(eventType, taskID string, payload map[string]interface{}) {
		mu.Lock()
		defer mu.Unlock()
		emittedPayloads = append(emittedPayloads, payload)
	})
	if err != nil {
		t.Fatal(err)
	}

	taskID := "task-multi-sess"
	ctx := WithTerminalTaskID(context.Background(), taskID)

	s1, err := manager.Start(ctx, TerminalStartRequest{
		RunID: "run-identity", OwnerTaskID: taskID, Agent: "agent-1", Command: []string{"echo", "sess1"},
	})
	if err != nil {
		t.Fatalf("Start s1: %v", err)
	}

	s2, err := manager.Start(ctx, TerminalStartRequest{
		RunID: "run-identity", OwnerTaskID: taskID, Agent: "agent-2", Command: []string{"echo", "sess2"},
	})
	if err != nil {
		t.Fatalf("Start s2: %v", err)
	}

	_ = waitForTerminal(t, manager, s1.ID, time.Second)
	_ = waitForTerminal(t, manager, s2.ID, time.Second)

	mu.Lock()
	defer mu.Unlock()

	if len(emittedPayloads) < 4 {
		t.Fatalf("expected at least 4 events, got %d", len(emittedPayloads))
	}

	s1EventCount := 0
	s2EventCount := 0
	for _, p := range emittedPayloads {
		sid, _ := p["session_id"].(string)
		if sid == "" {
			t.Fatalf("emitted payload missing session_id: %+v", p)
		}
		if p["run_id"] != "run-identity" || p["owner_task_id"] != taskID {
			t.Fatalf("emitted payload missing identity envelope: %+v", p)
		}
		if sid == s1.ID {
			s1EventCount++
		} else if sid == s2.ID {
			s2EventCount++
		}
	}

	if s1EventCount == 0 || s2EventCount == 0 {
		t.Fatalf("events were not properly attributed: s1 count=%d, s2 count=%d", s1EventCount, s2EventCount)
	}
}

func TestTerminalTools_AgentIntegration(t *testing.T) {
	workspace := t.TempDir()
	manager, err := NewTerminalSessionManager(workspace, nil)
	if err != nil {
		t.Fatal(err)
	}

	coord := &Coordinator{
		executionRunID:     "run-agent-test",
		terminalSessionMgr: manager,
		taskTracker:        NewTaskTracker(),
	}

	startTool := &terminalStartTool{coordinator: coord}
	writeTool := &terminalWriteTool{coordinator: coord}
	readTool := &terminalReadTool{coordinator: coord}
	waitTool := &terminalWaitTool{coordinator: coord}
	closeTool := &terminalCloseTool{coordinator: coord}

	ctx := WithTerminalTaskID(context.Background(), "task-agent-test")
	ctx = context.WithValue(ctx, tools.AgentToolsAllowedKey, []string{"terminal", "terminal_start", "terminal_write", "terminal_read", "terminal_wait", "terminal_close", "terminal_list", "terminal_reconcile"})

	// 1. Start session via tool
	startResp, err := startTool.Run(ctx, fantasy.ToolCall{
		Input: `{"command":["sh","-c","read line; echo hello:$line"],"working_dir":""}`,
	})
	if err != nil {
		t.Fatalf("startTool error: %v", err)
	}
	if strings.Contains(startResp.Content, "ERROR:") {
		t.Fatalf("startTool returned error: %s", startResp.Content)
	}

	var sessInfo TerminalSession
	if err := json.Unmarshal([]byte(startResp.Content), &sessInfo); err != nil {
		t.Fatalf("failed to unmarshal start response: %v, body: %s", err, startResp.Content)
	}
	if sessInfo.ID == "" {
		t.Fatalf("expected non-empty session ID from startTool")
	}

	// 2. Write to session via tool
	writeResp, err := writeTool.Run(ctx, fantasy.ToolCall{
		Input: fmt.Sprintf(`{"id":%q,"data":"world\n"}`, sessInfo.ID),
	})
	if err != nil {
		t.Fatalf("writeTool error: %v", err)
	}
	if strings.Contains(writeResp.Content, "ERROR:") {
		t.Fatalf("writeTool returned error: %s", writeResp.Content)
	}

	// 3. Read output via tool
	completed := waitForTerminal(t, manager, sessInfo.ID, time.Second)
	if completed.State != TerminalSessionExited {
		t.Fatalf("expected session exited, got %s", completed.State)
	}
	waitResp, err := waitTool.Run(ctx, fantasy.ToolCall{
		Input: fmt.Sprintf(`{"id":%q,"target":"exit"}`, sessInfo.ID),
	})
	if err != nil || strings.Contains(waitResp.Content, "ERROR:") || !strings.Contains(waitResp.Content, sessInfo.ID) {
		t.Fatalf("waitTool response = %q, %v", waitResp.Content, err)
	}

	readResp, err := readTool.Run(ctx, fantasy.ToolCall{
		Input: fmt.Sprintf(`{"id":%q}`, sessInfo.ID),
	})
	if err != nil {
		t.Fatalf("readTool error: %v", err)
	}
	if !strings.Contains(readResp.Content, "hello:world") {
		t.Fatalf("readTool content = %s, want hello:world", readResp.Content)
	}

	// 4. Close session via tool
	closeResp, err := closeTool.Run(ctx, fantasy.ToolCall{
		Input: fmt.Sprintf(`{"id":%q}`, sessInfo.ID),
	})
	if err != nil {
		t.Fatalf("closeTool error: %v", err)
	}
	if strings.Contains(closeResp.Content, "ERROR:") {
		t.Fatalf("closeTool returned error: %s", closeResp.Content)
	}

	// 5. Verify task gate is clear
	if err := manager.RequireTaskClosed("task-agent-test"); err != nil {
		t.Fatalf("RequireTaskClosed failed: %v", err)
	}
}

func waitForTerminal(t *testing.T, manager *TerminalSessionManager, id string, timeout time.Duration) TerminalSession {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		sessions, err := manager.List(context.Background(), "")
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		for _, session := range sessions {
			if session.ID == id && !session.Running {
				return session
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("terminal session %q did not finish within %s", id, timeout)
	return TerminalSession{}
}

func containsTerminalEvent(events []string, want string) bool {
	for _, event := range events {
		if event == want {
			return true
		}
	}
	return false
}

func containsRunEvent(events []RunEvent, want string) bool {
	for _, event := range events {
		if event.Type == want {
			return true
		}
	}
	return false
}

func TestSelectTools_TerminalToolsNotAlwaysIncluded(t *testing.T) {
	allTools := []fantasy.AgentTool{
		&terminalTool{},
		&terminalStartTool{},
		&terminalWriteTool{},
		&terminalReadTool{},
		&terminalWaitTool{},
		&terminalCloseTool{},
		&terminalListTool{},
		&terminalReconcileTool{},
	}

	// Read-only agent should NOT get terminal tools
	selected := agent.SelectTools(allTools, "view,grep")
	for _, tool := range selected {
		name := tool.Info().Name
		if strings.HasPrefix(name, "terminal") {
			t.Fatalf("unexpected terminal tool %q selected for read-only agent", name)
		}
	}

	// Agent requesting terminal should get terminal tools via ExpandImpliedTools
	expanded := agent.ExpandImpliedTools("terminal")
	selectedExpanded := agent.SelectTools(allTools, expanded)
	if len(selectedExpanded) != len(allTools) {
		t.Fatalf("expected all %d terminal tools selected when 'terminal' requested, got %d", len(allTools), len(selectedExpanded))
	}
}

func TestTerminalTools_PolicyEnforcement(t *testing.T) {
	workspace := t.TempDir()
	manager, err := NewTerminalSessionManager(workspace, nil)
	if err != nil {
		t.Fatal(err)
	}

	coord := &Coordinator{
		executionRunID:     "run-policy-test",
		terminalSessionMgr: manager,
		taskTracker:        NewTaskTracker(),
		projectDir:         workspace,
	}

	startTool := &terminalStartTool{coordinator: coord}
	ctx := WithTerminalTaskID(context.Background(), "task-policy")
	ctx = context.WithValue(ctx, tools.AgentNameKey, "test-agent")

	// 1. Force-MCP test: should be blocked
	forceMcpCtx := context.WithValue(ctx, tools.AgentForceMCPKey, true)
	resp, err := startTool.Run(forceMcpCtx, fantasy.ToolCall{
		Input: `{"command":["echo","test"]}`,
	})
	if err != nil {
		t.Fatalf("startTool error: %v", err)
	}
	if !strings.Contains(resp.Content, "blocked by --force-mcp") {
		t.Fatalf("expected --force-mcp blockage, got %q", resp.Content)
	}

	// 2. Unattended mode without allowlist: should be blocked
	unattendedCtx := context.WithValue(ctx, tools.UnattendedKey, true)
	resp, err = startTool.Run(unattendedCtx, fantasy.ToolCall{
		Input: `{"command":["echo","test"]}`,
	})
	if err != nil {
		t.Fatalf("startTool error: %v", err)
	}
	if !strings.Contains(resp.Content, "not permitted") {
		t.Fatalf("expected unattended allowlist blockage, got %q", resp.Content)
	}

	// 3. Path restriction test: outside allowed paths should be blocked
	allowedPathsCtx := context.WithValue(ctx, tools.AgentToolsAllowedKey, []string{"terminal", "terminal_start"})
	allowedPathsCtx = context.WithValue(allowedPathsCtx, tools.AgentAllowedPathsKey, []string{filepath.Join(workspace, "allowed")})
	resp, err = startTool.Run(allowedPathsCtx, fantasy.ToolCall{
		Input: fmt.Sprintf(`{"command":["echo","test"],"working_dir":%q}`, filepath.Join(workspace, "restricted")),
	})
	if err != nil {
		t.Fatalf("startTool error: %v", err)
	}
	if !strings.Contains(resp.Content, "outside allowed paths") {
		t.Fatalf("expected outside allowed paths blockage, got %q", resp.Content)
	}
}

func TestTerminalTools_LazilyEnablePTYOnStart(t *testing.T) {
	workspace := t.TempDir()
	manager, err := NewTerminalSessionManager(workspace, nil)
	if err != nil {
		t.Fatal(err)
	}
	coord := &Coordinator{
		session:            &TeamSession{Workspace: workspace},
		executionRunID:     "run-pty-lazy",
		terminalSessionMgr: manager,
		taskTracker:        NewTaskTracker(),
		reportStatus:       func(StatusEvent) {},
	}
	// Probe the same broker startup operation used by the lazy PTY path so a
	// restricted test sandbox can skip using the precise underlying errno. The
	// production terminal tool still preserves its normal response-only error
	// behavior.
	probe, err := StartTerminalBroker(workspace, manager)
	if err != nil {
		if isTerminalBrokerSandboxEPERM(err) {
			t.Skipf("sandbox does not permit Unix socket setup: %v", err)
		}
		t.Fatal(err)
	}
	_ = probe.Close()
	ctx := WithTerminalTaskID(context.Background(), "task-pty-lazy")
	ctx = context.WithValue(ctx, tools.AgentToolsAllowedKey, []string{"terminal", "terminal_start"})
	resp, err := (&terminalStartTool{coordinator: coord}).Run(ctx, fantasy.ToolCall{Input: `{"command":["sh","-c","true"],"pty":true}`})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(resp.Content, "ERROR:") {
		t.Fatalf("response = %q", resp.Content)
	}
	if !coord.PTYTerminalEnabled() {
		t.Fatal("pty:true did not lazily start the terminal broker")
	}
	var session TerminalSession
	if err := json.Unmarshal([]byte(resp.Content), &session); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = manager.Close(ctx, session.ID) }()
	defer func() { _ = coord.terminalBroker.Close() }()
	if session.Mode != TerminalModePTY {
		t.Fatalf("terminal mode = %q, want %q", session.Mode, TerminalModePTY)
	}
}

func TestTerminalSession_PIDIdentityMismatch(t *testing.T) {
	workspace := t.TempDir()
	manager, err := NewTerminalSessionManager(workspace, nil)
	if err != nil {
		t.Fatal(err)
	}

	ctx := WithTerminalTaskID(context.Background(), "task-mismatch")
	session, err := manager.Start(ctx, TerminalStartRequest{
		RunID: "run-mismatch", OwnerTaskID: "task-mismatch", Agent: "worker",
		Command: []string{"sh", "-c", "sleep 30"},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = manager.Close(ctx, session.ID) }()

	// Restore manager state to simulate restart
	restored, err := NewTerminalSessionManager(workspace, nil)
	if err != nil {
		t.Fatalf("NewTerminalSessionManager restore: %v", err)
	}

	// Tamper with durable ProcessIdentity to simulate PID reuse by a different process
	restored.mu.Lock()
	if managed, ok := restored.sessions[session.ID]; ok && managed.session.ProcessIdentity != nil {
		managed.session.ProcessIdentity.StartTime += 999999
		managed.session.ProcessIdentity.PGID += 999999
	}
	restored.mu.Unlock()

	// Reconcile should detect mismatch
	reconciled, err := restored.Reconcile(context.Background(), session.ID)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if reconciled.State != TerminalSessionUnknown {
		t.Fatalf("expected state unknown after identity mismatch, got %s", reconciled.State)
	}

	// Close should reject terminating the mismatched process and return error
	err = restored.Close(ctx, session.ID)
	if err == nil || !strings.Contains(err.Error(), "identity mismatch") {
		t.Fatalf("Close on identity mismatch error = %v, want identity mismatch rejection", err)
	}

	// Session must remain in unknown state
	if err := restored.RequireTaskClosed("task-mismatch"); err == nil {
		t.Fatalf("RequireTaskClosed should fail for unknown session after identity mismatch")
	}
}

func TestTerminalSessionUserLeaseBlocksAgentWrite(t *testing.T) {
	manager, err := NewTerminalSessionManager(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx := WithTerminalTaskID(context.Background(), "task-lease")
	session, err := manager.Start(ctx, TerminalStartRequest{
		RunID: "run-lease", OwnerTaskID: "task-lease",
		Command: []string{"sh", "-c", "read line; printf %s \"$line\""},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = manager.Close(ctx, session.ID) }()

	lease, err := manager.AcquireUserLease(session.ID)
	if err != nil {
		t.Fatalf("AcquireUserLease: %v", err)
	}
	if lease.ID == "" {
		t.Fatal("AcquireUserLease returned empty ID")
	}
	if err := manager.Write(ctx, session.ID, TerminalInput{Data: []byte("blocked\n")}); err == nil || !strings.Contains(err.Error(), "controlled by a user") {
		t.Fatalf("Write while leased error = %v, want user controller rejection", err)
	}
	if err := manager.ReleaseUserLease(session.ID, lease.ID); err != nil {
		t.Fatalf("ReleaseUserLease: %v", err)
	}
}

func TestTerminalSessionLegacyRecordDefaultsToPipe(t *testing.T) {
	workspace := t.TempDir()
	logs := filepath.Join(workspace, logsDir)
	if err := os.MkdirAll(logs, 0o755); err != nil {
		t.Fatal(err)
	}
	legacy := `[{"id":"legacy","run_id":"run","owner_task_id":"task","state":"exited"}]`
	if err := os.WriteFile(filepath.Join(logs, terminalSessionsFile), []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	manager, err := NewTerminalSessionManager(workspace, nil)
	if err != nil {
		t.Fatal(err)
	}
	sessions, err := manager.List(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 || sessions[0].Mode != TerminalModePipe {
		t.Fatalf("legacy session mode = %+v, want pipe", sessions)
	}
}

func TestPTYSessionReportsTTYAndAcceptsInput(t *testing.T) {
	manager, err := NewTerminalSessionManager(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx := WithTerminalTaskID(context.Background(), "task-pty")
	session, err := manager.Start(ctx, TerminalStartRequest{
		RunID: "run-pty", OwnerTaskID: "task-pty", Mode: TerminalModePTY,
		Command: []string{"sh", "-c", "test -t 0 && { read line; printf 'answer:%s' \"$line\"; }"},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = manager.Close(ctx, session.ID) }()
	if err := manager.Write(ctx, session.ID, TerminalInput{Data: []byte("ok\n")}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	completed := waitForTerminal(t, manager, session.ID, time.Second)
	if completed.ExitCode == nil || *completed.ExitCode != 0 {
		t.Fatalf("PTY command exit = %+v, want 0", completed)
	}
	read, err := manager.Read(ctx, session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(read.Output), "answer:ok") {
		t.Fatalf("PTY output = %q, want answer", read.Output)
	}
}

func TestPTYSessionResizeRejectsPipeSession(t *testing.T) {
	manager, err := NewTerminalSessionManager(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx := WithTerminalTaskID(context.Background(), "task-pipe")
	session, err := manager.Start(ctx, TerminalStartRequest{
		RunID: "run-pipe", OwnerTaskID: "task-pipe", Command: []string{"sh", "-c", "sleep 1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = manager.Close(ctx, session.ID) }()
	if err := manager.Resize(ctx, session.ID, 41, 123); err == nil || !strings.Contains(err.Error(), "not a PTY") {
		t.Fatalf("Resize pipe error = %v, want PTY rejection", err)
	}
}

func TestPTYReadReturnsNormalizedBoundedScreen(t *testing.T) {
	manager, err := NewTerminalSessionManager(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx := WithTerminalTaskID(context.Background(), "task-screen")
	session, err := manager.Start(ctx, TerminalStartRequest{
		RunID: "run-screen", OwnerTaskID: "task-screen", Mode: TerminalModePTY,
		Command: []string{"sh", "-c", "printf '\\033[2Jhello\\033[0m'"},
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = waitForTerminal(t, manager, session.ID, time.Second)
	read, err := manager.Read(ctx, session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(read.Screen, "\x1b[") || !strings.Contains(read.Screen, "hello") {
		t.Fatalf("screen = %q, want ANSI-free hello", read.Screen)
	}
}
