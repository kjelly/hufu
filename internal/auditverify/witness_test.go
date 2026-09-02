package auditverify

import (
	"testing"
	"time"

	"github.com/kjelly/hufu/internal/team"
)

func sampleWitness() *DecisionWitness {
	return &DecisionWitness{
		RunID: "run-1", Outcome: team.RunOutcomeCompleted, GoalSatisfied: true,
		AcceptanceState: team.AcceptancePassed, EvidenceManifestHash: "deadbeef",
		EventHeadID: "evt-9", EventHeadHash: "cafebabe",
		Criteria: []CriterionWitness{{CriterionID: "build", Status: "passed"}},
		Tasks:    []TaskWitness{{TaskID: "t1", Status: team.TaskDone}},
		Gate:     GateWitness{Accepted: true},
	}
}

func TestDecisionWitnessSealAndVerify(t *testing.T) {
	w := sampleWitness()
	if err := w.Seal(); err != nil {
		t.Fatalf("seal: %v", err)
	}
	if w.WitnessHash == "" {
		t.Fatal("sealed witness has empty hash")
	}
	if err := w.Verify(); err != nil {
		t.Fatalf("verify freshly sealed witness: %v", err)
	}
}

func TestDecisionWitnessSameProofSameHash(t *testing.T) {
	a, b := sampleWitness(), sampleWitness()
	if err := a.Seal(); err != nil {
		t.Fatal(err)
	}
	if err := b.Seal(); err != nil {
		t.Fatal(err)
	}
	if a.WitnessHash != b.WitnessHash {
		t.Fatalf("identical proofs produced different hashes: %s vs %s", a.WitnessHash, b.WitnessHash)
	}
}

func TestDecisionWitnessOrderInsensitive(t *testing.T) {
	a := sampleWitness()
	a.Criteria = []CriterionWitness{{CriterionID: "build", Status: "passed"}, {CriterionID: "tests", Status: "passed"}}
	a.Tasks = []TaskWitness{{TaskID: "t1", Status: team.TaskDone}, {TaskID: "t2", Status: team.TaskDone}}
	b := sampleWitness()
	b.Criteria = []CriterionWitness{{CriterionID: "tests", Status: "passed"}, {CriterionID: "build", Status: "passed"}}
	b.Tasks = []TaskWitness{{TaskID: "t2", Status: team.TaskDone}, {TaskID: "t1", Status: team.TaskDone}}

	if err := a.Seal(); err != nil {
		t.Fatal(err)
	}
	if err := b.Seal(); err != nil {
		t.Fatal(err)
	}
	if a.WitnessHash != b.WitnessHash {
		t.Fatalf("reordered criteria/tasks changed the witness hash: %s vs %s", a.WitnessHash, b.WitnessHash)
	}
}

func TestDecisionWitnessBindingChangeChangesHash(t *testing.T) {
	base := sampleWitness()
	if err := base.Seal(); err != nil {
		t.Fatal(err)
	}
	changed := sampleWitness()
	changed.Tasks[0].WinningAttempt.Attempt = 2
	if err := changed.Seal(); err != nil {
		t.Fatal(err)
	}
	if base.WitnessHash == changed.WitnessHash {
		t.Fatal("changing a task's winning attempt did not change the witness hash")
	}
}

func TestDecisionWitnessRunFinishedHashChangeChangesHash(t *testing.T) {
	base := sampleWitness()
	if err := base.Seal(); err != nil {
		t.Fatal(err)
	}
	changed := sampleWitness()
	changed.EventHeadHash = "differenthash"
	if err := changed.Seal(); err != nil {
		t.Fatal(err)
	}
	if base.WitnessHash == changed.WitnessHash {
		t.Fatal("changing the bound run_finished hash did not change the witness hash")
	}
}

func TestDecisionWitnessVerifyRejectsTamperedContent(t *testing.T) {
	w := sampleWitness()
	if err := w.Seal(); err != nil {
		t.Fatal(err)
	}
	w.Outcome = team.RunOutcomeFailed // mutate after sealing without resealing
	if err := w.Verify(); err == nil {
		t.Fatal("expected Verify to reject tampered content")
	}
}

func TestDecisionWitnessVerifyRejectsUnsealed(t *testing.T) {
	w := sampleWitness()
	if err := w.Verify(); err == nil {
		t.Fatal("expected Verify to reject an unsealed witness")
	}
}

func TestVerificationFingerprintNilIsEmpty(t *testing.T) {
	if got := VerificationFingerprint(nil); got != "" {
		t.Fatalf("nil fingerprint = %q, want empty", got)
	}
}

func TestVerificationFingerprintReturnsPersistedValue(t *testing.T) {
	vr := &team.VerificationResult{Fingerprint: "vfp_abc123", EvaluatedAt: time.Now()}
	if got := VerificationFingerprint(vr); got != "vfp_abc123" {
		t.Fatalf("fingerprint = %q, want vfp_abc123", got)
	}
}

func TestRequiredCriteriaIDsLatestRevisionWins(t *testing.T) {
	lineage := []team.RunEvent{
		{Type: "acceptance_contract_modified", RunID: "run-1", Payload: mustJSON(t, map[string]any{
			"new_spec": team.AcceptanceSpec{Criteria: []team.AcceptanceCriterion{{ID: "build", Required: true}, {ID: "lint", Required: false}}},
		})},
		{Type: "acceptance_contract_modified", RunID: "run-1", Payload: mustJSON(t, map[string]any{
			"new_spec": team.AcceptanceSpec{Criteria: []team.AcceptanceCriterion{{ID: "build", Required: false}}},
		})},
	}
	got := requiredCriteriaIDs(lineage, "run-1")
	if required, ok := got["build"]; !ok || required {
		t.Fatalf("build required = (%v, %v), want (false, true) from the latest revision", required, ok)
	}
	if _, ok := got["lint"]; ok {
		t.Fatal("lint should not survive being dropped by the latest revision")
	}
}

func TestRequiredCriteriaIDsIgnoresOtherRuns(t *testing.T) {
	lineage := []team.RunEvent{
		{Type: "acceptance_contract_modified", RunID: "run-other", Payload: mustJSON(t, map[string]any{
			"new_spec": team.AcceptanceSpec{Criteria: []team.AcceptanceCriterion{{ID: "build", Required: true}}},
		})},
	}
	if got := requiredCriteriaIDs(lineage, "run-1"); len(got) != 0 {
		t.Fatalf("got %v, want empty map for an unrelated run", got)
	}
}
