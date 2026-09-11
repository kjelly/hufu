package team

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"

	"github.com/kjelly/hufu/internal/agent"
)

// The decision run envelope: the durable admission boundary for judge
// execution (docs/architecture/decision-runtime.md §39).
//
// The envelope artifact must reach the content-addressed store before its
// anchor event, and the anchor must reach the journal before the first judge
// dispatch. Moved out of decision_engine.go unchanged.

func (e *decisionEngine) anchorDecisionRun(ctx context.Context, req *DecisionRequest, state *decisionState, policy DecisionPolicy, packet DecisionEvidencePacket, packetArtifact ArtifactRef) error {
	if state.EnvelopeRef.ID != "" {
		return nil
	}
	if e.services.Store == nil {
		return fmt.Errorf("decision %s: judge dispatch requires a durable artifact store and run envelope", req.DecisionID)
	}
	if packetArtifact.ID != "" {
		req.EvidenceArtifactRef = packetArtifact
	}
	envelope := newDecisionRunEnvelope(*req, policy, packet, e.now())
	if admission, found, err := loadDecisionAdmission(ctx, e.services.Journal, req.TaskID, req.Attempt); err != nil {
		return fmt.Errorf("load decision admission before envelope anchor: %w", err)
	} else if found {
		if err := validateDecisionAdmissionEnvelope(admission, envelope); err != nil {
			return err
		}
	}
	envelopeRef, err := persistDecisionRunEnvelope(ctx, e.services.Store, envelope)
	if err != nil {
		return err
	}
	anchor, err := appendDecisionEventResult(ctx, e.services.Journal, agent.EventDecisionRunEnvelopeAnchored, decisionEvent{
		DecisionID: req.DecisionID, RunID: req.RunID, TaskID: req.TaskID, Attempt: req.Attempt,
		EvidenceHash: packet.Hash, EnvelopeRef: envelopeRef, EnvelopeHash: envelopeRef.SHA256,
		FinalizationMode: policy.EffectiveFinalization(), FinalizationJudgeID: policy.Finalization.JudgeID,
		IdempotencyKey: decisionRunEnvelopeEventKey(req.DecisionID, packet.Hash),
	})
	if err != nil {
		return err
	}
	var authoritative decisionEvent
	if err := json.Unmarshal(anchor.Payload, &authoritative); err != nil {
		return fmt.Errorf("decode authoritative decision run envelope anchor: %w", err)
	}
	if err := validateDecisionEnvelopeAnchor(anchor, authoritative, *req, packet, envelopeRef); err != nil {
		return err
	}
	state.EnvelopeRef = authoritative.EnvelopeRef
	return nil
}

func validateDecisionRunRequestIdentity(req DecisionRequest, envelope DecisionRunEnvelope) error {
	if req.DecisionID != "" && req.DecisionID != envelope.DecisionID {
		return fmt.Errorf("decision run envelope decision identity mismatch: request %q, envelope %q", req.DecisionID, envelope.DecisionID)
	}
	if req.RunID != "" && req.RunID != envelope.RunID {
		return fmt.Errorf("decision run envelope run identity mismatch: request %q, envelope %q", req.RunID, envelope.RunID)
	}
	if req.TaskID != "" && req.TaskID != envelope.TaskID {
		return fmt.Errorf("decision run envelope task identity mismatch: request %q, envelope %q", req.TaskID, envelope.TaskID)
	}
	if req.Attempt > 0 && req.Attempt != envelope.Attempt {
		return fmt.Errorf("decision run envelope attempt identity mismatch: request %d, envelope %d", req.Attempt, envelope.Attempt)
	}
	if req.Profile != "" && req.Profile != envelope.Profile {
		return fmt.Errorf("decision run envelope profile mismatch: request %q, envelope %q", req.Profile, envelope.Profile)
	}
	if !reflect.DeepEqual(req.Policy, DecisionPolicy{}) && !reflect.DeepEqual(req.Policy, envelope.Policy) {
		return fmt.Errorf("decision run envelope policy snapshot mismatch")
	}
	if strings.TrimSpace(req.Question) != "" && req.Question != envelope.Request.Question {
		return fmt.Errorf("decision run envelope immutable question mismatch")
	}
	return nil
}

func validateDecisionEnvelopeAnchor(event RunEvent, payload decisionEvent, req DecisionRequest, packet DecisionEvidencePacket, expected ArtifactRef) error {
	if event.RunID != req.RunID || event.TaskID != req.TaskID || event.Attempt != req.Attempt {
		return fmt.Errorf("authoritative decision run envelope anchor identity does not match request")
	}
	if payload.DecisionID != req.DecisionID || payload.RunID != req.RunID || payload.TaskID != req.TaskID || payload.Attempt != req.Attempt {
		return fmt.Errorf("authoritative decision run envelope anchor payload identity does not match request")
	}
	if payload.EvidenceHash != packet.Hash {
		return fmt.Errorf("authoritative decision run envelope anchor evidence identity does not match sealed evidence")
	}
	if !sameArtifactIdentity(payload.EnvelopeRef, expected) || payload.EnvelopeHash != payload.EnvelopeRef.SHA256 {
		return fmt.Errorf("authoritative decision run envelope anchor artifact identity does not match")
	}
	if payload.EnvelopeRef.RunID != req.RunID || payload.EnvelopeRef.TaskID != req.TaskID || payload.EnvelopeRef.Attempt != req.Attempt {
		return fmt.Errorf("authoritative decision run envelope anchor artifact metadata does not match request")
	}
	return nil
}
