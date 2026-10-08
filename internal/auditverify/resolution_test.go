package auditverify

import (
	"testing"

	"github.com/kjelly/hufu/internal/team"
)

func TestResolvedTaskAuditAndWitnessPreserveFailureAndReplacement(t *testing.T) {
	const runID = "run-resolution-audit"
	workspace := t.TempDir()
	store, err := team.NewFileArtifactStore(workspace, workspace)
	if err != nil {
		t.Fatal(err)
	}
	artifact, err := store.Put(t.Context(), team.PutArtifactRequest{Kind: "task_transcript", Path: "replacement.jsonl", Content: []byte("verified replacement"), RunID: runID, TaskID: "2", Attempt: 1, Agent: "worker"})
	if err != nil {
		t.Fatal(err)
	}
	original := &team.TodoItem{ID: "1", Agent: "worker", Status: team.TaskBlocked, Resolution: &team.TaskResolution{Status: "superseded", ResolvedBy: "2"}}
	replacement := &team.TodoItem{ID: "2", Agent: "worker", Status: team.TaskDone, VerifyResult: &team.VerificationResult{ExitCode: 0}, ExecutionReceipt: &team.ExecutionReceipt{
		RunID: runID, TaskID: "2", Attempt: 1, ModelExecutionID: "exec-2", ProducerID: "worker", TranscriptRef: artifact.ID, ExitCode: new(0),
	}}
	items := []*team.TodoItem{original, replacement}
	binding := &team.EvidenceBinding{RunID: runID, TaskID: "2", Attempt: 1, ModelExecutionID: "exec-2", ProducerID: "worker", TranscriptRef: artifact.ID, ArtifactIDs: []string{artifact.ID}}
	manifest := &team.EvidenceManifest{RunID: runID, Status: "accepted", ArtifactRefs: []team.ArtifactRef{artifact.ArtifactRef}, EvidenceResults: []team.EvidenceResult{
		{RequirementID: "task:1", Status: "passed", Binding: binding, ArtifactRefs: []team.ArtifactRef{artifact.ArtifactRef}, Resolution: &team.EvidenceResolution{Status: "superseded", ResolvedBy: "2", OriginalStatus: string(team.TaskBlocked)}},
		{RequirementID: "task:2", Status: "passed", Binding: binding, ArtifactRefs: []team.ArtifactRef{artifact.ArtifactRef}},
	}}
	if err := manifest.Seal(); err != nil {
		t.Fatal(err)
	}
	if err := manifest.Verify(t.Context(), store); err != nil {
		t.Fatal(err)
	}
	runResult := &team.RunResult{RunID: runID, Outcome: team.RunOutcomeCompleted, GoalSatisfied: true, EvidenceManifest: manifest}
	verification := &AuditVerificationResult{}
	if got := verifyProvenanceDimension(runID, runResult, items, verification); got.Status != AuditDimensionPass {
		t.Fatalf("provenance = %#v, findings = %#v", got, verification.Findings)
	}
	if !allRequiredTasksComplete(items, runID, manifest) {
		t.Fatal("verified replacement did not satisfy the original requirement")
	}
	witness, err := buildDecisionWitness(runID, runResult, items, "event-head", "event-hash", nil, GateWitness{Accepted: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(witness.Tasks) != 2 {
		t.Fatalf("task witnesses = %#v", witness.Tasks)
	}
	first := witness.Tasks[0]
	if first.Status != team.TaskBlocked || first.Resolution == nil || first.Resolution.ResolvedBy != "2" || first.WinningAttempt.TaskID != "2" || first.WinningAttempt.ReceiptHash == "" {
		t.Fatalf("witness lost original failure or replacement receipt: %#v", first)
	}
	original.Resolution.ResolvedBy = "missing"
	if got := verifyProvenanceDimension(runID, runResult, items, &AuditVerificationResult{}); got.Status != AuditDimensionFail {
		t.Fatalf("forged replayed resolution accepted: %#v", got)
	}
	if allRequiredTasksComplete(items, runID, manifest) {
		t.Fatal("forged replayed resolution satisfied completion")
	}
}
