package team

import (
	"encoding/json"
	"testing"

	"github.com/kjelly/hufu/internal/executioncompat"
)

func TestTerminalRunSnapshotPersistsInputsAndAssertionSummaries(t *testing.T) {
	snapshot, err := ResolveRunInputSnapshot([]RunInputDefinition{{
		Name: "scope", Schema: RunInputSchema{Type: "integer"}, Required: true,
	}}, []RunInputAssignment{{Name: "scope", RawValue: []byte("3"), Source: RunInputSourceCLI}}, "run-present", "invocation-1", "team")
	if err != nil {
		t.Fatal(err)
	}
	spec := VerificationSpec{Type: VerifyTaskOutputAssert, WorksetSourceTask: "producer", TaskOutputName: "scope", Assertions: []TaskOutputAssertion{{Pointer: "/requested", Op: "equals_input", Input: "scope"}}}
	verification := &VerificationResult{ExitCode: 0, Spec: &spec, TaskOutputAssertions: []TaskOutputAssertionResult{{SourceTaskID: "producer-1", Output: "scope", Pointer: "/requested", Op: "equals_input", Passed: true}}}
	acceptance := &AcceptanceResult{State: AcceptancePassed, VerificationEvidence: []*VerificationResult{verification}}
	summaries := SummarizeInputBoundAssertions(acceptance)
	if len(summaries) != 1 || summaries[0].State != "passed" || summaries[0].SourceTask != "producer" {
		t.Fatalf("summaries = %#v", summaries)
	}
	result := &RunResult{RunID: "run-present", Outcome: RunOutcomeCompleted, GoalSatisfied: true, Acceptance: acceptance, RunInputs: snapshot, InputBoundAssertions: summaries}
	payload, err := json.Marshal(terminalLifecyclePayload(nil, result))
	if err != nil {
		t.Fatal(err)
	}
	event := RunEvent{SchemaVersion: eventStoreSchemaVersion, ID: "event-1", RunID: result.RunID, SessionID: "session-1", BranchID: "main", Actor: "coordinator", Timestamp: "2026-09-14T00:00:00Z", Type: string(EventRunFinished), Payload: payload}
	if err := ValidateEventPayload(event); err != nil {
		t.Fatalf("ValidateEventPayload: %v", err)
	}
	replayed := ReduceToSessionData([]RunEvent{event})
	if replayed.RunResult == nil || replayed.RunResult.RunInputs == nil || replayed.RunResult.RunInputs.SnapshotHash != snapshot.SnapshotHash || len(replayed.RunResult.InputBoundAssertions) != 1 {
		t.Fatalf("replayed terminal result = %#v", replayed.RunResult)
	}
}

func TestRunInputCompatibilityInspectorCountsBoundAndLegacyTasks(t *testing.T) {
	snapshot, err := ResolveRunInputSnapshot([]RunInputDefinition{{
		Name: "scope", Schema: RunInputSchema{Type: "integer"}, Required: true,
	}}, []RunInputAssignment{{Name: "scope", RawValue: []byte("1"), Source: RunInputSourceCLI}}, "run-compat", "invocation-1", "team")
	if err != nil {
		t.Fatal(err)
	}
	snapshotPayload, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	boundPayload, err := json.Marshal(map[string]any{
		"id": "bound", "status": "done", "run_input_snapshot_id": snapshot.ID, "run_input_snapshot_hash": snapshot.SnapshotHash,
		"materialized_action_payload_hash": "sha256:payload", "bound_inputs": map[string]string{"scope": snapshot.Inputs[0].ValueHash},
		"typed_result": map[string]any{"source": "runtime", "summary": "done", "status": TaskResultStatusSuccess, "runtime_outputs": map[string]any{"scope": 1}},
	})
	if err != nil {
		t.Fatal(err)
	}
	legacyPayload, err := json.Marshal(map[string]any{
		"id": "legacy", "status": "done", "typed_result": map[string]any{"source": "runtime", "summary": "done", "status": TaskResultStatusSuccess, "facts": map[string]any{"scope": 1}},
	})
	if err != nil {
		t.Fatal(err)
	}
	report := &executioncompat.InspectionReport{}
	inspectRunInputCompatibility(report, []RunEvent{
		{Type: string(EventRunInputsResolved), RunID: "run-compat", Payload: snapshotPayload},
		{Type: string(EventTaskCompleted), RunID: "run-compat", TaskID: "bound", Payload: boundPayload},
		{Type: string(EventTaskCompleted), RunID: "run-compat", TaskID: "legacy", Payload: legacyPayload},
	})
	if report.CanonicalRunInputSnapshots != 1 || report.InputBoundTasks != 1 || report.LegacyUnboundRuntimeOutputs != 1 || report.InputBindingConflicts != 0 {
		t.Fatalf("compatibility report = %#v", report)
	}
}

func TestAggregateRunResultsFoldsTypedInputMetrics(t *testing.T) {
	results := []*RunResult{
		{Outcome: RunOutcomeCompleted, GoalSatisfied: true, Acceptance: &AcceptanceResult{State: AcceptancePassed}, Metrics: RunMetrics{
			TypedRunInputsResolved: 1, InputBoundActions: 2, InputBoundAssertionsPassed: 1,
		}},
		{Outcome: RunOutcomePartial, Acceptance: &AcceptanceResult{State: AcceptanceFailed}, Metrics: RunMetrics{
			TypedRunInputsResolved: 2, InputBoundActions: 1, InputBoundAssertionsFailed: 1,
		}},
	}
	metrics := AggregateRunResults(results, nil, RunStats{}).Metrics
	if metrics.TypedRunInputsResolved != 3 || metrics.InputBoundActions != 3 || metrics.InputBoundAssertionsPassed != 1 || metrics.InputBoundAssertionsFailed != 1 {
		t.Fatalf("aggregated typed-input metrics = %#v", metrics)
	}
}
