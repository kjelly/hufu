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

func TestReduceToTodoList_ReconstructsTypedVerificationSpec(t *testing.T) {
	payload, err := json.Marshal(map[string]any{
		"id":          "typed-verify",
		"desc":        "write verified report",
		"status":      "pending",
		"verify_spec": VerificationSpec{Type: VerifyJSONAssert, Path: "report.json", Assertions: []JSONAssertion{{Path: "status", Equals: "ok"}}},
	})
	if err != nil {
		t.Fatal(err)
	}

	todos := ReduceToTodoList([]RunEvent{{Type: "task_created", TaskID: "typed-verify", Payload: payload}})
	if len(todos) != 1 || todos[0].VerifySpec == nil {
		t.Fatalf("typed verifier was lost during event reduction: %#v", todos)
	}
	got := todos[0].VerifySpec
	if got.Type != VerifyJSONAssert || got.Path != "report.json" || len(got.Assertions) != 1 || got.Assertions[0].Path != "status" || got.Assertions[0].Equals != "ok" {
		t.Fatalf("reduced typed verifier = %#v", got)
	}
}

func TestReducersDeduplicateAndDoNotReopenTerminalTask(t *testing.T) {
	completed := RunEvent{
		ID: "evt-completed", IdempotencyKey: "task-1:done:1", Type: string(EventTaskCompleted), TaskID: "task-1",
		Payload: []byte(`{"id":"task-1","status":"done","output":"durable"}`),
	}
	lateStarted := RunEvent{
		ID: "evt-started", Type: string(EventTaskStarted), TaskID: "task-1",
		Payload: []byte(`{"id":"task-1","status":"in_progress"}`),
	}
	todos := ReduceToTodoList([]RunEvent{completed, completed, lateStarted})
	if len(todos) != 1 || todos[0].Status != TaskDone || todos[0].Output != "durable" {
		t.Fatalf("terminal task was not deterministic: %#v", todos)
	}
	first := ReduceToSessionData([]RunEvent{completed, lateStarted})
	second := ReduceToSessionData([]RunEvent{completed, lateStarted})
	if got, want := first.Tasks[0].Status, second.Tasks[0].Status; got != want {
		t.Fatalf("replay was nondeterministic: %s != %s", got, want)
	}
}

func TestReducersRestoreVerifyingTask(t *testing.T) {
	events := []RunEvent{
		{Type: string(EventTaskCreated), TaskID: "verify-1", Payload: []byte(`{"id":"verify-1","status":"pending"}`)},
		{Type: string(EventTaskStarted), TaskID: "verify-1", Payload: []byte(`{"id":"verify-1","status":"in_progress"}`)},
		{Type: string(EventTaskVerifying), TaskID: "verify-1", Payload: []byte(`{"id":"verify-1","status":"verifying"}`)},
	}
	todos := ReduceToTodoList(events)
	if len(todos) != 1 || todos[0].Status != TaskVerifying {
		t.Fatalf("verifying state was not replayed: %#v", todos)
	}
}

func TestReducersRestoreRuntimeTaskContract(t *testing.T) {
	item := &TodoItem{
		ID: "runtime-1", Phase: PhaseExecute, PlanTaskID: "plan-1", ContractID: "contract-1", ContractHash: "hash", ContractRevision: 2,
		Agent: "worker", Desc: "apply action", Status: TaskInProgress, Detail: "running", Model: "model-1",
		Skills: []string{"build"}, InjectedSkills: []string{"team-context"}, LoadedSkills: []string{"build"}, Source: TaskSourceCoordinator, ParentID: "parent-1",
		OnFailure: "repair-1", SideEffect: SideEffectExternalWrite, Recovery: RecoveryReconcile, RecoveryState: "awaiting_reconcile",
		RuntimeError: &ExecutionError{Category: "provider_failed"}, Resolution: &TaskResolution{Status: "reconciled", ResolvedBy: "operator"},
		DiagnosticHints: []string{"use reconciliation evidence"}, LastOperation: "deploy", Execution: ExecutionContract{Kind: ExecutionKindExternal},
	}
	payload, err := json.Marshal(taskTransitionPayload(item))
	if err != nil {
		t.Fatal(err)
	}
	todos := ReduceToTodoList([]RunEvent{{Type: string(EventTaskStarted), TaskID: item.ID, Payload: payload}})
	if len(todos) != 1 {
		t.Fatalf("replayed task count = %d", len(todos))
	}
	got := todos[0]
	if got.Phase != item.Phase || got.ContractID != item.ContractID || got.ContractHash != item.ContractHash || got.ContractRevision != item.ContractRevision || got.Model != item.Model || got.ParentID != item.ParentID || got.OnFailure != item.OnFailure || got.RecoveryState != item.RecoveryState || got.RuntimeError == nil || got.Resolution == nil || got.LastOperation != item.LastOperation || got.Detail != item.Detail {
		t.Fatalf("runtime task contract was not replayed: %#v", got)
	}
	if len(got.Skills) != 1 || len(got.DiagnosticHints) != 1 || got.Execution.Kind != ExecutionKindExternal {
		t.Fatalf("runtime task slices/contract were not replayed: %#v", got)
	}
}
