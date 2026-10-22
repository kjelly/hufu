package auditverify

import (
	"testing"

	"github.com/kjelly/hufu/internal/team"
)

func TestRequestAcceptanceAuditHandlesOrdinaryTasksAndFrozenResume(t *testing.T) {
	const originalRun = "run-original"
	snapshot, err := team.ResolveRunInputSnapshot([]team.RunInputDefinition{{Name: "hufu.request_acceptance", Required: true, Schema: team.RunInputSchema{Type: "object"}}},
		[]team.RunInputAssignment{{Name: "hufu.request_acceptance", Source: team.RunInputSourceResolver, ResolverID: "hufu-request-acceptance", ResolverVersion: "1", RawValue: []byte(`{"version":1,"request":"read document","requirements":[{"id":"requirement-1","statement":"read document","source_quote":"read document","required":true,"verification":"observed"}]}`)}},
		originalRun, "invocation", "generic")
	if err != nil {
		t.Fatal(err)
	}
	for _, runID := range []string{originalRun, "run-resumed"} {
		item := &team.TodoItem{ID: "1", Status: team.TaskDone, RunInputSnapshotID: snapshot.ID, RunInputSnapshotHash: snapshot.SnapshotHash,
			ExecutionReceipt: &team.ExecutionReceipt{RunID: runID, TaskID: "1", ExitCode: new(0),
				RunInputSnapshotID: snapshot.ID, RunInputSnapshotHash: snapshot.SnapshotHash}}
		result := &team.RunResult{RunID: runID, RunInputs: snapshot}
		audit := &AuditVerificationResult{RunID: runID}
		if got := verifyRunInputBindingDimension(runID, result, []*team.TodoItem{item}, []team.RunInputSnapshot{*snapshot}, audit); got.Status != AuditDimensionPass {
			t.Fatalf("%s ordinary task audit=%#v", runID, got)
		}
		item.ExecutionReceipt.RunInputSnapshotHash = "foreign-snapshot"
		if got := verifyRunInputBindingDimension(runID, result, []*team.TodoItem{item}, []team.RunInputSnapshot{*snapshot}, audit); got.Status != AuditDimensionFail {
			t.Fatalf("%s accepted foreign receipt: %#v", runID, got)
		}
	}
}
