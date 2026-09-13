package auditverify

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
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

func TestDecisionWitnessSemanticRegressionChangeChangesHash(t *testing.T) {
	base := sampleWitness()
	if err := base.Seal(); err != nil {
		t.Fatal(err)
	}
	changed := sampleWitness()
	changed.Gate.SemanticRegressionConfigured = true
	changed.Gate.SemanticRegressionBlockingCount = 1
	changed.Gate.SemanticRegressionReasons = []string{"task gate invariant safe is violated"}
	if err := changed.Seal(); err != nil {
		t.Fatal(err)
	}
	if base.WitnessHash == changed.WitnessHash {
		t.Fatal("changing the semantic regression decision did not change the witness hash")
	}
}

func TestVerifySemanticRegressionDimensionRejectsInvalidAttestation(t *testing.T) {
	item := &team.TodoItem{ID: "gate", Status: team.TaskDone, InvariantVerification: team.InvariantVerificationGate}
	decision := team.EvaluateSemanticRegression("run-1", []*team.TodoItem{item})
	result := &AuditVerificationResult{RunID: "run-1"}
	dimension := verifySemanticRegressionDimension("run-1", []*team.TodoItem{item}, decision, result)
	if dimension.Status != AuditDimensionFail {
		t.Fatalf("dimension = %#v, want fail", dimension)
	}
	if len(result.Findings) != 1 || result.Findings[0].Code != CodeInvariantAttestationInvalid {
		t.Fatalf("findings = %#v, want attestation-invalid finding", result.Findings)
	}
}

func TestVerifyWitnessLinkageRejectsSemanticDecisionMismatch(t *testing.T) {
	dir := t.TempDir()
	witness := sampleWitness()
	if err := witness.Seal(); err != nil {
		t.Fatal(err)
	}
	writeDecisionWitness(t, dir, witness)
	manifest := AuditBundleManifest{RunID: witness.RunID, RunFinishedEventHash: witness.EventHeadHash, EvidenceManifestHash: witness.EvidenceManifestHash}
	result := &AuditVerificationResult{RunID: witness.RunID, Integrity: AuditDimensionResult{Status: AuditDimensionPass}}
	projection := &runProjection{tasks: []*team.TodoItem{{ID: "gate", InvariantVerification: team.InvariantVerificationGate}}}

	verifyWitnessLinkage(dir, manifest, result, projection)
	if result.Integrity.Status != AuditDimensionFail || len(result.Findings) != 1 || result.Findings[0].Code != CodeInvariantWitnessMismatch {
		t.Fatalf("semantic witness mismatch result = %#v", result)
	}
}

func TestVerifyWitnessLinkageAcceptsLegacyWitnessOnlyWithoutGate(t *testing.T) {
	dir := t.TempDir()
	witness := sampleWitness()
	sealLegacyWitness(t, witness)
	writeDecisionWitness(t, dir, witness)
	manifest := AuditBundleManifest{RunID: witness.RunID, RunFinishedEventHash: witness.EventHeadHash, EvidenceManifestHash: witness.EvidenceManifestHash}

	legacy := &AuditVerificationResult{RunID: witness.RunID, Integrity: AuditDimensionResult{Status: AuditDimensionPass}}
	verifyWitnessLinkage(dir, manifest, legacy, &runProjection{})
	if legacy.Integrity.Status != AuditDimensionPass {
		t.Fatalf("legacy no-gate witness = %#v, want pass", legacy)
	}

	withGate := &AuditVerificationResult{RunID: witness.RunID, Integrity: AuditDimensionResult{Status: AuditDimensionPass}}
	verifyWitnessLinkage(dir, manifest, withGate, &runProjection{tasks: []*team.TodoItem{{ID: "gate", InvariantVerification: team.InvariantVerificationGate}}})
	if withGate.Integrity.Status != AuditDimensionFail || len(withGate.Findings) != 1 || withGate.Findings[0].Code != CodeInvariantWitnessMismatch {
		t.Fatalf("legacy gate witness = %#v, want semantic mismatch", withGate)
	}
}

func writeDecisionWitness(t *testing.T, dir string, witness *DecisionWitness) {
	t.Helper()
	data, err := json.Marshal(witness)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "decision-witness.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func sealLegacyWitness(t *testing.T, witness *DecisionWitness) {
	t.Helper()
	witness.SchemaVersion = 1
	witness.WitnessHash = ""
	data, err := json.Marshal(witness)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	witness.WitnessHash = hex.EncodeToString(sum[:])
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
