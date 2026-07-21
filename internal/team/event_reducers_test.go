package team

import (
	"encoding/json"
	"testing"
)

func TestReducersReconstructState(t *testing.T) {
	var events []RunEvent

	p1, _ := json.Marshal(map[string]string{"role": "user", "content": "Build website"})
	events = append(events, RunEvent{Type: "user_message_added", Payload: p1, Timestamp: "2026-07-21T06:00:00Z"})

	p2, _ := json.Marshal(map[string]string{"role": "assistant", "content": "Starting build"})
	events = append(events, RunEvent{Type: "assistant_message_added", Payload: p2, Timestamp: "2026-07-21T06:01:00Z"})

	p3, _ := json.Marshal(map[string]interface{}{"id": "1", "description": "Create HTML", "status": "pending"})
	events = append(events, RunEvent{Type: "task_created", TaskID: "1", Payload: p3})

	p4, _ := json.Marshal(map[string]interface{}{"id": "1", "status": "in_progress"})
	events = append(events, RunEvent{Type: "task_started", TaskID: "1", Payload: p4})

	p5, _ := json.Marshal(map[string]interface{}{"id": "1", "status": "done", "output": "HTML created"})
	events = append(events, RunEvent{Type: "task_completed", TaskID: "1", Payload: p5})

	session := ReduceToSessionData(events)
	if len(session.Entries) != 2 {
		t.Fatalf("expected 2 session entries, got %d", len(session.Entries))
	}
	if session.Entries[0].Role != "user" || session.Entries[0].Content != "Build website" {
		t.Errorf("unexpected entry 0: %+v", session.Entries[0])
	}

	todos := ReduceToTodoList(events)
	if len(todos) != 1 {
		t.Fatalf("expected 1 todo item, got %d", len(todos))
	}
	if todos[0].ID != "1" || todos[0].Status != TaskDone || todos[0].Output != "HTML created" {
		t.Errorf("unexpected todo item: %+v", todos[0])
	}
}

func TestReducersEmptyAndMalformedEvents(t *testing.T) {
	session := ReduceToSessionData(nil)
	if session == nil || len(session.Entries) != 0 {
		t.Errorf("expected empty session for nil events")
	}

	todos := ReduceToTodoList(nil)
	if len(todos) != 0 {
		t.Errorf("expected empty todo list for nil events")
	}

	// Malformed event payload
	events := []RunEvent{
		{Type: "user_message_added", Payload: json.RawMessage(`invalid json`)},
		{Type: "task_created", TaskID: "99", Payload: json.RawMessage(`{`)},
	}
	session2 := ReduceToSessionData(events)
	if len(session2.Entries) != 0 {
		t.Errorf("expected 0 entries for malformed payload")
	}
	todos2 := ReduceToTodoList(events)
	if len(todos2) != 1 || todos2[0].ID != "99" {
		t.Errorf("expected 1 todo item created from TaskID")
	}
}
