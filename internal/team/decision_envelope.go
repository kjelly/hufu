package team

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"time"

	"github.com/kjelly/hufu/internal/agent"
)

// DecisionRunEnvelopeSchemaVersion is the wire version for the immutable
// request snapshot used to resume a decision in a fresh process.
const (
	decisionRunEnvelopeLegacySchemaVersion = 1
	DecisionRunEnvelopeSchemaVersion       = 2
)

// ErrLegacyUnfinishedDecision identifies a pre-envelope decision that cannot
// be resumed safely. Its request snapshot was never committed to CAS, so a
// caller must not reconstruct it from current configuration.
var ErrLegacyUnfinishedDecision = errors.New("legacy unfinished decision requires a durable run envelope")

// LegacyUnfinishedDecisionError is returned for an unfinished decision whose
// durable history predates the run envelope. Finalized legacy decisions remain
// readable because their record is already authoritative.
type LegacyUnfinishedDecisionError struct {
	DecisionID string
}

func (e *LegacyUnfinishedDecisionError) Error() string {
	if e == nil || strings.TrimSpace(e.DecisionID) == "" {
		return ErrLegacyUnfinishedDecision.Error()
	}
	return fmt.Sprintf("decision %s has no durable run envelope; resume requires its request for a legacy unfinished run", e.DecisionID)
}

func (e *LegacyUnfinishedDecisionError) Unwrap() error { return ErrLegacyUnfinishedDecision }

// DecisionRunStageProgress describes the point at which the immutable run
// input was committed. The envelope is not updated in place; later progress is
// represented by the event projection.
type DecisionRunStageProgress struct {
	CurrentStage string `json:"current_stage"`
	EvidenceHash string `json:"evidence_hash,omitempty"`
	NextStage    string `json:"next_stage"`
}

// DecisionRunEnvelope is the immutable, content-addressed input snapshot for
// one decision run. Request contains the resolved request inputs (including
// normalized artifact references), while Policy is the profile snapshot used
// for every subsequent stage. Neither field is reconstructed from current
// configuration during resume.
type DecisionRunEnvelope struct {
	SchemaVersion int `json:"schema_version"`

	DecisionID     string `json:"decision_id"`
	RunID          string `json:"run_id,omitempty"`
	TaskID         string `json:"task_id,omitempty"`
	Attempt        int    `json:"attempt,omitempty"`
	Profile        string `json:"profile"`
	ProfileOrigin  string `json:"profile_origin,omitempty"`
	ProfileVersion string `json:"profile_version,omitempty"`
	ProfileRef     string `json:"profile_ref,omitempty"`
	PolicyDigest   string `json:"policy_digest,omitempty"`

	Request DecisionRequest `json:"request"`
	Policy  DecisionPolicy  `json:"policy"`

	StageProgress   DecisionRunStageProgress `json:"stage_progress"`
	IdempotencyKeys map[string]string        `json:"idempotency_keys,omitempty"`
	CreatedAt       time.Time                `json:"created_at"`
}

func (e DecisionRunEnvelope) Validate() error {
	if e.SchemaVersion != decisionRunEnvelopeLegacySchemaVersion && e.SchemaVersion != DecisionRunEnvelopeSchemaVersion {
		return fmt.Errorf("unsupported decision run envelope schema version %d", e.SchemaVersion)
	}
	if strings.TrimSpace(e.DecisionID) == "" || strings.TrimSpace(e.RunID) == "" ||
		strings.TrimSpace(e.TaskID) == "" || e.Attempt < 1 || strings.TrimSpace(e.Profile) == "" {
		return fmt.Errorf("decision run envelope identity is incomplete")
	}
	if e.Request.DecisionID != e.DecisionID {
		return fmt.Errorf("decision run envelope request decision identity does not match")
	}
	if e.Request.RunID != e.RunID || e.Request.TaskID != e.TaskID || e.Request.Attempt != e.Attempt || e.Request.Profile != e.Profile {
		return fmt.Errorf("decision run envelope request identity does not match")
	}
	if strings.TrimSpace(e.Request.EvidenceArtifactRef.ID) == "" || strings.TrimSpace(e.Request.EvidenceArtifactRef.SHA256) == "" {
		return fmt.Errorf("decision run envelope evidence identity is incomplete")
	}
	if !reflect.DeepEqual(e.Request.Policy, e.Policy) {
		return fmt.Errorf("decision run envelope policy snapshot does not match request")
	}
	identity := e.profileIdentity()
	requestIdentity := e.Request.profileIdentity()
	if e.SchemaVersion == DecisionRunEnvelopeSchemaVersion {
		if err := validateDecisionProfileIdentity(identity, e.Policy, true, true); err != nil {
			return fmt.Errorf("decision run envelope profile identity: %w", err)
		}
		if !identity.equal(requestIdentity) {
			return fmt.Errorf("decision run envelope profile identity does not match request")
		}
	} else {
		if err := validateDecisionProfileIdentity(identity, e.Policy, false, false); err != nil {
			return fmt.Errorf("legacy decision run envelope profile identity: %w", err)
		}
		if err := validateDecisionProfileIdentity(requestIdentity, e.Request.Policy, false, false); err != nil {
			return fmt.Errorf("legacy decision run envelope request profile identity: %w", err)
		}
		if err := validateLegacyDecisionProfileIdentityAgreement(identity, requestIdentity); err != nil {
			return fmt.Errorf("legacy decision run envelope profile identity: %w", err)
		}
	}
	if e.StageProgress.CurrentStage == "" || e.StageProgress.NextStage == "" {
		return fmt.Errorf("decision run envelope stage progress is incomplete")
	}
	if e.StageProgress.EvidenceHash == "" {
		return fmt.Errorf("decision run envelope has no sealed evidence hash")
	}
	return nil
}

func (e DecisionRunEnvelope) RequestSnapshot() DecisionRequest {
	return cloneDecisionRequest(e.Request)
}

func newDecisionRunEnvelope(req DecisionRequest, policy DecisionPolicy, packet DecisionEvidencePacket, now time.Time) (DecisionRunEnvelope, error) {
	req = cloneDecisionRequest(req)
	policyClone, err := cloneDecisionPolicy(policy)
	if err != nil {
		return DecisionRunEnvelope{}, err
	}
	req.Policy = policyClone
	digest, err := decisionPolicyDigest(policyClone)
	if err != nil {
		return DecisionRunEnvelope{}, err
	}
	req.PolicyDigest = digest
	if now.IsZero() {
		now = time.Now().UTC()
	}
	return DecisionRunEnvelope{
		SchemaVersion:  DecisionRunEnvelopeSchemaVersion,
		DecisionID:     req.DecisionID,
		RunID:          req.RunID,
		TaskID:         req.TaskID,
		Attempt:        req.Attempt,
		Profile:        req.Profile,
		ProfileOrigin:  req.ProfileOrigin,
		ProfileVersion: req.ProfileVersion,
		ProfileRef:     req.ProfileRef,
		PolicyDigest:   req.PolicyDigest,
		Request:        req,
		Policy:         policyClone,
		StageProgress: DecisionRunStageProgress{
			CurrentStage: "evidence_sealed",
			EvidenceHash: packet.Hash,
			NextStage:    "judge",
		},
		IdempotencyKeys: map[string]string{
			"envelope":    decisionRunEnvelopeEventKey(req.DecisionID, packet.Hash),
			"aggregate:1": decisionStageEventKey(req.DecisionID, "aggregate", packet.Hash, "1"),
			"finalized":   decisionStageEventKey(req.DecisionID, "finalized", packet.Hash),
		},
		CreatedAt: now.UTC(),
	}, nil
}

func (e DecisionRunEnvelope) profileIdentity() decisionProfileIdentity {
	return decisionProfileIdentity{Origin: e.ProfileOrigin, Version: e.ProfileVersion, Ref: e.ProfileRef, Digest: e.PolicyDigest}
}

func validateLegacyDecisionProfileIdentityAgreement(a, b decisionProfileIdentity) error {
	if a.Origin != "" && b.Origin != "" && a.Origin != b.Origin ||
		a.Version != "" && b.Version != "" && a.Version != b.Version ||
		a.Ref != "" && b.Ref != "" && a.Ref != b.Ref ||
		a.Digest != "" && b.Digest != "" && a.Digest != b.Digest {
		return fmt.Errorf("envelope and request metadata conflict")
	}
	return nil
}

func cloneDecisionPolicy(policy DecisionPolicy) (DecisionPolicy, error) {
	clone, err := agent.CloneDecisionPolicy(policy)
	if err != nil {
		return DecisionPolicy{}, fmt.Errorf("clone decision policy: %w", err)
	}
	return clone, nil
}

func decisionPolicyDigest(policy DecisionPolicy) (string, error) {
	digest, err := agent.DecisionPolicyDigest(policy)
	if err != nil {
		return "", fmt.Errorf("compute decision policy digest: %w", err)
	}
	return digest, nil
}

func persistDecisionRunEnvelope(ctx context.Context, store ArtifactStore, envelope DecisionRunEnvelope) (ArtifactRef, error) {
	if store == nil {
		return ArtifactRef{}, fmt.Errorf("decision run envelope requires an artifact store")
	}
	if err := envelope.Validate(); err != nil {
		return ArtifactRef{}, err
	}
	data, err := json.Marshal(envelope)
	if err != nil {
		return ArtifactRef{}, fmt.Errorf("encode decision run envelope: %w", err)
	}
	put, err := store.Put(ctx, PutArtifactRequest{
		Content: data, Path: "decisions/envelopes/" + envelope.DecisionID + ".json",
		Kind: "decision_run_envelope", Role: "decision",
		Description: "decision run envelope " + envelope.DecisionID,
		MediaType:   "application/json", RunID: envelope.RunID, TaskID: envelope.TaskID,
		Attempt: envelope.Attempt,
	})
	if err != nil {
		return ArtifactRef{}, fmt.Errorf("persisting decision run envelope: %w", err)
	}
	if strings.TrimSpace(put.ID) == "" || strings.TrimSpace(put.SHA256) == "" {
		return ArtifactRef{}, fmt.Errorf("persisting decision run envelope returned an incomplete artifact reference")
	}
	return put.ArtifactRef, nil
}

func loadDecisionRunEnvelope(ctx context.Context, store ArtifactStore, ref ArtifactRef) (DecisionRunEnvelope, error) {
	if store == nil {
		return DecisionRunEnvelope{}, fmt.Errorf("decision run envelope artifact store is unavailable")
	}
	if strings.TrimSpace(ref.ID) == "" {
		return DecisionRunEnvelope{}, fmt.Errorf("decision run envelope artifact reference is missing")
	}
	resolved, err := store.Resolve(ctx, ref)
	if err != nil {
		return DecisionRunEnvelope{}, fmt.Errorf("resolve decision run envelope: %w", err)
	}
	if err := store.Verify(ctx, resolved); err != nil {
		return DecisionRunEnvelope{}, fmt.Errorf("verify decision run envelope: %w", err)
	}
	reader, err := store.Open(ctx, resolved.ID)
	if err != nil {
		return DecisionRunEnvelope{}, fmt.Errorf("open decision run envelope: %w", err)
	}
	defer func() { _ = reader.Close() }()
	data, err := io.ReadAll(reader)
	if err != nil {
		return DecisionRunEnvelope{}, fmt.Errorf("read decision run envelope: %w", err)
	}
	var envelope DecisionRunEnvelope
	if err := json.Unmarshal(data, &envelope); err != nil {
		return DecisionRunEnvelope{}, fmt.Errorf("decode decision run envelope: %w", err)
	}
	if err := envelope.Validate(); err != nil {
		return DecisionRunEnvelope{}, fmt.Errorf("validate decision run envelope: %w", err)
	}
	if envelope.DecisionID == "" ||
		resolved.TaskID != "" && resolved.TaskID != envelope.TaskID ||
		resolved.RunID != "" && resolved.RunID != envelope.RunID ||
		resolved.Attempt != 0 && resolved.Attempt != envelope.Attempt {
		return DecisionRunEnvelope{}, fmt.Errorf("decision run envelope artifact metadata does not match envelope identity")
	}
	if resolved.ID != ref.ID || resolved.SHA256 != ref.SHA256 {
		return DecisionRunEnvelope{}, fmt.Errorf("decision run envelope artifact reference changed during resolution")
	}
	return envelope, nil
}

func decisionRunEnvelopeEventKey(decisionID, evidenceHash string) string {
	return decisionStageEventKey(decisionID, "run_envelope", evidenceHash)
}

func sameArtifactIdentity(a, b ArtifactRef) bool {
	return strings.TrimSpace(a.ID) != "" && a.ID == b.ID &&
		strings.TrimSpace(a.SHA256) != "" && a.SHA256 == b.SHA256
}

func decisionStageEventKey(decisionID, stage string, parts ...string) string {
	key := "decision:" + strings.TrimSpace(decisionID) + ":" + stage
	for _, part := range parts {
		key += ":" + strings.TrimSpace(part)
	}
	return key
}
