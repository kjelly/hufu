package team

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/kjelly/hufu/internal/agent"
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

func TestCoordinatorRunEmitsSessionEvents(t *testing.T) {
	dir := t.TempDir()
	teamSession := &TeamSession{Workspace: dir, Config: agent.TeamConfig{Name: "test-team"}}
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

	coord.addSessionUserMessage("Analyze repository")
	coord.addSessionAssistantMessage("Task completed successfully")

	events, err := coord.EventStore().ReadEvents()
	if err != nil {
		t.Fatalf("ReadEvents error: %v", err)
	}

	hasUserMsg := false
	hasAssistantMsg := false
	for _, e := range events {
		if e.Type == "user_message_added" {
			hasUserMsg = true
		}
		if e.Type == "assistant_message_added" {
			hasAssistantMsg = true
		}
	}
	if !hasUserMsg || !hasAssistantMsg {
		t.Errorf("expected both user and assistant message events, got user=%v assistant=%v", hasUserMsg, hasAssistantMsg)
	}

	// Reconstruct session data using reducer
	sessionProj := ReduceToSessionData(events)
	if len(sessionProj.Entries) != 2 {
		t.Fatalf("expected 2 entries in session projection, got %d", len(sessionProj.Entries))
	}
	if sessionProj.Entries[0].Content != "Analyze repository" || sessionProj.Entries[1].Content != "Task completed successfully" {
		t.Errorf("unexpected projection content: %+v", sessionProj.Entries)
	}
}

func TestDeduplicatedTaskEventsEmission(t *testing.T) {
	dir := t.TempDir()
	teamSession := &TeamSession{Workspace: dir}
	coord := &Coordinator{
		session:                teamSession,
		sessionData:            NewSession(),
		taskTracker:            NewTaskTracker(),
		emittedTaskTransitions: make(map[string]bool),
	}

	coord.initEventStore()
	if coord.EventStore() == nil {
		t.Fatalf("EventStore is nil")
	}
	defer coord.EventStore().Close()

	items := coord.taskTracker.TodoList().AddBatch([]TodoSpec{
		{Agent: "worker1", Desc: "Task 1"},
	})
	coord.taskTracker.TodoList().UpdateStatus(items[0].ID, TaskDone, "completed")

	// Call saveCheckpoint multiple times
	coord.saveCheckpoint()
	coord.saveCheckpoint()
	coord.saveCheckpoint()

	events, err := coord.EventStore().ReadEvents()
	if err != nil {
		t.Fatal(err)
	}

	taskCompletedCount := 0
	for _, e := range events {
		if e.Type == "task_completed" && e.TaskID == items[0].ID {
			taskCompletedCount++
		}
	}

	if taskCompletedCount != 1 {
		t.Errorf("expected exactly 1 task_completed event, got %d", taskCompletedCount)
	}

	// Reconstruct todo list using reducer
	todos := ReduceToTodoList(events)
	if len(todos) != 1 {
		t.Fatalf("expected 1 todo reconstructed, got %d", len(todos))
	}
	if todos[0].ID != items[0].ID || todos[0].Status != TaskDone {
		t.Errorf("reconstructed todo mismatch: %+v", todos[0])
	}
}

func TestResumeAfterWorkerSuccessThenPolicyErrorPreservesCanonicalTaskShadow(t *testing.T) {
	workspace := t.TempDir()
	live := &Coordinator{
		session: &TeamSession{
			Workspace: workspace,
			Config: agent.TeamConfig{
				Name: "resume-parity",
				Delegation: agent.DelegationPolicy{
					NoRedispatchAfterSuccess: []string{"worker"},
				},
			},
		},
		sessionData: NewSession(),
		taskTracker: NewTaskTracker(),
	}
	live.initEventStore()
	if live.EventStore() == nil {
		t.Fatal("EventStore is nil")
	}
	live.taskTracker.TodoList().onChange = live.saveCheckpoint

	items, err := live.CommitTaskCreation(context.Background(), []TodoSpec{{
		Agent: "worker",
		Desc:  "review the persisted work",
	}})
	if err != nil {
		t.Fatalf("CommitTaskCreation: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("created %d tasks, want 1", len(items))
	}
	taskID := items[0].ID
	if err := live.CommitTaskTransition(context.Background(), taskID, TaskPending, TaskInProgress, "worker started", "", nil); err != nil {
		t.Fatalf("start task: %v", err)
	}

	// The worker's submitted result and execution receipt are installed before
	// the terminal transition, so the completed event is its complete replay
	// projection rather than a status-only marker.
	live.storeSubmittedTaskResult(taskID, &TaskResult{
		TaskID:  taskID,
		Attempt: 1,
		Agent:   "worker",
		Status:  TaskResultStatusSuccess,
		Summary: "review completed",
		Details: "no blocking issue found",
	})
	exitCode := 0
	if err := live.taskTracker.TodoList().SetExecutionReceipt(taskID, &ExecutionReceipt{
		RunID:            "worker-run",
		TaskID:           taskID,
		Attempt:          1,
		ModelExecutionID: "worker-execution",
		ExitCode:         &exitCode,
		ProducerID:       "worker",
	}); err != nil {
		t.Fatalf("SetExecutionReceipt: %v", err)
	}
	if err := live.CommitTaskTransition(context.Background(), taskID, TaskInProgress, TaskDone, "worker completed", "review completed", nil); err != nil {
		t.Fatalf("complete task: %v", err)
	}

	// Reproduce the coordinator's next-step policy rejection. It must leave the
	// already-successful task durable and restartable, rather than prompting a
	// recovery mismatch on the next process.
	err = live.validateDelegationPolicy([]TaskDef{{Agent: "worker", Goal: "repeat the review"}})
	if err == nil || !strings.Contains(err.Error(), "may not be redispatched") {
		t.Fatalf("policy error = %v, want no-redispatch rejection", err)
	}

	checkpoint := LoadSession(workspace)
	if checkpoint == nil {
		t.Fatal("session checkpoint was not written")
	}
	events, err := live.EventStore().ReadEvents()
	if err != nil {
		t.Fatalf("ReadEvents: %v", err)
	}
	var completedPayload struct {
		TypedResult       *TaskResult        `json:"typed_result"`
		ExecutionReceipts []ExecutionReceipt `json:"execution_receipts"`
	}
	completedFound := false
	for _, event := range events {
		if event.Type != string(EventTaskCompleted) || event.TaskID != taskID {
			continue
		}
		completedFound = true
		if err := json.Unmarshal(event.Payload, &completedPayload); err != nil {
			t.Fatalf("decode completed event: %v", err)
		}
	}
	if !completedFound || completedPayload.TypedResult == nil || len(completedPayload.ExecutionReceipts) != 1 {
		t.Fatalf("task_completed did not carry the canonical result and receipt: %#v", completedPayload)
	}
	if err := CompareCanonicalProjection(checkpoint, events); err != nil {
		t.Fatalf("checkpoint diverged before restart: %v", err)
	}
	if err := live.EventStore().Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	restarted := &Coordinator{
		session:     live.session,
		sessionData: LoadSession(workspace),
		taskTracker: NewTaskTracker(),
	}
	restarted.initEventStore()
	if restarted.EventStore() == nil {
		t.Fatal("restarted EventStore is nil")
	}
	defer restarted.EventStore().Close()
	if restarted.sessionData.RecoveryRequired {
		t.Fatalf("restart incorrectly requires recovery: %s", restarted.sessionData.RecoveryReason)
	}
	replayedEvents, err := restarted.EventStore().ReadEvents()
	if err != nil {
		t.Fatalf("ReadEvents after restart: %v", err)
	}
	if err := CompareCanonicalProjection(restarted.sessionData, replayedEvents); err != nil {
		t.Fatalf("restart canonical projection diverged: %v", err)
	}
	if len(restarted.sessionData.Tasks) != 1 || restarted.sessionData.Tasks[0].Status != TaskDone {
		t.Fatalf("restarted task projection = %#v, want one done task", restarted.sessionData.Tasks)
	}
	if got := restarted.sessionData.Tasks[0]; got.TypedResult == nil || len(got.ExecutionReceipts) != 1 {
		t.Fatalf("restarted terminal evidence was not preserved: %#v", got)
	}
}

func TestCheckpointDoesNotAdvanceTaskWhenCanonicalEventAppendFails(t *testing.T) {
	workspace := t.TempDir()
	coord := &Coordinator{
		session:     &TeamSession{Workspace: workspace},
		sessionData: NewSession(),
		taskTracker: NewTaskTracker(),
	}
	coord.initEventStore()
	if coord.EventStore() == nil {
		t.Fatal("EventStore is nil")
	}
	coord.taskTracker.TodoList().onChange = coord.saveCheckpoint

	items, err := coord.CommitTaskCreation(context.Background(), []TodoSpec{{Agent: "worker", Desc: "persist safely"}})
	if err != nil {
		t.Fatalf("CommitTaskCreation: %v", err)
	}
	if err := coord.CommitTaskTransition(context.Background(), items[0].ID, TaskPending, TaskInProgress, "started", "", nil); err != nil {
		t.Fatalf("start task: %v", err)
	}
	if err := coord.EventStore().Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// The legacy direct mutation path still reaches saveCheckpoint through the
	// TodoList hook. A closed event store must defer the done checkpoint rather
	// than silently persisting a state replay cannot reproduce.
	coord.taskTracker.TodoList().UpdateStatus(items[0].ID, TaskDone, "worker completed")
	checkpoint := LoadSession(workspace)
	if checkpoint == nil || len(checkpoint.Tasks) != 1 {
		t.Fatalf("checkpoint = %#v, want prior in-progress task", checkpoint)
	}
	if got := checkpoint.Tasks[0].Status; got != TaskInProgress {
		t.Fatalf("checkpoint status = %s, want %s after event append failure", got, TaskInProgress)
	}
}
