package team

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"
)

// These hashes freeze the pre-generalization policy and wire snapshots. They
// intentionally use the current JSON representation rather than the new
// canonical policy encoder: Phase 0 records the input that later phases must
// preserve, including omitted semantic defaults.
func TestDecisionProfileGeneralizationBaseline(t *testing.T) {
	if DecisionAdmissionSchemaVersion != 1 || DecisionRunEnvelopeSchemaVersion != 1 {
		t.Fatalf("baseline schemas = admission %d, envelope %d; want 1/1",
			DecisionAdmissionSchemaVersion, DecisionRunEnvelopeSchemaVersion)
	}

	teamDir := filepath.Join("..", "..", ".agent-teams", "strategic-decision")
	cfg, err := parseTeamYML(teamDir, nil)
	if err != nil {
		t.Fatalf("parse strategic decision team: %v", err)
	}
	policyHashes := map[string]string{
		"light":       "e6990648e95bd74b6c4b8ed47b7203d61e4aaf862365a0b92afdda8c8ec7e2e4",
		"standard":    "613045ed4a491c2c98b369804d4ec5bcaeca48fa387bdcd1fd26cb73cd73aa59",
		"high-stakes": "9a1c0a4f05b4083ec35146f6eb5ed8c80ae9151813c89d40ae9e975cc228b198",
	}
	for name, want := range policyHashes {
		policy, ok := cfg.Decision.Profiles[name]
		if !ok {
			t.Fatalf("strategic profile %q is missing", name)
		}
		if got := baselineJSONHash(t, policy); got != want {
			t.Errorf("strategic profile %q JSON hash = %s, want %s", name, got, want)
		}
	}

	policy := enginePolicy(2)
	admission := DecisionAdmission{
		SchemaVersion:   1,
		RunID:           "run-baseline",
		TaskID:          "task-baseline",
		Attempt:         1,
		Profile:         "standard",
		Source:          DecisionProfileSourceTask,
		Enabled:         true,
		Policy:          &policy,
		DecisionID:      "decision-baseline",
		TaskInputDigest: "input-baseline",
	}
	if got, want := baselineJSONHash(t, admission), "a9fc4615cc7899f6b53756880cd2ffee8214bd737a1e2c5d7e6b665214c64206"; got != want {
		t.Errorf("v1 admission JSON hash = %s, want %s", got, want)
	}

	req := engineRequest(policy)
	req.DecisionID = "decision-baseline"
	req.EvidenceArtifactRef = ArtifactRef{ID: "evidence-baseline", SHA256: "evidence-sha"}
	envelope := newDecisionRunEnvelope(req, policy, DecisionEvidencePacket{Hash: "packet-baseline"}, time.Unix(1_700_000_000, 0).UTC())
	if got, want := baselineJSONHash(t, envelope), "d16a0d53e2dd6880a2f9402b052a3d583898af6b83989a3c4e4be794f4b5dd7b"; got != want {
		t.Errorf("v1 envelope JSON hash = %s, want %s", got, want)
	}
}

func TestDecisionProfileGeneralizationBaselineRejectsCoordinatorDegradation(t *testing.T) {
	admission := admissionForTest(t, true)
	req := engineRequest(*admission.Policy)
	req.DecisionID = admission.DecisionID
	req.RunID = admission.RunID
	req.TaskID = admission.TaskID
	req.Attempt = admission.Attempt
	req.Profile = admission.Profile
	req.AdmissionInputDigest = admission.TaskInputDigest
	req.Policy.IndependentJudgments--
	envelope := newDecisionRunEnvelope(req, req.Policy, DecisionEvidencePacket{Hash: "packet-baseline"}, time.Unix(1_700_000_000, 0).UTC())
	if err := validateDecisionAdmissionEnvelope(admission, envelope); err == nil {
		t.Fatal("coordinator admission accepted a policy-changing degradation")
	}
}

func baselineJSONHash(t *testing.T, value any) string {
	t.Helper()
	b, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal baseline value: %v", err)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
