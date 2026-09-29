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

func TestExecutionEventExporterMapsCanonicalRunEvents(t *testing.T) {
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
	workspace := t.TempDir()
	if err := ExportExecutionEvents(workspace, events); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(workspace, logsDir, eventStoreExecutionEventsFile))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 4 || !strings.Contains(lines[1], `"status":"in_progress"`) || !strings.Contains(lines[1], `"invocation_id":"inv-1"`) || !strings.Contains(lines[3], `"evidence_manifest_hash":"manifest-1"`) {
		t.Fatalf("exported events = %s", data)
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
	projected := projectedExecutionEvents(events)
	if got, want := len(projected), 5; got != want {
		t.Fatalf("projected event count = %d, want %d: %#v", got, want, projected)
	}
	if projected[1].Attempt != 1 || projected[3].Attempt != 2 || projected[4].Attempt != 2 {
		t.Fatalf("retry attempts = %#v, want first attempt 1 and retry attempt 2", projected)
	}
}

// The legacy logger numbers attempts with the in-dispatch counter that the
// attempt-starting task_started event carries as dispatch_attempt, and records
// a status again when the task re-enters it after another status. The
// projection must do the same, or parity reports a mismatch for every retry
// and every approved --plan execution.
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
			for _, projected := range projectedExecutionEvents(tc.events) {
				got = append(got, fmt.Sprintf("%s/%s/%d", projected.Status, projected.TaskID, projected.Attempt))
			}
			if strings.Join(got, " ") != strings.Join(tc.want, " ") {
				t.Fatalf("projected = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestExecutionEventExporterTerminalOutcomeParity(t *testing.T) {
	cases := []struct {
		name       string
		runEvents  []RunEvent
		legacyLogs []ExecutionEvent
	}{
		{
			name: "completed_run_with_passed_acceptance",
			runEvents: []RunEvent{
				{Type: string(EventRunStarted), RunID: "run-1", TaskID: "", Actor: "coordinator", Payload: []byte(`{"team":"alpha"}`)},
				{Type: string(EventTaskStarted), RunID: "run-1", TaskID: "1", Actor: "worker", Payload: []byte(`{"id":"1","agent":"worker","attempt":1}`)},
				{Type: string(EventTaskCompleted), RunID: "run-1", TaskID: "1", Actor: "worker", Payload: []byte(`{"id":"1","agent":"worker","attempt":1}`)},
				{Type: string(EventRunFinished), RunID: "run-1", TaskID: "", Actor: "coordinator", Payload: []byte(`{"team":"alpha","outcome":"completed","acceptance_state":"passed","evidence_manifest":{"manifest_hash":"hash-123"}}`)},
			},
			legacyLogs: []ExecutionEvent{
				{Status: "run_started", RunID: "run-1", TaskID: "", Agent: "coordinator", Team: "alpha"},
				{Status: "in_progress", RunID: "run-1", TaskID: "1", Agent: "worker", Attempt: 1},
				{Status: "done", RunID: "run-1", TaskID: "1", Agent: "worker", Attempt: 1},
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
			legacyLogs: []ExecutionEvent{
				{Status: "run_started", RunID: "run-2", TaskID: "", Agent: "coordinator"},
				{Status: "in_progress", RunID: "run-2", TaskID: "1", Agent: "worker", Attempt: 1},
				{Status: "error", RunID: "run-2", TaskID: "1", Agent: "worker", Attempt: 1},
				{Status: "in_progress", RunID: "run-2", TaskID: "1", Agent: "worker", Attempt: 2},
				{Status: "error", RunID: "run-2", TaskID: "1", Agent: "worker", Attempt: 2},
				{Status: "run_finished", RunID: "run-2", TaskID: "", Agent: "coordinator", Outcome: RunOutcomeBlocked},
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
			legacyLogs: []ExecutionEvent{
				{Status: "run_started", RunID: "run-3", TaskID: "", Agent: "coordinator"},
				{Status: "in_progress", RunID: "run-3", TaskID: "1", Agent: "worker", Attempt: 1},
				{Status: "error", RunID: "run-3", TaskID: "1", Agent: "worker", Attempt: 1},
				{Status: "run_finished", RunID: "run-3", TaskID: "", Agent: "coordinator", Outcome: RunOutcomeFailed, AcceptanceState: AcceptanceFailed},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var exported []ExecutionEvent
			for _, re := range tc.runEvents {
				if ee, ok := ExecutionEventFromRunEvent(re); ok {
					exported = append(exported, ee)
				}
			}
			if err := CompareExecutionEventsParity(tc.legacyLogs, exported); err != nil {
				t.Fatalf("parity check failed for %s: %v", tc.name, err)
			}
		})
	}
}

func TestCompareExecutionEventsParityAllowsConcurrentTaskInterleaving(t *testing.T) {
	event := func(status, taskID string) ExecutionEvent {
		return ExecutionEvent{Status: status, RunID: "run-1", TaskID: taskID, Agent: "worker", Attempt: 1}
	}
	legacy := []ExecutionEvent{
		{Status: "run_started", RunID: "run-1", Agent: "coordinator"},
		event("in_progress", "3"),
		event("in_progress", "4"),
		event("in_progress", "2"),
		event("verifying", "3"),
		event("done", "3"),
		event("verifying", "2"),
		event("done", "2"),
		event("verifying", "4"),
		event("done", "4"),
		{Status: "run_finished", RunID: "run-1", Agent: "coordinator", Outcome: RunOutcomeCompleted},
	}
	exported := []ExecutionEvent{
		{Status: "run_started", RunID: "run-1", Agent: "coordinator"},
		event("in_progress", "3"),
		event("in_progress", "2"),
		event("in_progress", "4"),
		event("verifying", "3"),
		event("done", "3"),
		event("verifying", "4"),
		event("done", "4"),
		event("verifying", "2"),
		event("done", "2"),
		{Status: "run_finished", RunID: "run-1", Agent: "coordinator", Outcome: RunOutcomeCompleted},
	}

	if err := CompareExecutionEventsParity(legacy, exported); err != nil {
		t.Fatalf("independent task interleaving should have parity: %v", err)
	}
}

func TestCompareExecutionEventsParityRejectsTaskLifecycleDivergence(t *testing.T) {
	event := func(status, taskID string, attempt int) ExecutionEvent {
		return ExecutionEvent{Status: status, RunID: "run-1", TaskID: taskID, Agent: "worker", Attempt: attempt}
	}
	tests := []struct {
		name     string
		legacy   []ExecutionEvent
		exported []ExecutionEvent
		want     string
	}{
		{
			name: "within-task status order",
			legacy: []ExecutionEvent{
				event("in_progress", "1", 1),
				event("verifying", "1", 1),
				event("done", "1", 1),
			},
			exported: []ExecutionEvent{
				event("in_progress", "1", 1),
				event("done", "1", 1),
				event("verifying", "1", 1),
			},
			want: "status mismatch",
		},
		{
			name: "retry order",
			legacy: []ExecutionEvent{
				event("in_progress", "1", 1),
				event("error", "1", 1),
				event("in_progress", "1", 2),
			},
			exported: []ExecutionEvent{
				event("in_progress", "1", 2),
				event("error", "1", 1),
				event("in_progress", "1", 1),
			},
			want: "attempt mismatch",
		},
		{
			name: "missing task replaced by duplicate",
			legacy: []ExecutionEvent{
				event("in_progress", "1", 1),
				event("done", "1", 1),
				event("in_progress", "2", 1),
				event("done", "2", 1),
			},
			exported: []ExecutionEvent{
				event("in_progress", "1", 1),
				event("done", "1", 1),
				event("in_progress", "1", 1),
				event("done", "1", 1),
			},
			want: "task_id mismatch",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := CompareExecutionEventsParity(tt.legacy, tt.exported)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("CompareExecutionEventsParity() error = %v, want substring %q", err, tt.want)
			}
		})
	}
}

func TestCompareExecutionEventsParityRejectsRunBoundaryMovement(t *testing.T) {
	legacy := []ExecutionEvent{
		{Status: "run_started", RunID: "run-1"},
		{Status: "in_progress", RunID: "run-1", TaskID: "1", Attempt: 1},
		{Status: "run_finished", RunID: "run-1"},
	}
	exported := []ExecutionEvent{
		{Status: "run_started", RunID: "run-1"},
		{Status: "run_finished", RunID: "run-1"},
		{Status: "in_progress", RunID: "run-1", TaskID: "1", Attempt: 1},
	}

	err := CompareExecutionEventsParity(legacy, exported)
	if err == nil || !strings.Contains(err.Error(), "position mismatch") {
		t.Fatalf("CompareExecutionEventsParity() error = %v, want run boundary position mismatch", err)
	}
}

func TestExportAndVerifyExecutionEvents_EndToEndParityAndForcedMismatch(t *testing.T) {
	workspace := t.TempDir()
	runID := "run-test-123"

	// 1. Create legacy execution event logger and write events
	logger, err := newExecutionEventLogger(workspace)
	if err != nil {
		t.Fatal(err)
	}
	logger.append(ExecutionEvent{Status: "run_started", RunID: runID, Team: "team-alpha", Agent: "coordinator"})
	logger.append(ExecutionEvent{Status: "in_progress", RunID: runID, TaskID: "1", Agent: "worker", Attempt: 1})
	logger.append(ExecutionEvent{Status: "done", RunID: runID, TaskID: "1", Agent: "worker", Attempt: 1})
	logger.append(ExecutionEvent{Status: "run_finished", RunID: runID, Team: "team-alpha", Agent: "coordinator", Outcome: RunOutcomeCompleted, AcceptanceState: AcceptancePassed, EvidenceManifestHash: "ev-hash-1"})
	logger.close()

	// 2. Canonical events in EventStore
	canonicalEvents := []RunEvent{
		{Type: string(EventRunStarted), RunID: runID, Actor: "coordinator", Payload: []byte(`{"team":"team-alpha"}`)},
		{Type: string(EventTaskStarted), RunID: runID, TaskID: "1", Actor: "worker", Payload: []byte(`{"id":"1","agent":"worker","attempt":1}`)},
		{Type: string(EventTaskCompleted), RunID: runID, TaskID: "1", Actor: "worker", Payload: []byte(`{"id":"1","agent":"worker","attempt":1}`)},
		{Type: string(EventRunFinished), RunID: runID, Actor: "coordinator", Payload: []byte(`{"team":"team-alpha","outcome":"completed","acceptance_state":"passed","evidence_manifest":{"manifest_hash":"ev-hash-1"}}`)},
	}

	// 3. Export and verify parity -> should succeed
	parity, err := ExportAndVerifyExecutionEvents(workspace, runID, canonicalEvents)
	if err != nil || !parity {
		t.Fatalf("expected parity to succeed, got parity=%v, err=%v", parity, err)
	}

	// Verify exported shadow file was written
	shadowPath := filepath.Join(workspace, logsDir, eventStoreExecutionEventsFile)
	if _, err := os.Stat(shadowPath); err != nil {
		t.Fatalf("shadow file was not written: %v", err)
	}

	// 4. Force mismatch: add divergent canonical event
	divergentCanonical := append(canonicalEvents, RunEvent{
		Type:    string(EventTaskCompleted),
		RunID:   runID,
		TaskID:  "extra-task",
		Actor:   "worker",
		Payload: []byte(`{"id":"extra-task","agent":"worker","attempt":1}`),
	})

	mismatchParity, mismatchErr := ExportAndVerifyExecutionEvents(workspace, runID, divergentCanonical)
	if mismatchErr == nil || mismatchParity {
		t.Fatalf("expected parity mismatch, got parity=%v, err=%v", mismatchParity, mismatchErr)
	}
	if !strings.Contains(mismatchErr.Error(), "length mismatch") {
		t.Fatalf("unexpected mismatch error: %v", mismatchErr)
	}
}

func TestExecutionEvents_ProductionBeginAndFinalizeParity(t *testing.T) {
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
	if c.executionEvents == nil {
		t.Fatal("expected legacy executionEvents logger to be non-nil")
	}
	if c.eventStore == nil {
		t.Fatal("expected EventStore to be non-nil")
	}

	c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "test task"}})
	todoID := "1"
	c.recordExecutionEvent(todoID, "worker", 1, "in_progress", "model-1", 0, ExecutionUsage{})
	if err := c.CommitTaskTransition(context.Background(), todoID, TaskPending, TaskInProgress, "", "", nil); err != nil {
		t.Fatal(err)
	}

	c.recordExecutionEvent(todoID, "worker", 1, "done", "model-1", 0, ExecutionUsage{})
	if err := c.CommitTaskTransition(context.Background(), todoID, TaskInProgress, TaskDone, "task completed", "", nil); err != nil {
		t.Fatal(err)
	}

	c.SetLastRunResult(&RunResult{
		Outcome:       RunOutcomeCompleted,
		GoalSatisfied: true,
	})
	closer()

	if c.dualWriteFailures.Load() > 0 {
		t.Fatalf("expected 0 dual write failures, got %d", c.dualWriteFailures.Load())
	}
	shadowPath := filepath.Join(workspace, logsDir, eventStoreExecutionEventsFile)
	if _, err := os.Stat(shadowPath); err != nil {
		t.Fatalf("shadow file not found: %v", err)
	}
}

// TestSkippedTasksKeepExecutionEventParity pins the legacy stream to the
// export for tasks that end skipped: the export projects task_skipped, so the
// legacy logger must record the skip too, at the attempt the export reports.
func TestSkippedTasksKeepExecutionEventParity(t *testing.T) {
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
			c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "test task"}})
			ctx := context.Background()
			if tt.attempt > 0 {
				c.recordExecutionEvent("1", "worker", tt.attempt, "in_progress", "model-1", 0, ExecutionUsage{})
				if err := c.CommitTaskTransition(ctx, "1", TaskPending, TaskInProgress, "", "", attemptStartMetadata(tt.attempt)); err != nil {
					t.Fatal(err)
				}
				c.recordExecutionEvent("1", "worker", tt.attempt, "planned", "model-1", 0, ExecutionUsage{})
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
				t.Fatalf("dual-write failures = %d, want 0", failures)
			}
			legacy, err := ReadExecutionEvents(filepath.Join(workspace, logsDir, executionEventsFile))
			if err != nil {
				t.Fatal(err)
			}
			var skipped []ExecutionEvent
			for _, event := range legacy {
				if event.Status == string(TaskSkipped) {
					skipped = append(skipped, event)
				}
			}
			wantAttempt := max(tt.attempt, 1)
			if len(skipped) != 1 || skipped[0].TaskID != "1" || skipped[0].Attempt != wantAttempt {
				t.Fatalf("legacy skipped events = %#v, want one for task 1 at attempt %d", skipped, wantAttempt)
			}
		})
	}
}
