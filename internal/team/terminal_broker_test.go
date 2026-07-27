package team

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestTerminalBrokerAttachStreamsInputAndOutput(t *testing.T) {
	workspace := t.TempDir()
	manager, err := NewTerminalSessionManager(workspace, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx := WithTerminalTaskID(context.Background(), "task-broker")
	session, err := manager.Start(ctx, TerminalStartRequest{
		RunID: "run-broker", OwnerTaskID: "task-broker", Mode: TerminalModePTY,
		Command: []string{"sh", "-c", `read line; printf 'echo:%s' "$line"`},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = manager.Close(ctx, session.ID) }()

	broker, err := StartTerminalBroker(workspace, manager)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = broker.Close() }()

	conn, err := net.Dial("unix", filepath.Join(workspace, logsDir, terminalBrokerSocket))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	enc := json.NewEncoder(conn)
	dec := json.NewDecoder(bufio.NewReader(conn))
	if err := enc.Encode(terminalBrokerRequest{Action: "attach", SessionID: session.ID}); err != nil {
		t.Fatal(err)
	}
	var attached terminalBrokerResponse
	if err := dec.Decode(&attached); err != nil {
		t.Fatal(err)
	}
	if attached.Error != "" || attached.LeaseID == "" {
		t.Fatalf("attach response = %+v", attached)
	}
	if err := enc.Encode(terminalBrokerRequest{Action: "write", LeaseID: attached.LeaseID, Data: "ok\n"}); err != nil {
		t.Fatal(err)
	}
	var wrote terminalBrokerResponse
	if err := dec.Decode(&wrote); err != nil || wrote.Error != "" {
		t.Fatalf("write response = %+v, err=%v", wrote, err)
	}
	deadline := time.Now().Add(time.Second)
	for {
		if err := enc.Encode(terminalBrokerRequest{Action: "read", LeaseID: attached.LeaseID}); err != nil {
			t.Fatal(err)
		}
		var read terminalBrokerResponse
		if err := dec.Decode(&read); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(read.Screen, "echo:ok") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("screen never contained child output: %+v", read)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestTerminalBrokerRejectsConcurrentAttach(t *testing.T) {
	workspace := t.TempDir()
	manager, err := NewTerminalSessionManager(workspace, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx := WithTerminalTaskID(context.Background(), "task-concurrent")
	session, err := manager.Start(ctx, TerminalStartRequest{
		RunID: "run-concurrent", OwnerTaskID: "task-concurrent", Mode: TerminalModePTY,
		Command: []string{"sh", "-c", "sleep 5"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = manager.Close(ctx, session.ID) }()
	broker, err := StartTerminalBroker(workspace, manager)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = broker.Close() }()

	attach := func() (net.Conn, terminalBrokerResponse) {
		conn, err := net.Dial("unix", filepath.Join(workspace, logsDir, terminalBrokerSocket))
		if err != nil {
			t.Fatal(err)
		}
		if err := json.NewEncoder(conn).Encode(terminalBrokerRequest{Action: "attach", SessionID: session.ID}); err != nil {
			t.Fatal(err)
		}
		var response terminalBrokerResponse
		if err := json.NewDecoder(conn).Decode(&response); err != nil {
			t.Fatal(err)
		}
		return conn, response
	}
	conn1, first := attach()
	defer conn1.Close()
	if first.Error != "" {
		t.Fatalf("first attach = %+v", first)
	}
	conn2, second := attach()
	defer conn2.Close()
	if !strings.Contains(second.Error, "already controlled by a user") {
		t.Fatalf("second attach = %+v, want conflict", second)
	}
}
