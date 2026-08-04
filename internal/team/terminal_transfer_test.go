package team

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestTerminalTransferRequiresExplicitAuthorizationAndMovesControlNotProvenance(t *testing.T) {
	var mu sync.Mutex
	var events []struct {
		typ     string
		payload map[string]interface{}
	}
	workspace := t.TempDir()
	manager, err := NewTerminalSessionManager(workspace, func(typ, _ string, payload map[string]interface{}) {
		mu.Lock()
		defer mu.Unlock()
		events = append(events, struct {
			typ     string
			payload map[string]interface{}
		}{typ, payload})
	})
	if err != nil {
		t.Fatal(err)
	}
	active := map[string]bool{"source": true}
	manager.SetActiveTaskRoundChecker(func(id string) bool { return active[id] })
	sourceCtx := WithTerminalTaskID(context.Background(), "source")
	session, err := manager.Start(sourceCtx, TerminalStartRequest{
		RunID: "run-transfer", OwnerTaskID: "source", Agent: "worker", Command: []string{"sh", "-c", "sleep 30"},
	})
	if err != nil {
		t.Fatal(err)
	}
	req := TerminalTransferRequest{
		SessionID: session.ID, RunID: "run-transfer", SourceTaskID: "source", DestinationTaskID: "destination",
		AcceptSessionID: session.ID, AcceptMode: TerminalModePipe, Reason: "operator approved repair handoff", OperatorAuthorization: "incident-123",
	}
	if _, err := manager.TransferTerminal(context.Background(), req); err == nil || !strings.Contains(err.Error(), "active model round") {
		t.Fatalf("transfer with active source round error = %v, want active-round rejection", err)
	}
	active["source"] = false
	transferred, err := manager.TransferTerminal(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if transferred.OwnerTaskID != "source" || transferred.ControllerTaskID != "destination" {
		t.Fatalf("handoff changed provenance or did not move control: %+v", transferred)
	}
	if transferred.HandoffAuthorizedBy != "incident-123" || transferred.HandoffReason != req.Reason || transferred.HandedOffAt.IsZero() {
		t.Fatalf("handoff audit fields missing: %+v", transferred)
	}
	reloaded, err := NewTerminalSessionManager(workspace, nil)
	if err != nil {
		t.Fatal(err)
	}
	restored, err := reloaded.List(context.Background(), "run-transfer")
	if err != nil || len(restored) != 1 {
		t.Fatalf("reload transferred session = %#v, err=%v", restored, err)
	}
	if got := restored[0]; got.OwnerTaskID != "source" || got.ControllerTaskID != "destination" || got.HandoffReason != req.Reason || got.HandoffAuthorizedBy != "incident-123" || got.HandedOffAt.IsZero() {
		t.Fatalf("persisted transfer audit fields = %+v", got)
	}
	if err := manager.Write(sourceCtx, session.ID, TerminalInput{Data: []byte("x")}); err == nil || !strings.Contains(err.Error(), "belongs to task") {
		t.Fatalf("source write after transfer error = %v, want authorization rejection", err)
	}
	if err := manager.RequireTaskClosed("source"); err != nil {
		t.Fatalf("source remained blocked by transferred terminal: %v", err)
	}
	if err := manager.RequireTaskClosed("destination"); err == nil {
		t.Fatal("destination was not made responsible for the active terminal")
	}
	if err := manager.Close(WithTerminalTaskID(context.Background(), "destination"), session.ID); err != nil {
		t.Fatalf("destination close after transfer: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, event := range events {
		if event.typ == string(TerminalTaskTransferred) {
			if event.payload["owner_task_id"] != "source" || event.payload["controller_task_id"] != "destination" || event.payload["source_task_id"] != "source" || event.payload["destination_task_id"] != "destination" || event.payload["reason"] != req.Reason || event.payload["operator_authorization"] != "incident-123" || event.payload["handoff_authorized_by"] != "incident-123" {
				t.Fatalf("transfer event payload = %#v", event.payload)
			}
			if timestamp, ok := event.payload["handed_off_at"].(time.Time); !ok || timestamp.IsZero() {
				t.Fatalf("transfer event timestamp = %#v", event.payload["handed_off_at"])
			}
			return
		}
	}
	t.Fatal("missing terminal task transfer event")
}

func TestCoordinatorTransferTerminalRejectsUnsafeRequests(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Coordinator, *TaskTracker, []*TodoItem, *TerminalTransferRequest)
		wantErr string
	}{
		{name: "wrong active run", mutate: func(_ *Coordinator, _ *TaskTracker, _ []*TodoItem, req *TerminalTransferRequest) {
			req.RunID = "other-run"
		}, wantErr: "not the active run"},
		{name: "source not paused", mutate: func(_ *Coordinator, tracker *TaskTracker, items []*TodoItem, _ *TerminalTransferRequest) {
			_ = tracker.TodoList().TryUpdateStatusAndOutput(items[0].ID, TaskInProgress, "", "")
		}, wantErr: "must be paused"},
		{name: "terminal destination", mutate: func(_ *Coordinator, tracker *TaskTracker, items []*TodoItem, _ *TerminalTransferRequest) {
			_ = tracker.TodoList().TryUpdateStatusAndOutput(items[1].ID, TaskDone, "", "")
		}, wantErr: "is terminal"},
		{name: "active source round", mutate: func(coord *Coordinator, _ *TaskTracker, items []*TodoItem, _ *TerminalTransferRequest) {
			coord.registerTerminalRound(items[0].ID, func() {})
		}, wantErr: "must not have active model rounds"},
		{name: "active destination round", mutate: func(coord *Coordinator, _ *TaskTracker, items []*TodoItem, _ *TerminalTransferRequest) {
			coord.registerTerminalRound(items[1].ID, func() {})
		}, wantErr: "must not have active model rounds"},
		{name: "missing source task", mutate: func(_ *Coordinator, _ *TaskTracker, _ []*TodoItem, req *TerminalTransferRequest) {
			req.SourceTaskID = "missing"
		}, wantErr: "must exist"},
		{name: "missing destination task", mutate: func(_ *Coordinator, _ *TaskTracker, _ []*TodoItem, req *TerminalTransferRequest) {
			req.DestinationTaskID = "missing"
		}, wantErr: "must exist"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			manager, coord, tracker, items, session := transferCoordinatorFixture(t)
			req := transferRequest(session, items[0].ID, items[1].ID)
			tt.mutate(coord, tracker, items, &req)
			if _, err := coord.TransferTerminal(context.Background(), req); err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("TransferTerminal error = %v, want %q", err, tt.wantErr)
			}
			coord.unregisterTerminalRound(items[0].ID)
			coord.unregisterTerminalRound(items[1].ID)
			if err := manager.Close(WithTerminalTaskID(context.Background(), items[0].ID), session.ID); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestTerminalTransferManagerRejectsUnsafeRequests(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*TerminalSessionManager, *TerminalTransferRequest, string)
		wantErr string
	}{
		{name: "missing reason", mutate: func(_ *TerminalSessionManager, req *TerminalTransferRequest, _ string) { req.Reason = "" }, wantErr: "reason and operator authorization"},
		{name: "missing authorization", mutate: func(_ *TerminalSessionManager, req *TerminalTransferRequest, _ string) {
			req.OperatorAuthorization = ""
		}, wantErr: "reason and operator authorization"},
		{name: "mismatched accepted session", mutate: func(_ *TerminalSessionManager, req *TerminalTransferRequest, _ string) {
			req.AcceptSessionID = "other-session"
		}, wantErr: "explicitly accept the session ID"},
		{name: "missing accepted mode", mutate: func(_ *TerminalSessionManager, req *TerminalTransferRequest, _ string) { req.AcceptMode = "" }, wantErr: "explicitly accept the terminal mode"},
		{name: "mismatched accepted mode", mutate: func(_ *TerminalSessionManager, req *TerminalTransferRequest, _ string) {
			req.AcceptMode = TerminalModePTY
		}, wantErr: "was not accepted"},
		{name: "live user lease", mutate: func(manager *TerminalSessionManager, _ *TerminalTransferRequest, sessionID string) {
			if _, err := manager.AcquireUserLease(sessionID); err != nil {
				t.Fatalf("AcquireUserLease: %v", err)
			}
		}, wantErr: "user lease must be released"},
		{name: "non-owner custody", mutate: func(manager *TerminalSessionManager, _ *TerminalTransferRequest, sessionID string) {
			manager.mu.Lock()
			manager.sessions[sessionID].session.Custodian = TerminalCustodianOperator
			manager.mu.Unlock()
		}, wantErr: "unavailable during custody or cleanup"},
		{name: "cleanup state", mutate: func(manager *TerminalSessionManager, _ *TerminalTransferRequest, sessionID string) {
			manager.mu.Lock()
			manager.sessions[sessionID].session.CleanupState = TerminalCleanupRequested
			manager.mu.Unlock()
		}, wantErr: "unavailable during custody or cleanup"},
		{name: "cleanup in progress", mutate: func(manager *TerminalSessionManager, _ *TerminalTransferRequest, sessionID string) {
			manager.mu.Lock()
			manager.sessions[sessionID].cleanupInProgress = true
			manager.mu.Unlock()
		}, wantErr: "unavailable during custody or cleanup"},
		{name: "mismatched source", mutate: func(_ *TerminalSessionManager, req *TerminalTransferRequest, _ string) {
			req.SourceTaskID = "other-source"
		}, wantErr: "does not control it"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			manager, session := transferManagerFixture(t)
			req := transferRequest(session, "source", "destination")
			tt.mutate(manager, &req, session.ID)
			if _, err := manager.TransferTerminal(context.Background(), req); err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("TransferTerminal error = %v, want %q", err, tt.wantErr)
			}
			manager.mu.Lock()
			manager.sessions[session.ID].cleanupInProgress = false
			manager.sessions[session.ID].session.Custodian = TerminalCustodianOwner
			manager.sessions[session.ID].session.CleanupState = TerminalCleanupNone
			manager.mu.Unlock()
			if err := manager.Close(WithTerminalTaskID(context.Background(), "source"), session.ID); err != nil {
				t.Fatal(err)
			}
		})
	}

	t.Run("repeated transfer rejects stale source", func(t *testing.T) {
		manager, session := transferManagerFixture(t)
		req := transferRequest(session, "source", "destination")
		if _, err := manager.TransferTerminal(context.Background(), req); err != nil {
			t.Fatal(err)
		}
		if _, err := manager.TransferTerminal(context.Background(), req); err == nil || !strings.Contains(err.Error(), "does not control it") {
			t.Fatalf("repeated transfer error = %v", err)
		}
		if err := manager.Close(WithTerminalTaskID(context.Background(), "destination"), session.ID); err != nil {
			t.Fatal(err)
		}
	})
}

func transferManagerFixture(t *testing.T) (*TerminalSessionManager, *TerminalSession) {
	t.Helper()
	manager, err := NewTerminalSessionManager(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	manager.SetActiveTaskRoundChecker(func(string) bool { return false })
	session, err := manager.Start(WithTerminalTaskID(context.Background(), "source"), TerminalStartRequest{
		RunID: "run-transfer", OwnerTaskID: "source", Command: []string{"sh", "-c", "sleep 30"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return manager, session
}

func transferCoordinatorFixture(t *testing.T) (*TerminalSessionManager, *Coordinator, *TaskTracker, []*TodoItem, *TerminalSession) {
	t.Helper()
	manager, err := NewTerminalSessionManager(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	tracker := NewTaskTracker()
	items := tracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "source"}, {Agent: "worker", Desc: "destination"}})
	if err := tracker.TodoList().TryUpdateStatusAndOutput(items[0].ID, TaskInProgress, "", ""); err != nil {
		t.Fatal(err)
	}
	if err := tracker.TodoList().TryUpdateStatusAndOutput(items[0].ID, TaskPaused, "", ""); err != nil {
		t.Fatal(err)
	}
	coord := &Coordinator{terminalSessionMgr: manager, taskTracker: tracker, executionRunID: "run-transfer"}
	manager.SetActiveTaskRoundChecker(coord.isTerminalRoundActive)
	session, err := manager.Start(WithTerminalTaskID(context.Background(), items[0].ID), TerminalStartRequest{
		RunID: "run-transfer", OwnerTaskID: items[0].ID, Command: []string{"sh", "-c", "sleep 30"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return manager, coord, tracker, items, session
}

func transferRequest(session *TerminalSession, sourceTaskID, destinationTaskID string) TerminalTransferRequest {
	return TerminalTransferRequest{SessionID: session.ID, RunID: session.RunID, SourceTaskID: sourceTaskID, DestinationTaskID: destinationTaskID, AcceptSessionID: session.ID, AcceptMode: session.Mode, Reason: "manual repair", OperatorAuthorization: "incident-test"}
}

func TestCoordinatorTransferTerminalRequiresPausedSourceAndDeclaredDestination(t *testing.T) {
	manager, err := NewTerminalSessionManager(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	tracker := NewTaskTracker()
	items := tracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "source"}, {Agent: "worker", Desc: "destination"}})
	if err := tracker.TodoList().TryUpdateStatusAndOutput(items[0].ID, TaskInProgress, "", ""); err != nil {
		t.Fatal(err)
	}
	if err := tracker.TodoList().TryUpdateStatusAndOutput(items[0].ID, TaskPaused, "", ""); err != nil {
		t.Fatal(err)
	}
	coord := &Coordinator{terminalSessionMgr: manager, taskTracker: tracker, executionRunID: "run-transfer"}
	manager.SetActiveTaskRoundChecker(coord.isTerminalRoundActive)
	session, err := manager.Start(WithTerminalTaskID(context.Background(), items[0].ID), TerminalStartRequest{
		RunID: "run-transfer", OwnerTaskID: items[0].ID, Command: []string{"sh", "-c", "sleep 30"},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = coord.TransferTerminal(context.Background(), TerminalTransferRequest{
		SessionID: session.ID, SourceTaskID: items[0].ID, DestinationTaskID: items[1].ID,
		AcceptSessionID: session.ID, AcceptMode: TerminalModePipe, Reason: "manual repair", OperatorAuthorization: "operator@example.test",
	})
	if err != nil {
		t.Fatalf("coordinator transfer: %v", err)
	}
	if err := manager.Close(WithTerminalTaskID(context.Background(), items[1].ID), session.ID); err != nil {
		t.Fatal(err)
	}
}

func TestTerminalTransferMakesDestinationResponsibleForCleanup(t *testing.T) {
	manager, err := NewTerminalSessionManager(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	manager.SetActiveTaskRoundChecker(func(string) bool { return false })
	session, err := manager.Start(WithTerminalTaskID(context.Background(), "source"), TerminalStartRequest{
		RunID: "run-transfer-cleanup", OwnerTaskID: "source", Command: []string{"sh", "-c", "sleep 30"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.TransferTerminal(context.Background(), TerminalTransferRequest{
		SessionID: session.ID, RunID: "run-transfer-cleanup", SourceTaskID: "source", DestinationTaskID: "destination",
		AcceptSessionID: session.ID, AcceptMode: TerminalModePipe, Reason: "operator approved containment", OperatorAuthorization: "incident-456",
	}); err != nil {
		t.Fatal(err)
	}
	results, err := manager.CleanupTaskTerminals(context.Background(), TerminalCleanupRequest{
		OwnerTaskID: "destination", Reason: TerminalCleanupTaskFailed,
	})
	if err != nil || len(results) != 1 || results[0].ManualAction || results[0].Session.CleanupState != TerminalCleanupCompleted {
		t.Fatalf("destination cleanup results = %#v, err=%v", results, err)
	}
	if err := manager.RequireTaskClosed("destination"); err != nil {
		t.Fatalf("destination cleanup remained a retry gate: %v", err)
	}
}
