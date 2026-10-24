package team

import (
	"context"
	"encoding/json"
	"net"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestTerminalBrokerCloseInterruptsIdleClient(t *testing.T) {
	workspace := terminalBrokerTestWorkspace(t)
	manager, err := NewTerminalSessionManager(workspace, nil)
	if err != nil {
		t.Fatal(err)
	}
	broker, err := StartTerminalBroker(workspace, manager)
	if err != nil {
		skipTerminalBrokerSandboxRestriction(t, err)
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = broker.Close() })
	conn, err := net.Dial("unix", filepath.Join(workspace, logsDir, terminalBrokerSocket))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	// A completed request proves this client has been admitted; its next read
	// blocks until Close interrupts it, without relying on a startup delay.
	if err := json.NewEncoder(conn).Encode(terminalBrokerRequest{Action: "unknown"}); err != nil {
		t.Fatal(err)
	}
	var response terminalBrokerResponse
	if err := json.NewDecoder(conn).Decode(&response); err != nil || response.Error == "" {
		t.Fatalf("handshake = %+v, err=%v", response, err)
	}
	if err := broker.Close(); err != nil {
		t.Fatal(err)
	}
	if err := conn.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Read(make([]byte, 1)); err == nil {
		t.Fatal("Close left the client connected")
	} else if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
		t.Fatal("Close did not interrupt the client")
	}
	broker.connMu.Lock()
	remaining := len(broker.conns)
	broker.connMu.Unlock()
	if remaining != 0 {
		t.Fatalf("Close retained %d client handlers", remaining)
	}
}

func TestTerminalBrokerConcurrentCloseWaitsForLeasePersistence(t *testing.T) {
	workspace := terminalBrokerTestWorkspace(t)
	abandonStarted, releaseAbandon := make(chan struct{}), make(chan struct{})
	manager, err := NewTerminalSessionManager(workspace, func(kind, _ string, _ map[string]interface{}) {
		if kind == "terminal_session_abandoned" {
			close(abandonStarted)
			<-releaseAbandon
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := WithTerminalTaskID(context.Background(), "task-close")
	session, err := manager.Start(ctx, TerminalStartRequest{
		RunID: "run-close", OwnerTaskID: "task-close", Mode: TerminalModePTY,
		Command: []string{"sh", "-c", "read line"},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := manager.Close(ctx, session.ID); err != nil {
			t.Error(err)
		}
	})
	broker, err := StartTerminalBroker(workspace, manager)
	if err != nil {
		skipTerminalBrokerSandboxRestriction(t, err)
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = broker.Close() })
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseAbandon) }) }
	t.Cleanup(release)
	conn, err := net.Dial("unix", filepath.Join(workspace, logsDir, terminalBrokerSocket))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if err := json.NewEncoder(conn).Encode(terminalBrokerRequest{Action: "attach", SessionID: session.ID}); err != nil {
		t.Fatal(err)
	}
	var attached terminalBrokerResponse
	if err := json.NewDecoder(conn).Decode(&attached); err != nil || attached.Error != "" || attached.LeaseID == "" {
		t.Fatalf("attach = %+v, err=%v", attached, err)
	}
	const callers = 8
	closed := make(chan error, callers)
	for range callers {
		go func() { closed <- broker.Close() }()
	}
	select {
	case <-abandonStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not interrupt the attached client")
	}
	select {
	case err := <-closed:
		t.Fatalf("Close returned while the handler was still persisting its lease: %v", err)
	default:
	}
	release()
	for range callers {
		select {
		case err := <-closed:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("Close did not finish after lease persistence")
		}
	}
	sessions, err := manager.List(ctx, "")
	if err != nil || len(sessions) != 1 || sessions[0].Controller != TerminalControllerNone || sessions[0].LeaseID != "" {
		t.Fatalf("lease after Close = %+v, err=%v", sessions, err)
	}
}
