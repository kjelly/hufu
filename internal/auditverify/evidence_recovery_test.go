package auditverify

import (
	"testing"

	"github.com/kjelly/hufu/internal/team"
)

func TestRecoveryProvenanceRequiresCanonicalInputBoundReceipt(t *testing.T) {
	for _, mode := range []string{"valid", "missing_receipt", "wrong_input", "wrong_transcript", "wrong_origin", "missing_snapshot"} {
		t.Run(mode, func(t *testing.T) {
			snapshot, err := team.ResolveRunInputSnapshot([]team.RunInputDefinition{{Name: "count", Schema: team.RunInputSchema{Type: "integer"}, Required: true}},
				[]team.RunInputAssignment{{Name: "count", RawValue: []byte("3"), Source: team.RunInputSourceCLI}}, "request-run", "request", "generic")
			if err != nil {
				t.Fatal(err)
			}
			receipt := team.ExecutionReceipt{RunID: "producer-run", TaskID: "1", Attempt: 1, ModelExecutionID: "execution", ProducerID: "worker", ExitCode: new(0), TranscriptRef: "transcript",
				RunInputSnapshotID: snapshot.ID, RunInputSnapshotHash: snapshot.SnapshotHash}
			item := &team.TodoItem{ID: "1", Status: team.TaskDone, RunInputSnapshotID: snapshot.ID, RunInputSnapshotHash: snapshot.SnapshotHash, ExecutionReceipts: []team.ExecutionReceipt{receipt}}
			binding := &team.EvidenceBinding{RunID: receipt.RunID, TaskID: "1", Attempt: 1, ModelExecutionID: receipt.ModelExecutionID, ProducerID: receipt.ProducerID, TranscriptRef: receipt.TranscriptRef,
				RunInputSnapshotID: snapshot.ID, RunInputSnapshotHash: snapshot.SnapshotHash}
			manifest := &team.EvidenceManifest{RunID: "recovery-run", RecoverySnapshotID: snapshot.ID, RecoverySnapshotHash: snapshot.SnapshotHash,
				EvidenceResults: []team.EvidenceResult{{RequirementID: "task:1", Status: "passed", Binding: binding}}}
			run := &team.RunResult{RunID: manifest.RunID, RunInputs: snapshot, RunInputOriginRunID: snapshot.RunID, EvidenceManifest: manifest}
			switch mode {
			case "missing_receipt":
				item.ExecutionReceipts = nil
			case "wrong_input":
				item.RunInputSnapshotHash = "wrong"
			case "wrong_transcript":
				binding.TranscriptRef = "different"
			case "wrong_origin":
				binding.RunID = "unrelated-run"
			case "missing_snapshot":
				run.RunInputs = nil
			}
			audit := &AuditVerificationResult{}
			result := verifyProvenanceDimension(run.RunID, run, []*team.TodoItem{item}, audit)
			if (result.Status == AuditDimensionPass) != (mode == "valid") {
				t.Fatalf("provenance=%s reason=%s", result.Status, result.Reason)
			}
		})
	}
}
