package team

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
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
	closeTool := &terminalCloseTool{coordinator: coord}

	ctx := WithTerminalTaskID(context.Background(), "task-agent-test")
	ctx = context.WithValue(ctx, tools.AgentToolsAllowedKey, []string{"terminal", "terminal_start", "terminal_write", "terminal_read", "terminal_close", "terminal_list", "terminal_reconcile"})

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

func TestTerminalTools_RejectPTYWhenFeatureDisabled(t *testing.T) {
	manager, err := NewTerminalSessionManager(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	coord := &Coordinator{executionRunID: "run-pty-disabled", terminalSessionMgr: manager, taskTracker: NewTaskTracker()}
	ctx := WithTerminalTaskID(context.Background(), "task-pty-disabled")
	ctx = context.WithValue(ctx, tools.AgentToolsAllowedKey, []string{"terminal", "terminal_start"})
	resp, err := (&terminalStartTool{coordinator: coord}).Run(ctx, fantasy.ToolCall{Input: `{"command":["sh","-c","true"],"pty":true}`})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(resp.Content, "PTY terminal feature is disabled") {
		t.Fatalf("response = %q, want feature-flag rejection", resp.Content)
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
