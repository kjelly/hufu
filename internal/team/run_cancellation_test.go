package team

import (
	"encoding/json"
	"testing"
	"time"
)

func TestRecordRunCancellationPersistsTypedCauseOnce(t *testing.T) {
	workspace := t.TempDir()
	store, err := NewEventStore(workspace, "run-cancel", "session-cancel")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	coordinator := &Coordinator{
		eventStore: store, executionRunID: "run-cancel", terminalLifecycleRunID: "run-cancel",
		session: &TeamSession{Workspace: workspace},
	}

	for range 2 {
		if err := coordinator.RecordRunCancellation(RunCancellationGracefulTimeout, "runtime_watchdog", 45*time.Minute); err != nil {
			t.Fatal(err)
		}
	}
	events, err := store.ReadEvents()
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Type != string(EventRunCancellationRequested) {
		t.Fatalf("cancellation events = %#v", events)
	}
	var payload RunCancellationRequestedPayload
	if err := json.Unmarshal(events[0].Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.ReasonCode != string(RunCancellationGracefulTimeout) || payload.Source != "runtime_watchdog" || payload.GracefulTimeoutMillis != (45*time.Minute).Milliseconds() || payload.Status != "cancellation_requested" {
		t.Fatalf("cancellation payload = %#v", payload)
	}
}

func TestRecordRunCancellationDistinguishesOperatorForceQuit(t *testing.T) {
	workspace := t.TempDir()
	store, err := NewEventStore(workspace, "run-force", "session-force")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	coordinator := &Coordinator{
		eventStore: store, executionRunID: "run-force", terminalLifecycleRunID: "run-force",
		session: &TeamSession{Workspace: workspace},
	}
	if err := coordinator.RecordRunCancellation(RunCancellationOperatorForce, "operator_sigint", 0); err != nil {
		t.Fatal(err)
	}
	events, err := store.ReadEvents()
	if err != nil {
		t.Fatal(err)
	}
	var payload RunCancellationRequestedPayload
	if len(events) != 1 {
		t.Fatalf("cancellation events = %#v", events)
	}
	if err := json.Unmarshal(events[0].Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.ReasonCode != string(RunCancellationOperatorForce) || payload.Source != "operator_sigint" || payload.GracefulTimeoutMillis != 0 {
		t.Fatalf("operator cancellation payload = %#v", payload)
	}
}
