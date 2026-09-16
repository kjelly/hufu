package team

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/kjelly/hufu/internal/agent"
)

func TestDecisionRunEnvelopeReadsV1AndWritesV2Identity(t *testing.T) {
	policy := enginePolicy(2)
	req := validEnvelopeRequest(policy)

	legacy := legacyDecisionRunEnvelopeForBaseline(req, policy, DecisionEvidencePacket{Hash: "packet-v1"}, time.Unix(1, 0))
	if err := legacy.Validate(); err != nil {
		t.Fatalf("legacy envelope validation: %v", err)
	}

	materialized, err := materializeDirectDecisionRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	envelope, err := newDecisionRunEnvelope(materialized, materialized.Policy, DecisionEvidencePacket{Hash: "packet-v2"}, time.Unix(2, 0))
	if err != nil {
		t.Fatal(err)
	}
	if err := envelope.Validate(); err != nil {
		t.Fatalf("schema-v2 envelope validation: %v", err)
	}
	if envelope.SchemaVersion != DecisionRunEnvelopeSchemaVersion || envelope.ProfileOrigin != agent.DecisionProfileOriginRequestInline || envelope.PolicyDigest == "" {
		t.Fatalf("schema-v2 envelope identity = %#v", envelope.profileIdentity())
	}
	if !envelope.profileIdentity().equal(envelope.Request.profileIdentity()) {
		t.Fatal("top-level and nested request identities differ")
	}

	tampered := envelope
	tampered.Policy.IndependentJudgments++
	tampered.Request.Policy = tampered.Policy
	if err := tampered.Validate(); err == nil || !strings.Contains(err.Error(), "digest mismatch") {
		t.Fatalf("tampered policy validation = %v, want digest mismatch", err)
	}
	tampered = envelope
	tampered.Request.PolicyDigest = strings.Repeat("a", 64)
	if err := tampered.Validate(); err == nil {
		t.Fatal("accepted malformed nested request digest")
	}
}

func TestDecisionAdmissionEnvelopeSchemaCompatibilityMatrix(t *testing.T) {
	v2Admission := admissionForTest(t, true)
	v2Admission.RequestContractRef = "contract-v2"
	v2Admission.RequestContractRevision = 2
	v2Admission.RequestContractArtifact = ArtifactRef{ID: "contract-v2", SHA256: strings.Repeat("b", 64)}
	v2Request := requestForAdmission(*v2Admission.Policy, v2Admission)
	v2Envelope, err := newDecisionRunEnvelope(v2Request, v2Request.Policy, DecisionEvidencePacket{Hash: "packet-v2"}, time.Unix(2, 0))
	if err != nil {
		t.Fatal(err)
	}
	if err := validateDecisionAdmissionEnvelope(v2Admission, v2Envelope); err != nil {
		t.Fatalf("v2 admission + v2 envelope: %v", err)
	}

	v1Envelope := legacyDecisionRunEnvelopeForBaseline(v2Request, v2Request.Policy, DecisionEvidencePacket{Hash: "packet-v1"}, time.Unix(1, 0))
	v1Envelope.ProfileOrigin, v1Envelope.PolicyDigest = "", ""
	v1Envelope.Request.ProfileOrigin, v1Envelope.Request.PolicyDigest = "", ""
	if err := validateDecisionAdmissionEnvelope(v2Admission, v1Envelope); err == nil || !strings.Contains(err.Error(), "schema-v1") {
		t.Fatalf("v2 admission + v1 envelope = %v, want downgrade rejection", err)
	}

	v1Admission := v2Admission
	v1Admission.SchemaVersion = decisionAdmissionLegacySchemaVersion
	v1Admission.ProfileOrigin, v1Admission.ProfileVersion, v1Admission.ProfileRef, v1Admission.PolicyDigest = "", "", "", ""
	raw := *v1Admission.Policy
	raw.MaxRounds = 0
	v1Admission.Policy = &raw
	bridgeRequest := requestForAdmission(raw, v1Admission)
	bridgeRequest.ProfileOrigin = agent.DecisionProfileOriginLegacyInline
	bridgeRequest.PolicyDigest, err = agent.DecisionPolicyDigest(raw)
	if err != nil {
		t.Fatal(err)
	}
	bridgeRequest, err = materializeDirectDecisionRequest(bridgeRequest)
	if err != nil {
		t.Fatal(err)
	}
	if bridgeRequest.Policy.MaxRounds != 0 {
		t.Fatal("v1 admission raw policy was normalized in place")
	}
	bridge, err := newDecisionRunEnvelope(bridgeRequest, bridgeRequest.Policy, DecisionEvidencePacket{Hash: "packet-bridge"}, time.Unix(3, 0))
	if err != nil {
		t.Fatal(err)
	}
	if err := validateDecisionAdmissionEnvelope(v1Admission, bridge); err != nil {
		t.Fatalf("v1 admission + v2 bridge: %v", err)
	}
	if bridge.ProfileOrigin != agent.DecisionProfileOriginLegacyInline || bridge.Policy.MaxRounds != 0 {
		t.Fatalf("bridge did not preserve legacy identity/raw policy: %#v", bridge)
	}

	legacy := legacyDecisionRunEnvelopeForBaseline(bridgeRequest, raw, DecisionEvidencePacket{Hash: "packet-legacy"}, time.Unix(1, 0))
	legacy.ProfileOrigin, legacy.PolicyDigest = "", ""
	legacy.Request.ProfileOrigin, legacy.Request.PolicyDigest = "", ""
	if err := validateDecisionAdmissionEnvelope(v1Admission, legacy); err != nil {
		t.Fatalf("v1 admission + v1 envelope: %v", err)
	}
}

func TestV1AdmissionWithoutEnvelopeAnchorsV2Bridge(t *testing.T) {
	journal := &memoryJournal{}
	policy := enginePolicy(1)
	policy.MaxRounds = 0
	admission := DecisionAdmission{
		SchemaVersion: decisionAdmissionLegacySchemaVersion,
		RunID:         "run-bridge", TaskID: "task-bridge", Attempt: 1,
		Profile: "legacy", Source: DecisionProfileSourceTask, Enabled: true,
		Policy: &policy, DecisionID: "decision-bridge", TaskInputDigest: "input-bridge",
		RequestContractRef: "contract-bridge", RequestContractRevision: 1,
		RequestContractArtifact: ArtifactRef{ID: "contract-bridge", SHA256: strings.Repeat("c", 64)},
	}
	if _, err := appendDecisionAdmission(t.Context(), journal, admission); err != nil {
		t.Fatal(err)
	}
	req := engineRequest(policy)
	req.DecisionID, req.RunID, req.TaskID, req.Attempt = admission.DecisionID, admission.RunID, admission.TaskID, admission.Attempt
	req.Profile, req.AdmissionInputDigest = admission.Profile, admission.TaskInputDigest
	req.RequestContractRef, req.RequestContractRevision = admission.RequestContractRef, admission.RequestContractRevision
	req.RequestContractArtifact = admission.RequestContractArtifact
	req.ProfileOrigin = agent.DecisionProfileOriginLegacyInline
	var err error
	req.PolicyDigest, err = agent.DecisionPolicyDigest(policy)
	if err != nil {
		t.Fatal(err)
	}
	runner := newRecordingRunner(func(string, int) (DecisionOpinion, error) {
		return DecisionOpinion{}, context.Canceled
	})
	engine := newTestEngine(journal, runner, nil)
	_, runErr := engine.Run(t.Context(), req)
	if runErr == nil {
		t.Fatal("bridge fixture unexpectedly finalized")
	}
	state, err := projectDecision(t.Context(), journal, req.DecisionID)
	if err != nil {
		t.Fatal(err)
	}
	if state.EnvelopeRef.ID == "" {
		t.Fatalf("v1 admission did not anchor a bridge envelope before dispatch: %v", runErr)
	}
	envelope, err := loadDecisionRunEnvelope(t.Context(), journal.store, state.EnvelopeRef)
	if err != nil {
		t.Fatal(err)
	}
	if envelope.SchemaVersion != DecisionRunEnvelopeSchemaVersion || envelope.ProfileOrigin != agent.DecisionProfileOriginLegacyInline {
		t.Fatalf("bridge envelope identity = schema %d, origin %q", envelope.SchemaVersion, envelope.ProfileOrigin)
	}
	if !reflect.DeepEqual(envelope.Policy, policy) || envelope.Policy.MaxRounds != 0 {
		t.Fatal("bridge envelope rewrote the v1 policy snapshot")
	}
}

func TestDecisionRequestProfileIdentityFailsClosedAndClonesPolicy(t *testing.T) {
	policy := enginePolicy(1)
	req := validEnvelopeRequest(policy)
	req.ProfileOrigin = agent.DecisionProfileOriginBuiltin
	req.ProfileVersion = "v1"
	req.ProfileRef = agent.DecisionProfileBuiltinStandardV1
	if _, err := materializeDirectDecisionRequest(req); err == nil || !strings.Contains(err.Error(), "digest") {
		t.Fatalf("partial supplied identity = %v, want failure", err)
	}

	req = validEnvelopeRequest(policy)
	materialized, err := materializeDirectDecisionRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	envelope, err := newDecisionRunEnvelope(materialized, materialized.Policy, DecisionEvidencePacket{Hash: "packet-clone"}, time.Unix(4, 0))
	if err != nil {
		t.Fatal(err)
	}
	materialized.Policy.Criteria[0].ID = "mutated"
	policy.Criteria[0].ID = "also-mutated"
	if envelope.Policy.Criteria[0].ID != "cost" || envelope.Request.Policy.Criteria[0].ID != "cost" {
		t.Fatal("envelope retained a caller-owned policy slice")
	}
}

func validEnvelopeRequest(policy DecisionPolicy) DecisionRequest {
	req := engineRequest(policy)
	req.DecisionID = "decision-envelope"
	req.Attempt = 1
	req.EvidenceArtifactRef = ArtifactRef{ID: "evidence", SHA256: strings.Repeat("a", 64)}
	return req
}

func requestForAdmission(policy DecisionPolicy, admission DecisionAdmission) DecisionRequest {
	req := validEnvelopeRequest(policy)
	req.DecisionID, req.RunID, req.TaskID, req.Attempt = admission.DecisionID, admission.RunID, admission.TaskID, admission.Attempt
	req.Profile, req.AdmissionInputDigest = admission.Profile, admission.TaskInputDigest
	req.ProfileOrigin, req.ProfileVersion = admission.ProfileOrigin, admission.ProfileVersion
	req.ProfileRef, req.PolicyDigest = admission.ProfileRef, admission.PolicyDigest
	req.RequestContractRef, req.RequestContractRevision = admission.RequestContractRef, admission.RequestContractRevision
	req.RequestContractArtifact = admission.RequestContractArtifact
	return req
}
