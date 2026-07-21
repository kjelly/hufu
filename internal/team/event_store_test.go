package team

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestEventStoreAppendAndVerifyHashChain(t *testing.T) {
	dir := t.TempDir()
	es, err := NewEventStore(dir, "run-101", "session-202")
	if err != nil {
		t.Fatalf("NewEventStore failed: %v", err)
	}
	defer es.Close()

	payload1, _ := json.Marshal(map[string]string{"goal": "Build feature X"})
	e1 := RunEvent{
		Type:    "run_started",
		Actor:   "user",
		Payload: payload1,
	}
	if err := es.Append(e1); err != nil {
		t.Fatalf("Append e1 failed: %v", err)
	}

	payload2, _ := json.Marshal(map[string]string{"task_id": "task-1", "desc": "Write code"})
	e2 := RunEvent{
		Type:    "task_created",
		Actor:   "coordinator",
		TaskID:  "task-1",
		Payload: payload2,
	}
	if err := es.Append(e2); err != nil {
		t.Fatalf("Append e2 failed: %v", err)
	}

	events, err := es.ReadEvents()
	if err != nil {
		t.Fatalf("ReadEvents failed: %v", err)
	}

	if len(events) != 2 {
		t.Fatalf("expected 2 events, got %d", len(events))
	}

	if err := es.VerifyHashChain(); err != nil {
		t.Errorf("VerifyHashChain failed: %v", err)
	}
}

func TestEventStoreTamperDetection(t *testing.T) {
	dir := t.TempDir()
	es, err := NewEventStore(dir, "run-101", "session-202")
	if err != nil {
		t.Fatal(err)
	}
	_ = es.Append(RunEvent{Type: "run_started", Actor: "user"})
	_ = es.Append(RunEvent{Type: "run_finished", Actor: "coordinator"})
	_ = es.Close()

	// Tamper with event log file directly
	path := filepath.Join(dir, logsDir, eventStoreFile)
	data, _ := os.ReadFile(path)
	tampered := bytes.Replace(data, []byte("run_started"), []byte("run_hacked"), 1)
	_ = os.WriteFile(path, tampered, 0o644)

	es2, err := OpenEventStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer es2.Close()

	if err := es2.VerifyHashChain(); err == nil {
		t.Errorf("expected hash chain error on tampered file, got nil")
	}
}
