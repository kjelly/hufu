package team

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestTerminalCleanupLegacyDefaultsAndOwnershipGuard(t *testing.T) {
	workspace := t.TempDir()
	path := filepath.Join(workspace, logsDir, terminalSessionsFile)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal([]TerminalSession{{ID: "legacy", OwnerTaskID: "task-a", State: TerminalSessionExited}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	manager, err := NewTerminalSessionManager(workspace, nil)
	if err != nil {
		t.Fatal(err)
	}
	sessions, err := manager.List(context.Background(), "")
	if err != nil || len(sessions) != 1 {
		t.Fatalf("legacy sessions = %#v, err=%v", sessions, err)
	}
	if sessions[0].Custodian != TerminalCustodianOwner || sessions[0].CleanupState != TerminalCleanupNone {
		t.Fatalf("legacy cleanup defaults = %#v", sessions[0])
	}
	var persisted []map[string]json.RawMessage
	data, err = os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &persisted); err != nil {
		t.Fatal(err)
	}
	if len(persisted) != 1 {
		t.Fatalf("persisted legacy sessions = %#v", persisted)
	}
	for _, field := range []string{"custodian", "cleanup_state"} {
		if _, ok := persisted[0][field]; !ok {
			t.Fatalf("legacy migration did not persist %q: %s", field, data)
		}
	}
	if _, err := manager.CleanupTaskTerminals(context.Background(), TerminalCleanupRequest{OwnerTaskID: "other"}); err != nil {
		t.Fatalf("cleanup of unrelated task should be a no-op: %v", err)
	}
}

func TestTerminalCleanupGracefulCustodyTransferAndLeaseRevocation(t *testing.T) {
	workspace := t.TempDir()
	var events []string
	var gracefulDurable bool
	sessionPath := filepath.Join(workspace, logsDir, terminalSessionsFile)
	manager, err := NewTerminalSessionManager(workspace, func(eventType, _ string, _ map[string]interface{}) {
		events = append(events, eventType)
		if eventType == string(TerminalCleanupGracefulEvent) {
			data, readErr := os.ReadFile(sessionPath)
			var persisted []TerminalSession
			if readErr == nil && json.Unmarshal(data, &persisted) == nil && len(persisted) == 1 {
				gracefulDurable = persisted[0].CleanupState == TerminalCleanupGraceful
			}
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	owner := WithTerminalTaskID(context.Background(), "task-cleanup")
	session, err := manager.Start(owner, TerminalStartRequest{
		RunID: "run-cleanup", OwnerTaskID: "task-cleanup", Command: []string{"sh", "-c", "trap 'exit 0' TERM; while :; do sleep .05; done"},
	})
	if err != nil {
		t.Fatal(err)
	}
	lease, err := manager.AcquireUserLease(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	results, err := manager.CleanupTaskTerminals(context.Background(), TerminalCleanupRequest{
		OwnerTaskID: "task-cleanup", Reason: TerminalCleanupTaskFailed,
		GracePeriod: 300 * time.Millisecond, ForceAfter: 300 * time.Millisecond,
	})
	if err != nil || len(results) != 1 {
		t.Fatalf("cleanup results = %#v, err=%v", results, err)
	}
	result := results[0]
	if result.ManualAction || !result.Graceful || result.Session.Custodian != TerminalCustodianCoordinator || result.Session.CleanupState != TerminalCleanupCompleted {
		t.Fatalf("cleanup result = %#v", result)
	}
	if !gracefulDurable {
		t.Fatal("graceful cleanup state was not durable when graceful event was emitted")
	}
	if err := manager.WriteUserLease(session.ID, lease.ID, []byte("must-fail\n")); err == nil {
		t.Fatal("revoked user lease accepted input")
	}
	if err := manager.RequireTaskClosed("task-cleanup"); err != nil {
		t.Fatalf("contained terminal still blocks task closure: %v", err)
	}
	for _, want := range []string{string(TerminalCustodyTransferred), string(TerminalCleanupRequestedEvent), string(TerminalLeaseRevoked), string(TerminalCleanupGracefulEvent), string(TerminalCleanupCompletedEvent)} {
		found := false
		for _, got := range events {
			if got == want {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("events = %v, missing %q", events, want)
		}
	}
	index := func(want string) int {
		for i, got := range events {
			if got == want {
				return i
			}
		}
		return -1
	}
	if index(string(TerminalCustodyTransferred)) > index(string(TerminalCleanupRequestedEvent)) || index(string(TerminalCleanupRequestedEvent)) > index(string(TerminalLeaseRevoked)) || index(string(TerminalCleanupGracefulEvent)) > index(string(TerminalCleanupCompletedEvent)) {
		t.Fatalf("cleanup events out of order: %v", events)
	}
}

func TestTerminalCleanupEscalatesAndIsIdempotent(t *testing.T) {
	workspace := t.TempDir()
	manager, err := NewTerminalSessionManager(workspace, nil)
	if err != nil {
		t.Fatal(err)
	}
	owner := WithTerminalTaskID(context.Background(), "task-force")
	_, err = manager.Start(owner, TerminalStartRequest{
		RunID: "run-force", OwnerTaskID: "task-force", Command: []string{"sh", "-c", "trap '' TERM; exec sleep 5"},
	})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	results, err := manager.CleanupTaskTerminals(context.Background(), TerminalCleanupRequest{
		OwnerTaskID: "task-force", Reason: TerminalCleanupTaskCancelled,
		GracePeriod: 20 * time.Millisecond, ForceAfter: 500 * time.Millisecond,
	})
	if err != nil || len(results) != 1 {
		t.Fatalf("forced cleanup results = %#v, err=%v", results, err)
	}
	if !results[0].Forced || results[0].ManualAction || results[0].Session.CleanupState != TerminalCleanupCompleted {
		t.Fatalf("forced cleanup result = %#v", results[0])
	}
	second, err := manager.CleanupTaskTerminals(context.Background(), TerminalCleanupRequest{OwnerTaskID: "task-force"})
	if err != nil || len(second) != 0 {
		t.Fatalf("idempotent cleanup = %#v, err=%v", second, err)
	}
}

func TestTerminalCleanupForcedStatePersistenceFailureStopsEscalation(t *testing.T) {
	workspace := t.TempDir()
	manager, err := NewTerminalSessionManager(workspace, nil)
	if err != nil {
		t.Fatal(err)
	}
	owner := WithTerminalTaskID(context.Background(), "task-persist-failure")
	session, err := manager.Start(owner, TerminalStartRequest{
		RunID: "run-persist-failure", OwnerTaskID: "task-persist-failure", Command: []string{"sh", "-c", "trap '' TERM; exec sleep 5"},
	})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	cleanupDone := make(chan error, 1)
	go func() {
		_, cleanupErr := manager.CleanupTaskTerminals(context.Background(), TerminalCleanupRequest{
			OwnerTaskID: "task-persist-failure", Reason: TerminalCleanupTaskFailed,
			GracePeriod: 400 * time.Millisecond, ForceAfter: 500 * time.Millisecond,
		})
		cleanupDone <- cleanupErr
	}()

	deadline := time.Now().Add(time.Second)
	for {
		listed, listErr := manager.List(context.Background(), "run-persist-failure")
		if listErr != nil {
			t.Fatal(listErr)
		}
		if len(listed) == 1 && listed[0].CleanupState == TerminalCleanupRequested {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("cleanup did not persist requested state")
		}
		time.Sleep(5 * time.Millisecond)
	}
	badPath := filepath.Join(workspace, "persist-failure-is-a-directory")
	if err := os.Mkdir(badPath, 0o755); err != nil {
		t.Fatal(err)
	}
	manager.mu.Lock()
	manager.path = badPath
	manager.mu.Unlock()
	if err := <-cleanupDone; err == nil || !strings.Contains(err.Error(), "persist forced terminal cleanup state") {
		t.Fatalf("forced persistence failure = %v", err)
	}
	if !isPIDAlive(session.PID) {
		t.Fatal("cleanup escalated to SIGKILL despite forced-state persistence failure")
	}
	manager.mu.Lock()
	manager.path = filepath.Join(workspace, logsDir, terminalSessionsFile)
	manager.mu.Unlock()
	results, err := manager.CleanupTaskTerminals(context.Background(), TerminalCleanupRequest{
		OwnerTaskID: "task-persist-failure", Reason: TerminalCleanupTaskFailed,
		GracePeriod: 20 * time.Millisecond, ForceAfter: 500 * time.Millisecond,
	})
	if err != nil || len(results) != 1 || !results[0].Forced {
		t.Fatalf("cleanup after persistence recovery = %#v, err=%v", results, err)
	}
}

func TestTerminalCleanupCustodyBlocksOwnerAndConcurrentCleanup(t *testing.T) {
	workspace := t.TempDir()
	manager, err := NewTerminalSessionManager(workspace, nil)
	if err != nil {
		t.Fatal(err)
	}
	owner := WithTerminalTaskID(context.Background(), "task-serialized")
	session, err := manager.Start(owner, TerminalStartRequest{
		RunID: "run-serialized", OwnerTaskID: "task-serialized", Command: []string{"sh", "-c", "trap '' TERM; exec sleep 5"},
	})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	firstDone := make(chan error, 1)
	go func() {
		_, cleanupErr := manager.CleanupTaskTerminals(context.Background(), TerminalCleanupRequest{
			OwnerTaskID: "task-serialized", Reason: TerminalCleanupTaskCancelled,
			GracePeriod: 200 * time.Millisecond, ForceAfter: 500 * time.Millisecond,
		})
		firstDone <- cleanupErr
	}()

	deadline := time.Now().Add(time.Second)
	for {
		listed, listErr := manager.List(context.Background(), "run-serialized")
		if listErr != nil {
			t.Fatal(listErr)
		}
		if len(listed) == 1 && listed[0].CleanupState == TerminalCleanupRequested {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("cleanup did not acquire custody")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err := manager.Write(owner, session.ID, TerminalInput{OwnerTaskID: "task-serialized", Data: []byte("blocked\n")}); err == nil {
		t.Fatal("owner write remained available after cleanup custody transfer")
	}
	if _, err := manager.CleanupTaskTerminals(context.Background(), TerminalCleanupRequest{OwnerTaskID: "task-serialized"}); err == nil || !strings.Contains(err.Error(), "already in progress") {
		t.Fatalf("concurrent cleanup error = %v", err)
	}
	if err := <-firstDone; err != nil {
		t.Fatal(err)
	}
}

func TestTerminalCleanupActiveRoundAndIdentityMismatchAreFailClosed(t *testing.T) {
	workspace := t.TempDir()
	manager, err := NewTerminalSessionManager(workspace, nil)
	if err != nil {
		t.Fatal(err)
	}
	active := true
	manager.SetActiveTaskRoundChecker(func(string) bool { return active })
	owner := WithTerminalTaskID(context.Background(), "task-active")
	_, err = manager.Start(owner, TerminalStartRequest{RunID: "run", OwnerTaskID: "task-active", Command: []string{"sh", "-c", "sleep 3"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.CleanupTaskTerminals(context.Background(), TerminalCleanupRequest{OwnerTaskID: "task-active"}); err == nil || !strings.Contains(err.Error(), "active model round") {
		t.Fatalf("active round cleanup error = %v", err)
	}
	active = false
	if _, err := manager.CleanupTaskTerminals(context.Background(), TerminalCleanupRequest{OwnerTaskID: "task-active", GracePeriod: 20 * time.Millisecond}); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command("sleep", "5")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	identity, err := getProcessIdentity(cmd.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	identity.StartTime++
	path := filepath.Join(workspace, logsDir, terminalSessionsFile)
	data, _ := json.Marshal([]TerminalSession{{ID: "unknown", RunID: "run", OwnerTaskID: "task-unknown", State: TerminalSessionUnknown, PID: cmd.Process.Pid, ProcessIdentity: identity}})
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	restored, err := NewTerminalSessionManager(workspace, nil)
	if err != nil {
		t.Fatal(err)
	}
	results, err := restored.CleanupTaskTerminals(context.Background(), TerminalCleanupRequest{OwnerTaskID: "task-unknown"})
	if err != nil || len(results) != 1 || !results[0].ManualAction || results[0].Session.CleanupState != TerminalCleanupManual {
		t.Fatalf("identity mismatch cleanup = %#v, err=%v", results, err)
	}
}
