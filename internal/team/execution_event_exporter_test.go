package team

import (
	"context"
	"encoding/json"
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
		{Type: string(EventRunStarted), RunID: "run-1", Timestamp: "2026-01-01T00:00:00Z", Payload: []byte(`{"team":"team"}`)},
		{Type: string(EventTaskStarted), RunID: "run-1", TaskID: "1", Actor: "worker", Timestamp: "2026-01-01T00:00:01Z", Payload: []byte(`{"id":"1","agent":"worker","attempt":1}`)},
		{Type: string(EventTaskCompleted), RunID: "run-1", TaskID: "1", Actor: "worker", Timestamp: "2026-01-01T00:00:02Z", Payload: []byte(`{"id":"1","agent":"worker","attempt":1}`)},
		{Type: string(EventRunFinished), RunID: "run-1", Timestamp: "2026-01-01T00:00:03Z", Payload: payload},
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
	if len(lines) != 4 || !strings.Contains(lines[1], `"status":"in_progress"`) || !strings.Contains(lines[3], `"evidence_manifest_hash":"manifest-1"`) {
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
