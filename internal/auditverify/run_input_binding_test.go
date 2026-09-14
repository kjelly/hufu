package auditverify

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/kjelly/hufu/internal/team"
)

func TestVerifyRunInputBindingDimensionReplaysAssertionsAndDetectsTampering(t *testing.T) {
	definitions := []team.RunInputDefinition{{Name: "review.scope", Schema: team.RunInputSchema{Type: "object"}, Required: true}}
	snapshot, err := team.ResolveRunInputSnapshot(definitions, []team.RunInputAssignment{{
		Name: "review.scope", RawValue: []byte(`{"count":1,"kind":"last_n"}`), Source: team.RunInputSourceCLI,
	}}, "run-input-audit", "invocation-1", "review")
	if err != nil {
		t.Fatal(err)
	}
	outputs, outputHash, err := team.CanonicalizeRuntimeOutputs(map[string]any{"scope": map[string]any{
		"requested":            map[string]any{"count": 1, "kind": "last_n"},
		"requested_input_hash": snapshot.Inputs[0].ValueHash,
		"satisfied":            true,
	}})
	if err != nil {
		t.Fatal(err)
	}
	bound := map[string]string{"review.scope": snapshot.Inputs[0].ValueHash}
	exitCode := 0
	item := &team.TodoItem{
		ID: "producer-1", PlanTaskID: "produce-workset", Status: team.TaskDone, OccurrenceRevision: 1,
		RunInputSnapshotID: snapshot.ID, RunInputSnapshotHash: snapshot.SnapshotHash,
		MaterializedActionPayloadHash: "sha256:materialized", BoundInputs: bound,
		TypedResult: &team.TaskResult{
			TaskID: "producer-1", Status: team.TaskResultStatusSuccess, Summary: "produced", Source: "runtime",
			RunInputSnapshotID: snapshot.ID, RunInputSnapshotHash: snapshot.SnapshotHash,
			MaterializedActionPayloadHash: "sha256:materialized", BoundInputs: bound,
			RuntimeOutputs: outputs, RuntimeOutputsHash: outputHash,
		},
		ExecutionReceipts: []team.ExecutionReceipt{{
			RunID: "run-input-audit", TaskID: "producer-1", Attempt: 1, ExitCode: &exitCode,
			StartedAt: time.Now().Add(-time.Second), FinishedAt: time.Now(), ActionInvocationID: "action-1", TranscriptRef: "artifact-transcript",
			RunInputSnapshotID: snapshot.ID, RunInputSnapshotHash: snapshot.SnapshotHash,
			MaterializedActionPayloadHash: "sha256:materialized", BoundInputs: bound, RuntimeOutputsHash: outputHash,
		}},
	}
	spec := team.VerificationSpec{
		Type: team.VerifyTaskOutputAssert, WorksetSourceTask: "produce-workset", TaskOutputName: "scope",
		Assertions: []team.TaskOutputAssertion{
			{Pointer: "/requested", Op: "equals_input", Input: "review.scope"},
			{Pointer: "/requested_input_hash", Op: "equals_input_hash", Input: "review.scope"},
			{Pointer: "/satisfied", Op: "equals", Value: true},
		},
	}
	evidence, err := team.ReplayTaskOutputAssertions([]*team.TodoItem{item}, "run-input-audit", snapshot, spec)
	if err != nil {
		t.Fatal(err)
	}
	verification := &team.VerificationResult{ExitCode: 0, Spec: &spec, TaskOutputAssertions: evidence}
	acceptance := &team.AcceptanceResult{State: team.AcceptancePassed, VerificationEvidence: []*team.VerificationResult{verification}}
	result := &team.RunResult{
		RunID: "run-input-audit", RunInputs: snapshot,
		Acceptance: acceptance, InputBoundAssertions: team.SummarizeInputBoundAssertions(acceptance),
	}
	audit := &AuditVerificationResult{RunID: result.RunID}
	if dimension := verifyRunInputBindingDimension(result.RunID, result, []*team.TodoItem{item}, []team.RunInputSnapshot{*snapshot}, audit); dimension.Status != AuditDimensionPass {
		t.Fatalf("dimension = %#v findings=%#v", dimension, audit.Findings)
	}

	item.TypedResult.RuntimeOutputs["scope"].(map[string]any)["satisfied"] = false
	audit = &AuditVerificationResult{RunID: result.RunID}
	dimension := verifyRunInputBindingDimension(result.RunID, result, []*team.TodoItem{item}, []team.RunInputSnapshot{*snapshot}, audit)
	if dimension.Status != AuditDimensionFail || len(audit.Findings) != 1 || audit.Findings[0].Code != CodeRunInputBindingInvalid {
		encoded, _ := json.Marshal(audit)
		t.Fatalf("tampered audit dimension = %#v audit=%s", dimension, encoded)
	}
}
