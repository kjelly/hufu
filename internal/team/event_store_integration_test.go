package team

import (
	"encoding/json"
	"testing"
)

func TestDualWriteEventStoreIntegration(t *testing.T) {
	dir := t.TempDir()
	session := NewSession()

	es, err := NewEventStore(dir, "run-test", "sess-test")
	if err != nil {
		t.Fatalf("NewEventStore error: %v", err)
	}
	defer es.Close()

	// Record user entry via dual write helper
	RecordSessionUserMessage(session, es, "Hello agent")
	if len(session.Entries) != 1 {
		t.Fatalf("expected 1 session entry, got %d", len(session.Entries))
	}

	// Record assistant entry via dual write helper
	RecordSessionAssistantMessage(session, es, "Hello user")
	if len(session.Entries) != 2 {
		t.Fatalf("expected 2 session entries, got %d", len(session.Entries))
	}

	events, err := es.ReadEvents()
	if err != nil {
		t.Fatalf("ReadEvents error: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("expected 2 events, got %d", len(events))
	}
	if events[0].Type != "user_message_added" {
		t.Errorf("unexpected event type 0: %s", events[0].Type)
	}
	if events[1].Type != "assistant_message_added" {
		t.Errorf("unexpected event type 1: %s", events[1].Type)
	}

	var p1 map[string]string
	_ = json.Unmarshal(events[0].Payload, &p1)
	if p1["content"] != "Hello agent" {
		t.Errorf("unexpected content 0: %s", p1["content"])
	}
}

func TestCheckpointTaskEventsEmission(t *testing.T) {
	dir := t.TempDir()
	teamSession := &TeamSession{Workspace: dir}
	coord := &Coordinator{
		session:     teamSession,
		sessionData: NewSession(),
		taskTracker: NewTaskTracker(),
	}

	coord.initEventStore()
	if coord.EventStore() == nil {
		t.Fatalf("EventStore is nil")
	}
	defer coord.EventStore().Close()

	// Add batch of tasks
	items := coord.taskTracker.TodoList().AddBatch([]TodoSpec{
		{Agent: "worker1", Desc: "Task 1"},
	})
	coord.taskTracker.TodoList().UpdateStatus(items[0].ID, TaskDone, "completed task 1")

	coord.saveCheckpoint()

	events, err := coord.EventStore().ReadEvents()
	if err != nil {
		t.Fatalf("ReadEvents failed: %v", err)
	}

	if len(events) == 0 {
		t.Fatalf("expected task events in EventStore, got 0")
	}

	// Verify reducer reconstructs completed task
	todos := ReduceToTodoList(events)
	if len(todos) != 1 {
		t.Fatalf("expected 1 todo reconstructed, got %d", len(todos))
	}
	if todos[0].ID != items[0].ID || todos[0].Status != TaskDone {
		t.Errorf("reconstructed todo mismatch: %+v", todos[0])
	}
}
