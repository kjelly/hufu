package team

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/kjelly/hufu/internal/agent"
)

// These hashes freeze the pre-generalization policy and wire snapshots. They
// intentionally use the current JSON representation rather than the new
// canonical policy encoder: Phase 0 records the input that later phases must
// preserve, including omitted semantic defaults.
func TestDecisionProfileGeneralizationBaseline(t *testing.T) {
	if decisionAdmissionLegacySchemaVersion != 1 || decisionRunEnvelopeLegacySchemaVersion != 1 {
		t.Fatalf("legacy schemas = admission %d, envelope %d; want 1/1",
			decisionAdmissionLegacySchemaVersion, decisionRunEnvelopeLegacySchemaVersion)
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
	envelope := legacyDecisionRunEnvelopeForBaseline(req, policy, DecisionEvidencePacket{Hash: "packet-baseline"}, time.Unix(1_700_000_000, 0).UTC())
	if got, want := baselineJSONHash(t, envelope), "d16a0d53e2dd6880a2f9402b052a3d583898af6b83989a3c4e4be794f4b5dd7b"; got != want {
		t.Errorf("v1 envelope JSON hash = %s, want %s", got, want)
	}
}

func TestBuiltInDecisionProfilesEqualStrategicBaseline(t *testing.T) {
	teamDir := filepath.Join("..", "..", ".agent-teams", "strategic-decision")
	cfg, err := parseTeamYML(teamDir, nil)
	if err != nil {
		t.Fatalf("parse strategic decision team: %v", err)
	}
	catalog := agent.BuiltInDecisionProfileCatalog()
	refs := map[string]string{
		"light":       agent.DecisionProfileBuiltinLightV1,
		"standard":    agent.DecisionProfileBuiltinStandardV1,
		"high-stakes": agent.DecisionProfileBuiltinHighStakesV1,
	}
	for local, ref := range refs {
		got, _, err := catalog.Resolve(agent.DecisionProfileRef{Name: ref})
		if err != nil {
			t.Fatalf("resolve %q: %v", ref, err)
		}
		if want := cfg.Decision.Profiles[local]; !reflect.DeepEqual(got, want) {
			t.Fatalf("built-in %q differs from strategic profile %q", ref, local)
		}
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
	digest, err := agent.DecisionPolicyDigest(req.Policy)
	if err != nil {
		t.Fatal(err)
	}
	req.ProfileOrigin, req.PolicyDigest = admission.ProfileOrigin, digest
	envelope, err := newDecisionRunEnvelope(req, req.Policy, DecisionEvidencePacket{Hash: "packet-baseline"}, time.Unix(1_700_000_000, 0).UTC())
	if err != nil {
		t.Fatal(err)
	}
	if err := validateDecisionAdmissionEnvelope(admission, envelope); err == nil {
		t.Fatal("coordinator admission accepted a policy-changing degradation")
	}
}

func legacyDecisionRunEnvelopeForBaseline(req DecisionRequest, policy DecisionPolicy, packet DecisionEvidencePacket, now time.Time) DecisionRunEnvelope {
	req = cloneDecisionRequest(req)
	req.Policy = policy
	return DecisionRunEnvelope{
		SchemaVersion: decisionRunEnvelopeLegacySchemaVersion,
		DecisionID:    req.DecisionID, RunID: req.RunID, TaskID: req.TaskID,
		Attempt: req.Attempt, Profile: req.Profile, Request: req, Policy: policy,
		StageProgress: DecisionRunStageProgress{CurrentStage: "evidence_sealed", EvidenceHash: packet.Hash, NextStage: "judge"},
		IdempotencyKeys: map[string]string{
			"envelope":    decisionRunEnvelopeEventKey(req.DecisionID, packet.Hash),
			"aggregate:1": decisionStageEventKey(req.DecisionID, "aggregate", packet.Hash, "1"),
			"finalized":   decisionStageEventKey(req.DecisionID, "finalized", packet.Hash),
		},
		CreatedAt: now.UTC(),
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
