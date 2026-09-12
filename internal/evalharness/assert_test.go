package evalharness

import (
	"encoding/json"
	"testing"

	"github.com/kjelly/hufu/internal/team"
)

func TestEvalEventAssertionOrdering(t *testing.T) {
	events := []team.StatusEvent{
		{Type: "start", Agent: "coordinator"},
		{Type: "step"},
		{Type: "todos_updated"},
		{Type: "start", Agent: "worker"},
		{Type: "done", Agent: "worker"},
		{Type: "done", Agent: "coordinator"},
	}

	if findings := assertStatusEvents(EventsExpect{Order: []string{"start", "todos_updated", "done"}}, events, nil); len(findings) != 0 {
		t.Errorf("expected in-order subsequence to pass, got findings: %+v", findings)
	}
	if findings := assertStatusEvents(EventsExpect{}, events, nil); len(findings) != 0 {
		t.Errorf("empty order should never produce findings, got: %+v", findings)
	}
	if findings := assertStatusEvents(EventsExpect{Order: []string{"done", "start"}}, events, nil); len(findings) == 0 {
		t.Error("expected reversed order to fail, got no findings")
	}
	if findings := assertStatusEvents(EventsExpect{Order: []string{"start", "budget_exceeded"}}, events, nil); len(findings) == 0 {
		t.Error("expected a never-observed event in the sequence to fail, got no findings")
	}
}

func TestEvalRunStopReasonAssertion(t *testing.T) {
	result := &team.RunResult{Outcome: team.RunOutcomePartial, StopReason: team.StopReasonEvidenceIncomplete}
	if findings := assertRun(ExpectSpec{StopReason: string(team.StopReasonEvidenceIncomplete)}, result, false, nil, nil); len(findings) != 0 {
		t.Fatalf("matching stop reason produced findings: %+v", findings)
	}
	findings := assertRun(ExpectSpec{StopReason: string(team.StopReasonBudgetExceeded)}, result, false, nil, nil)
	if len(findings) != 1 || findings[0].Dimension != "stop-reason" {
		t.Fatalf("mismatched stop reason findings = %+v, want one stop-reason finding", findings)
	}
}

func TestEvalRunTerminalLifecycleAssertion(t *testing.T) {
	expect := ExpectSpec{TerminalLifecycle: new(true)}
	if findings := assertRun(expect, &team.RunResult{}, true, nil, nil); len(findings) != 0 {
		t.Fatalf("confirmed terminal lifecycle produced findings: %+v", findings)
	}
	findings := assertRun(expect, &team.RunResult{}, false, nil, nil)
	if len(findings) != 1 || findings[0].Dimension != "terminal-lifecycle-confirmed" {
		t.Fatalf("unconfirmed terminal lifecycle findings = %+v", findings)
	}
}

func TestEvalTaskRecoveryAssertions(t *testing.T) {
	task := &team.TodoItem{
		SideEffect:        team.SideEffectUnknown,
		ExecutionReceipts: []team.ExecutionReceipt{{Attempt: 1}},
		FailureEvent: &team.FailureEventPayload{
			FailureClass:     team.FailureExecution,
			RetryDisposition: team.ReconcileOnly,
		},
	}
	want := TaskExpect{
		SideEffect:       string(team.SideEffectUnknown),
		Attempts:         new(1),
		RetryDisposition: string(team.ReconcileOnly),
	}
	if findings := assertTasks([]TaskExpect{want}, []*team.TodoItem{task}); len(findings) != 0 {
		t.Fatalf("matching task recovery fields produced findings: %+v", findings)
	}
	want.Attempts = new(2)
	findings := assertTasks([]TaskExpect{want}, []*team.TodoItem{task})
	if len(findings) != 1 || findings[0].Dimension != "tasks[0].attempts" {
		t.Fatalf("mismatched task attempts findings = %+v, want one attempts finding", findings)
	}
}

func TestEvalTaskExecutionTargetAssertion(t *testing.T) {
	task := &team.TodoItem{BackendBinding: &team.BackendBinding{Backend: "ollama", EffectiveTarget: "eval-harness-model"}}
	task.ExecutionTarget.Backend = "ollama"
	task.ExecutionTarget.Model = "eval-harness-model"
	want := TaskExpect{
		ExecutionTarget: "ollama/eval-harness-model",
		BackendBinding:  &BackendBindingExpect{Backend: "ollama", EffectiveTarget: "eval-harness-model"},
	}
	if findings := assertTasks([]TaskExpect{want}, []*team.TodoItem{task}); len(findings) != 0 {
		t.Fatalf("matching execution target produced findings: %+v", findings)
	}
	want.ExecutionTarget = "openai/eval-harness-model"
	findings := assertTasks([]TaskExpect{want}, []*team.TodoItem{task})
	if len(findings) != 1 || findings[0].Dimension != "tasks[0].execution-target" {
		t.Fatalf("mismatched execution target findings = %+v, want one execution-target finding", findings)
	}
}

func TestEvalTaskVerificationAssertion(t *testing.T) {
	tasks := []*team.TodoItem{
		{},
		{VerifyResult: &team.VerificationResult{ExitCode: 0}},
		{VerifyResult: &team.VerificationResult{ExitCode: 1}},
	}
	expect := []TaskExpect{{Verification: "not-run"}, {Verification: "passed"}, {Verification: "failed"}}
	if findings := assertTasks(expect, tasks); len(findings) != 0 {
		t.Fatalf("matching verification states produced findings: %+v", findings)
	}
}

func TestEvalRejectsUnauthorizedBackendFallback(t *testing.T) {
	task := &team.TodoItem{
		BackendBinding:    &team.BackendBinding{Backend: "ollama"},
		ExecutionReceipts: []team.ExecutionReceipt{{Backend: "ollama"}},
	}
	task.ExecutionTarget.Backend = "ollama"
	task.ExecutionTarget.Model = "model"
	if findings := assertNoUnauthorizedFallback([]*team.TodoItem{task}); len(findings) != 0 {
		t.Fatalf("matching execution provenance produced findings: %+v", findings)
	}
	task.ExecutionReceipts[0].Backend = "openai"
	findings := assertNoUnauthorizedFallback([]*team.TodoItem{task})
	if len(findings) != 1 || findings[0].Dimension != "tasks[0].no-unauthorized-fallback.receipts[0].backend" {
		t.Fatalf("fallback mismatch findings = %+v", findings)
	}
}

func TestEvalDurableEventAssertions(t *testing.T) {
	task := &team.TodoItem{ID: "opaque-task"}
	events := []team.RunEvent{{Type: "task_created"}, {Type: "execution_target_migrated"}, {
		Type: "task_completed", Actor: "worker", TaskID: task.ID, Attempt: 2,
		Payload: json.RawMessage(`{"status":"done"}`),
	}}
	want := EventsExpect{
		Required: []string{"execution_target_migrated"},
		Order:    []string{"task_created", "execution_target_migrated", "task_completed"},
		Counts:   []EventCountExpect{{Type: "task_completed", Count: 1}},
		Matches: []EventMatchExpect{{Type: "task_completed", Fields: map[string]any{
			"actor": "worker", "task_index": 0, "attempt": 2, "payload.status": "done",
		}}},
	}
	if findings := assertDurableEvents(want, events, []*team.TodoItem{task}); len(findings) != 0 {
		t.Fatalf("matching durable events produced findings: %+v", findings)
	}
	want.Required = append(want.Required, "missing_event")
	findings := assertDurableEvents(want, events, []*team.TodoItem{task})
	if len(findings) != 1 || findings[0].Dimension != "durable-events.missing_event" {
		t.Fatalf("missing durable event findings = %+v, want one required-event finding", findings)
	}
}

func TestEvalEventCardinalityAndFields(t *testing.T) {
	task := &team.TodoItem{ID: "task-opaque"}
	statusEvents := []team.StatusEvent{
		{Type: "start", Agent: "worker", TodoID: task.ID, ExecutionTarget: "ollama/model", Data: map[string]any{"reason": "dispatch"}},
		{Type: "start", Agent: "worker", TodoID: task.ID},
	}
	expect := EventsExpect{
		Counts: []EventCountExpect{{Type: "start", Count: 2}},
		Matches: []EventMatchExpect{{Type: "start", Fields: map[string]any{
			"agent": "worker", "task_index": 0, "execution_target": "ollama/model", "data.reason": "dispatch",
		}}},
	}
	if findings := assertStatusEvents(expect, statusEvents, []*team.TodoItem{task}); len(findings) != 0 {
		t.Fatalf("matching status event cardinality/fields produced findings: %+v", findings)
	}
	expect.Counts[0].Count = 1
	findings := assertStatusEvents(expect, statusEvents, []*team.TodoItem{task})
	if len(findings) != 1 || findings[0].Dimension != "events.count.start" {
		t.Fatalf("cardinality mismatch findings = %+v", findings)
	}
}

func TestEvalNormalizesOpaqueIDs(t *testing.T) {
	first := "run-20260912T093923.493930048Z-97f23d1c6c58"
	second := "run-20260101T000000.000000000Z-deadbeef0000"
	if got := normalizeOpaqueID(first); got != normalizeOpaqueID(second) {
		t.Errorf("two distinct run IDs normalized to different strings: %q vs %q", normalizeOpaqueID(first), normalizeOpaqueID(second))
	}
	embedded := "run ended before completion (" + first + ")"
	if got := normalizeOpaqueID(embedded); got == embedded {
		t.Errorf("normalizeOpaqueID left the run id untouched in %q", embedded)
	}
	plain := "no opaque id in this string"
	if got := normalizeOpaqueID(plain); got != plain {
		t.Errorf("normalizeOpaqueID modified a string with no run id: got %q, want unchanged %q", got, plain)
	}
}
