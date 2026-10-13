package team

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kjelly/hufu/internal/agent"
)

func TestExecutionEventProjectorMapsCanonicalRunEvents(t *testing.T) {
	manifest := &EvidenceManifest{ManifestHash: "manifest-1"}
	payload, err := json.Marshal(LifecycleEventPayload{Team: "team", Outcome: RunOutcomeCompleted, AcceptanceState: AcceptancePassed, EvidenceManifest: manifest})
	if err != nil {
		t.Fatal(err)
	}
	events := []RunEvent{
		{Type: string(EventRunStarted), InvocationID: "inv-1", RunID: "run-1", Timestamp: "2026-01-01T00:00:00Z", Payload: []byte(`{"team":"team"}`)},
		{Type: string(EventTaskStarted), InvocationID: "inv-1", RunID: "run-1", TaskID: "1", Actor: "worker", Timestamp: "2026-01-01T00:00:01Z", Payload: []byte(`{"id":"1","agent":"worker","attempt":1}`)},
		{Type: string(EventTaskCompleted), InvocationID: "inv-1", RunID: "run-1", TaskID: "1", Actor: "worker", Timestamp: "2026-01-01T00:00:02Z", Payload: []byte(`{"id":"1","agent":"worker","attempt":1}`)},
		{Type: string(EventRunFinished), InvocationID: "inv-1", RunID: "run-1", Timestamp: "2026-01-01T00:00:03Z", Payload: payload},
	}
	projected := projectEvents(events)
	if len(projected) != 4 || projected[1].Status != "in_progress" || projected[1].InvocationID != "inv-1" || projected[1].Team != "team" || projected[3].EvidenceManifestHash != "manifest-1" {
		t.Fatalf("projected events = %+v", projected)
	}
}

func TestExecutionEventExporterFallbackKeepsHistoricalAttemptAndAgent(t *testing.T) {
	events := []RunEvent{
		{Type: string(EventTaskStarted), RunID: "run", TaskID: "task", Actor: "worker", Payload: []byte(`{"agent":"worker","dispatch_attempt":1}`)},
		// Historical fallback events had only the control actor, not agent.
		{Type: string(EventExecutionFallbackDecided), RunID: "run", TaskID: "task", Actor: "execution-route", Payload: []byte(`{"from_attempt":1,"from_target":{"backend":"ollama","model":"primary"}}`)},
		{Type: string(EventTaskStarted), RunID: "run", TaskID: "task", Actor: "worker", Payload: []byte(`{"agent":"worker","dispatch_attempt":2}`)},
		{Type: string(EventTaskCompleted), RunID: "run", TaskID: "task", Actor: "worker", Payload: []byte(`{"agent":"worker","dispatch_attempt":2}`)},
	}
	projected := projectEvents(events)
	if len(projected) != 4 || projected[1].Agent != "worker" || projected[1].Status != "error" || projected[1].Attempt != 1 || projected[1].Model != "primary" || projected[2].Attempt != 2 || projected[3].Status != "done" {
		t.Fatalf("fallback projection: %+v", projected)
	}
}

func TestProjectedExecutionEventsCollapsesDuplicateDurableTransitions(t *testing.T) {
	events := []RunEvent{
		{Type: string(EventRunStarted), RunID: "run-1", Payload: []byte(`{"team":"team"}`)},
		{Type: string(EventTaskStarted), RunID: "run-1", TaskID: "1", Actor: "worker", Payload: []byte(`{"id":"1","retries":0}`)},
		{Type: string(EventTaskStarted), RunID: "run-1", TaskID: "1", Actor: "worker", Payload: []byte(`{"id":"1","retries":0}`)},
		{Type: string(EventTaskFailed), RunID: "run-1", TaskID: "1", Actor: "worker", Payload: []byte(`{"id":"1","retries":0}`)},
		{Type: string(EventTaskProtocolIncomplete), RunID: "run-1", TaskID: "1", Actor: "worker", Payload: []byte(`{"id":"1","retries":0}`)},
		{Type: string(EventTaskStarted), RunID: "run-1", TaskID: "1", Actor: "worker", Payload: []byte(`{"id":"1","retries":1}`)},
		{Type: string(EventTaskCompleted), RunID: "run-1", TaskID: "1", Actor: "worker", Payload: []byte(`{"id":"1","retries":1}`)},
	}
	projected := projectEvents(events)
	if got, want := len(projected), 5; got != want {
		t.Fatalf("projected event count = %d, want %d: %#v", got, want, projected)
	}
	if projected[1].Attempt != 1 || projected[3].Attempt != 2 || projected[4].Attempt != 2 {
		t.Fatalf("retry attempts = %#v, want first attempt 1 and retry attempt 2", projected)
	}
}

func TestProtocolIncompleteCompatibilityExportUsesRepairOutcome(t *testing.T) {
	for _, repaired := range []bool{true, false} {
		t.Run(fmt.Sprintf("repaired=%t", repaired), func(t *testing.T) {
			event := func(kind EventType) RunEvent {
				return RunEvent{Type: string(kind), RunID: "run-repair", TaskID: "1", Actor: "worker", Payload: []byte(`{"id":"1","agent":"worker","dispatch_attempt":1}`)}
			}
			events := []RunEvent{event(EventTaskStarted), event(EventTaskProtocolIncomplete), event(EventTaskProtocolIncomplete)}
			want := []ExecutionEvent{{Status: "in_progress", RunID: "run-repair", TaskID: "1", Agent: "worker", Attempt: 1}}
			if repaired {
				events = append(events, event(EventTaskVerifying), event(EventTaskCompleted))
				want = append(want,
					ExecutionEvent{Status: "verifying", RunID: "run-repair", TaskID: "1", Agent: "worker", Attempt: 1},
					ExecutionEvent{Status: "done", RunID: "run-repair", TaskID: "1", Agent: "worker", Attempt: 1})
			} else {
				events = append(events, event(EventTaskBlocked))
				want = append(want, ExecutionEvent{Status: "error", RunID: "run-repair", TaskID: "1", Agent: "worker", Attempt: 1})
			}
			assertProjectedRows(t, projectEvents(events), want)
			if _, mapped := ExecutionEventFromRunEvent(events[1]); mapped {
				t.Fatal("recoverable checkpoint was projected as a legacy terminal event")
			}
			if events[1].Type != string(EventTaskProtocolIncomplete) || events[2].Type != string(EventTaskProtocolIncomplete) {
				t.Fatal("export mutated canonical recovery evidence")
			}
		})
	}
}

// Execution telemetry numbers attempts with the in-dispatch counter that the
// attempt-starting task_started event carries as dispatch_attempt, and records
// a status again when the task re-enters it after another status (every retry
// and every approved --plan execution).
func TestProjectedExecutionEventsFollowDispatchAttempts(t *testing.T) {
	ev := func(eventType, taskID, payload string) RunEvent {
		return RunEvent{Type: eventType, RunID: "run-1", TaskID: taskID, Actor: "worker", Payload: []byte(payload)}
	}
	cases := []struct {
		name   string
		events []RunEvent
		want   []string
	}{
		{
			name: "approved plan re-enters in_progress on the same attempt",
			events: []RunEvent{
				ev(string(EventTaskStarted), "1", `{"id":"1","retries":0,"dispatch_attempt":1}`),
				ev(string(EventTaskStarted), "1", `{"id":"1","retries":0}`),
				ev(string(EventTaskPlanned), "1", `{"id":"1","retries":0}`),
				ev(string(EventTaskPlanned), "1", `{"id":"1","retries":0}`),
				ev(string(EventTaskStarted), "1", `{"id":"1","retries":0,"dispatch_attempt":1}`),
				ev(string(EventTaskVerifying), "1", `{"id":"1","retries":0}`),
				ev(string(EventTaskVerifying), "1", `{"id":"1","retries":0}`),
				ev(string(EventTaskCompleted), "1", `{"id":"1","retries":0}`),
			},
			want: []string{"in_progress/1/1", "planned/1/1", "in_progress/1/1", "verifying/1/1", "done/1/1"},
		},
		{
			name: "in-dispatch retry takes the dispatch attempt number",
			events: []RunEvent{
				ev(string(EventTaskStarted), "1", `{"id":"1","retries":0,"dispatch_attempt":1}`),
				ev(string(EventTaskFailed), "1", `{"id":"1","retries":0}`),
				ev(string(EventTaskStarted), "1", `{"id":"1","retries":0,"dispatch_attempt":2}`),
				ev(string(EventTaskProtocolIncomplete), "1", `{"id":"1","retries":0}`),
				ev(string(EventTaskBlocked), "1", `{"id":"1","retries":0}`),
			},
			want: []string{"in_progress/1/1", "error/1/1", "in_progress/1/2", "error/1/2"},
		},
		{
			name: "a new occurrence restarts the dispatch counter",
			events: []RunEvent{
				ev(string(EventTaskStarted), "1", `{"id":"1","retries":0,"dispatch_attempt":1}`),
				ev(string(EventTaskFailed), "1", `{"id":"1","retries":0}`),
				ev(string(EventTaskStarted), "1", `{"id":"1","retries":1,"dispatch_attempt":1}`),
				ev(string(EventTaskCompleted), "1", `{"id":"1","retries":1}`),
			},
			want: []string{"in_progress/1/1", "error/1/1", "in_progress/1/1", "done/1/1"},
		},
		{
			name: "interleaved tasks collapse duplicates per task",
			events: []RunEvent{
				ev(string(EventTaskStarted), "1", `{"id":"1","retries":0,"dispatch_attempt":1}`),
				ev(string(EventTaskStarted), "2", `{"id":"2","retries":0,"dispatch_attempt":1}`),
				ev(string(EventTaskStarted), "1", `{"id":"1","retries":0}`),
				ev(string(EventTaskPlanned), "2", `{"id":"2","retries":0}`),
				ev(string(EventTaskPlanned), "1", `{"id":"1","retries":0}`),
			},
			want: []string{"in_progress/1/1", "in_progress/2/1", "planned/2/1", "planned/1/1"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got []string
			for _, projected := range projectEvents(tc.events) {
				got = append(got, fmt.Sprintf("%s/%s/%d", projected.Status, projected.TaskID, projected.Attempt))
			}
			if strings.Join(got, " ") != strings.Join(tc.want, " ") {
				t.Fatalf("projected = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestExecutionEventProjectorTerminalOutcomes(t *testing.T) {
	cases := []struct {
		name      string
		runEvents []RunEvent
		want      []ExecutionEvent
	}{
		{
			name: "completed_run_with_passed_acceptance",
			runEvents: []RunEvent{
				{Type: string(EventRunStarted), RunID: "run-1", TaskID: "", Actor: "coordinator", Payload: []byte(`{"team":"alpha"}`)},
				{Type: string(EventTaskStarted), RunID: "run-1", TaskID: "1", Actor: "worker", Payload: []byte(`{"id":"1","agent":"worker","attempt":1}`)},
				{Type: string(EventTaskCompleted), RunID: "run-1", TaskID: "1", Actor: "worker", Payload: []byte(`{"id":"1","agent":"worker","attempt":1}`)},
				{Type: string(EventRunFinished), RunID: "run-1", TaskID: "", Actor: "coordinator", Payload: []byte(`{"team":"alpha","outcome":"completed","acceptance_state":"passed","evidence_manifest":{"manifest_hash":"hash-123"}}`)},
			},
			want: []ExecutionEvent{
				{Status: "run_started", RunID: "run-1", TaskID: "", Agent: "coordinator", Team: "alpha"},
				{Status: "in_progress", RunID: "run-1", TaskID: "1", Agent: "worker", Team: "alpha", Attempt: 1},
				{Status: "done", RunID: "run-1", TaskID: "1", Agent: "worker", Team: "alpha", Attempt: 1},
				{Status: "run_finished", RunID: "run-1", TaskID: "", Agent: "coordinator", Team: "alpha", Outcome: RunOutcomeCompleted, AcceptanceState: AcceptancePassed, EvidenceManifestHash: "hash-123"},
			},
		},
		{
			name: "retry_and_blocked_outcomes",
			runEvents: []RunEvent{
				{Type: string(EventRunStarted), RunID: "run-2", TaskID: "", Actor: "coordinator", Payload: []byte(`{"team":"alpha"}`)},
				{Type: string(EventTaskStarted), RunID: "run-2", TaskID: "1", Actor: "worker", Payload: []byte(`{"id":"1","agent":"worker","attempt":1}`)},
				{Type: string(EventTaskFailed), RunID: "run-2", TaskID: "1", Actor: "worker", Payload: []byte(`{"id":"1","agent":"worker","attempt":1}`)},
				{Type: string(EventTaskStarted), RunID: "run-2", TaskID: "1", Actor: "worker", Payload: []byte(`{"id":"1","agent":"worker","attempt":2}`)},
				{Type: string(EventTaskBlocked), RunID: "run-2", TaskID: "1", Actor: "worker", Payload: []byte(`{"id":"1","agent":"worker","attempt":2}`)},
				{Type: string(EventRunFinished), RunID: "run-2", TaskID: "", Actor: "coordinator", Payload: []byte(`{"team":"alpha","outcome":"blocked"}`)},
			},
			want: []ExecutionEvent{
				{Status: "run_started", RunID: "run-2", TaskID: "", Agent: "coordinator", Team: "alpha"},
				{Status: "in_progress", RunID: "run-2", TaskID: "1", Agent: "worker", Team: "alpha", Attempt: 1},
				{Status: "error", RunID: "run-2", TaskID: "1", Agent: "worker", Team: "alpha", Attempt: 1},
				{Status: "in_progress", RunID: "run-2", TaskID: "1", Agent: "worker", Team: "alpha", Attempt: 2},
				{Status: "error", RunID: "run-2", TaskID: "1", Agent: "worker", Team: "alpha", Attempt: 2},
				{Status: "run_finished", RunID: "run-2", TaskID: "", Agent: "coordinator", Team: "alpha", Outcome: RunOutcomeBlocked},
			},
		},
		{
			name: "cancelled_and_acceptance_failed_outcomes",
			runEvents: []RunEvent{
				{Type: string(EventRunStarted), RunID: "run-3", TaskID: "", Actor: "coordinator", Payload: []byte(`{"team":"alpha"}`)},
				{Type: string(EventTaskStarted), RunID: "run-3", TaskID: "1", Actor: "worker", Payload: []byte(`{"id":"1","agent":"worker","attempt":1}`)},
				{Type: string(EventTaskCancelled), RunID: "run-3", TaskID: "1", Actor: "worker", Payload: []byte(`{"id":"1","agent":"worker","attempt":1}`)},
				{Type: string(EventRunFinished), RunID: "run-3", TaskID: "", Actor: "coordinator", Payload: []byte(`{"team":"alpha","outcome":"failed","acceptance_state":"failed"}`)},
			},
			want: []ExecutionEvent{
				{Status: "run_started", RunID: "run-3", TaskID: "", Agent: "coordinator", Team: "alpha"},
				{Status: "in_progress", RunID: "run-3", TaskID: "1", Agent: "worker", Team: "alpha", Attempt: 1},
				{Status: "error", RunID: "run-3", TaskID: "1", Agent: "worker", Team: "alpha", Attempt: 1},
				{Status: "run_finished", RunID: "run-3", TaskID: "", Agent: "coordinator", Team: "alpha", Outcome: RunOutcomeFailed, AcceptanceState: AcceptanceFailed},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assertProjectedRows(t, projectEvents(tc.runEvents), tc.want)
		})
	}
}

// TestExecutionRunLeavesNoExecutionEventFiles pins that a run records its
// execution telemetry only as canonical events: no execution-event file is
// written, and the run projects from the event store.
func TestExecutionRunLeavesNoExecutionEventFiles(t *testing.T) {
	workspace := t.TempDir()
	c := &Coordinator{
		session: &TeamSession{
			Workspace: workspace,
			Config:    agent.TeamConfig{Name: "test-team"},
		},
		sessionData: NewSession(),
		taskTracker: NewTaskTracker(),
	}

	closer := c.beginExecutionRun()
	if c.eventStore == nil {
		t.Fatal("expected EventStore to be non-nil")
	}
	c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "test task", Model: "model-1"}})
	todoID := "1"
	if err := c.CommitTaskTransition(context.Background(), todoID, TaskPending, TaskInProgress, "", "", nil); err != nil {
		t.Fatal(err)
	}
	if err := c.CommitTaskTransition(context.Background(), todoID, TaskInProgress, TaskDone, "task completed", "", nil); err != nil {
		t.Fatal(err)
	}
	c.SetLastRunResult(&RunResult{
		Outcome:       RunOutcomeCompleted,
		GoalSatisfied: true,
	})
	closer()

	if c.dualWriteFailures.Load() > 0 {
		t.Fatalf("expected 0 event append failures, got %d", c.dualWriteFailures.Load())
	}
	for _, name := range []string{executionEventsFile, "execution-events.event-store.jsonl"} {
		if _, err := os.Stat(filepath.Join(workspace, logsDir, name)); !os.IsNotExist(err) {
			t.Fatalf("%s exists after the run (stat err = %v)", name, err)
		}
	}
	var statuses []string
	for _, row := range workspaceExecutionRows(t, workspace) {
		statuses = append(statuses, row.Status+"/"+row.Team)
	}
	if got, want := strings.Join(statuses, " "), "run_started/test-team in_progress/test-team done/test-team run_finished/test-team"; got != want {
		t.Fatalf("projected statuses = %q, want %q", got, want)
	}
}

// TestSkippedTasksProjectAtTheirLastAttempt pins that a skipped task projects
// one skip row, at the attempt its last dispatch started.
func TestSkippedTasksProjectAtTheirLastAttempt(t *testing.T) {
	tests := []struct {
		name    string
		attempt int // 0 leaves the task pending until it is skipped
	}{
		{name: "pending task skipped at finish"},
		{name: "rejected plan skipped after its first attempt", attempt: 1},
		{name: "rejected plan skipped after a later attempt", attempt: 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			workspace := t.TempDir()
			c := &Coordinator{
				session:     &TeamSession{Workspace: workspace, Config: agent.TeamConfig{Name: "test-team"}},
				sessionData: NewSession(),
				taskTracker: NewTaskTracker(),
			}
			closer := c.beginExecutionRun()
			c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "test task", Model: "model-1"}})
			ctx := context.Background()
			if tt.attempt > 0 {
				if err := c.CommitTaskTransition(ctx, "1", TaskPending, TaskInProgress, "", "", attemptStartMetadata(tt.attempt)); err != nil {
					t.Fatal(err)
				}
				if err := c.CommitTaskTransition(ctx, "1", TaskInProgress, TaskPlanned, "", "", nil); err != nil {
					t.Fatal(err)
				}
			}
			if err := c.commitTaskTransitionFromCurrent(ctx, "1", TaskSkipped, "plan rejected", "", nil); err != nil {
				t.Fatal(err)
			}
			c.SetLastRunResult(&RunResult{Outcome: RunOutcomePartial})
			closer()

			if failures := c.dualWriteFailures.Load(); failures != 0 {
				t.Fatalf("event append failures = %d, want 0", failures)
			}
			var skipped []ExecutionEvent
			for _, event := range workspaceExecutionRows(t, workspace) {
				if event.Status == string(TaskSkipped) {
					skipped = append(skipped, event)
				}
			}
			wantAttempt := max(tt.attempt, 1)
			if len(skipped) != 1 || skipped[0].TaskID != "1" || skipped[0].Attempt != wantAttempt {
				t.Fatalf("projected skipped events = %#v, want one for task 1 at attempt %d", skipped, wantAttempt)
			}
		})
	}
}

func TestSealedPartialRunDoesNotAppendLateTaskSkips(t *testing.T) {
	workspace := t.TempDir()
	c := &Coordinator{
		session:     &TeamSession{Workspace: workspace, Config: agent.TeamConfig{Name: "sealed-partial"}},
		sessionData: NewSession(), taskTracker: NewTaskTracker(),
	}
	c.SetSessionData(c.sessionData)
	closeRun := c.beginExecutionRun()
	item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "unexecuted work"}})[0]
	result := &RunResult{RunID: c.executionRunID, Outcome: RunOutcomePartial, StopReason: StopReasonUnresolvedTasks, UnresolvedTasks: []TaskReference{{ID: item.ID, Agent: item.Agent}}}
	c.FinalizeRun(t.Context(), result, nil)
	if !c.TerminalLifecycleConfirmed() {
		t.Fatal("run boundary was not sealed")
	}
	before := len(mustReadEvents(t, c.eventStore))
	// This is the normal Run/Continue tail after ensureFinished has already
	// committed the partial business result.
	c.finalizeNormalCompletion()
	if len(mustReadEvents(t, c.eventStore)) != before || c.todoItemByID(item.ID).Status != TaskPending {
		t.Fatal("normal completion rewrote the sealed task snapshot")
	}
	closeRun()
	if c.dualWriteFailures.Load() != 0 {
		t.Fatal("terminal task cleanup failed an event append")
	}
	canonical := workspaceExecutionRows(t, workspace)
	if len(canonical) != 2 || canonical[0].Status != "run_started" || canonical[1].Status != "run_finished" {
		t.Fatalf("canonical events=%+v", canonical)
	}
}
