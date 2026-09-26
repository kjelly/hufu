package auditverify

import (
	"strings"
	"testing"
	"time"

	"github.com/kjelly/hufu/internal/team"
)

// TestVerifyRunInputBindingDimensionAcceptsAFailedActionWithoutReceipt pins
// the audit of a run whose input-bound action failed: the action wrote no
// task receipt because it did not succeed, which is not a binding violation.
// A completed task, a leftover result, or a receipt that exists still has to
// match the frozen inputs.
func TestVerifyRunInputBindingDimensionAcceptsAFailedActionWithoutReceipt(t *testing.T) {
	const runID = "run-input-failed-action"
	definitions := []team.RunInputDefinition{{Name: "review.scope", Schema: team.RunInputSchema{Type: "object"}, Required: true}}
	snapshot, err := team.ResolveRunInputSnapshot(definitions, []team.RunInputAssignment{{
		Name: "review.scope", RawValue: []byte(`{"count":8,"kind":"last_n"}`), Source: team.RunInputSourceCLI,
	}}, runID, "invocation-failed", "review")
	if err != nil {
		t.Fatal(err)
	}
	bound := map[string]string{"review.scope": snapshot.Inputs[0].ValueHash}
	failedItem := func() *team.TodoItem {
		return &team.TodoItem{
			ID: "1", PlanTaskID: "produce-workset", Status: team.TaskError, OccurrenceRevision: 1,
			RunInputSnapshotID: snapshot.ID, RunInputSnapshotHash: snapshot.SnapshotHash,
			MaterializedActionPayloadHash: "sha256:materialized", BoundInputs: bound,
		}
	}
	exitCode := 1
	receipt := func(payloadHash string) team.ExecutionReceipt {
		return team.ExecutionReceipt{
			RunID: runID, TaskID: "1", Attempt: 1, ExitCode: &exitCode, ActionInvocationID: "action-1",
			StartedAt: time.Now().Add(-time.Second), FinishedAt: time.Now(),
			RunInputSnapshotID: snapshot.ID, RunInputSnapshotHash: snapshot.SnapshotHash,
			MaterializedActionPayloadHash: payloadHash, BoundInputs: bound,
		}
	}
	tests := []struct {
		name    string
		item    func() *team.TodoItem
		wantErr string
	}{
		{name: "failed action without a receipt", item: failedItem},
		{name: "failed action with a mismatching receipt", item: func() *team.TodoItem {
			item := failedItem()
			item.ExecutionReceipts = []team.ExecutionReceipt{receipt("sha256:other")}
			return item
		}, wantErr: "action receipt does not match"},
		{name: "completed action without a receipt", item: func() *team.TodoItem {
			item := failedItem()
			item.Status = team.TaskDone
			return item
		}, wantErr: "action receipt does not match"},
		{name: "failed action with a result but no receipt", item: func() *team.TodoItem {
			item := failedItem()
			item.TypedResult = &team.TaskResult{TaskID: "1", Status: team.TaskResultStatusFailed, Source: "runtime"}
			return item
		}, wantErr: "action receipt does not match"},
		{name: "failed action materialized from another snapshot", item: func() *team.TodoItem {
			item := failedItem()
			item.RunInputSnapshotID = "run-inputs-other"
			return item
		}, wantErr: "task materialization identity"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			item := tt.item()
			spec := team.VerificationSpec{
				Type: team.VerifyTaskOutputAssert, WorksetSourceTask: "produce-workset", TaskOutputName: "scope",
				Assertions: []team.TaskOutputAssertion{{Pointer: "/satisfied", Op: "equals", Value: true}},
			}
			evidence, replayErr := team.ReplayTaskOutputAssertions([]*team.TodoItem{item}, runID, snapshot, spec)
			if replayErr == nil {
				t.Fatal("assertions on a failed producer passed")
			}
			acceptance := &team.AcceptanceResult{State: team.AcceptanceFailed, VerificationEvidence: []*team.VerificationResult{
				{ExitCode: 1, Spec: &spec, TaskOutputAssertions: evidence},
			}}
			result := &team.RunResult{RunID: runID, RunInputs: snapshot, Acceptance: acceptance, InputBoundAssertions: team.SummarizeInputBoundAssertions(acceptance)}
			audit := &AuditVerificationResult{RunID: runID}
			dimension := verifyRunInputBindingDimension(runID, result, []*team.TodoItem{item}, []team.RunInputSnapshot{*snapshot}, audit)
			if tt.wantErr == "" {
				if dimension.Status != AuditDimensionPass || len(audit.Findings) != 0 {
					t.Fatalf("dimension = %#v findings = %#v, want a pass", dimension, audit.Findings)
				}
				return
			}
			if dimension.Status != AuditDimensionFail || len(audit.Findings) != 1 || audit.Findings[0].Code != CodeRunInputBindingInvalid ||
				!strings.Contains(dimension.Reason, tt.wantErr) {
				t.Fatalf("dimension = %#v findings = %#v, want %s with %q", dimension, audit.Findings, CodeRunInputBindingInvalid, tt.wantErr)
			}
		})
	}
}
