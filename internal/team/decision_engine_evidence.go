package team

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/kjelly/hufu/internal/agent"
)

// Resolving and sealing a decision's evidence.
//
// Sealing is the moment the decision's inputs become immutable: a material
// change supersedes the hash and makes earlier opinions stale rather than
// editing them (spec §15.4). Moved out of decision_engine.go unchanged.

func (e *decisionEngine) resolveDecisionEvidence(ctx context.Context, req *DecisionRequest) error {
	if req == nil {
		return fmt.Errorf("%s: decision request is nil", ReasonDecisionOutsideViewMissing)
	}
	if _, err := canonicalEncode(req.Facts); err != nil {
		return &GateResult{Reason: ReasonDecisionOutsideViewMissing, Detail: fmt.Sprintf("facts are not canonicalizable: %v", err)}
	}
	if !decisionRequestDeclaresEvidence(*req) {
		return nil
	}
	if e.services.Store == nil {
		return &GateResult{Reason: ReasonDecisionOutsideViewMissing, Detail: "decision evidence requires an artifact store"}
	}
	resolve := func(ref ArtifactRef) (ArtifactRef, ArtifactOriginMetadata, error) {
		resolved, err := e.services.Store.Resolve(ctx, ref)
		if err != nil {
			return ArtifactRef{}, ArtifactOriginMetadata{}, err
		}
		var origin ArtifactOriginMetadata
		if resolver, ok := e.services.Store.(TrustedArtifactMetadataResolver); ok {
			origin, err = resolver.TrustedArtifactMetadata(ctx, resolved)
			if err != nil {
				return ArtifactRef{}, ArtifactOriginMetadata{}, err
			}
			origin.ParentSourceIDs = append([]string(nil), origin.ParentSourceIDs...)
		}
		return resolved, origin, nil
	}
	metadata := make([]RuntimeEvidenceMetadata, 0, len(req.Artifacts)+len(req.BaseRates))
	for i, ref := range req.Artifacts {
		resolved, origin, err := resolve(ref)
		if err != nil {
			return &GateResult{Reason: ReasonDecisionOutsideViewMissing, Detail: fmt.Sprintf("artifacts[%d] cannot be resolved: %v", i, err)}
		}
		req.Artifacts[i] = resolved
		metadata = append(metadata, RuntimeEvidenceMetadata{Artifact: resolved, Origin: origin})
	}
	for i := range req.BaseRates {
		resolved, origin, err := resolve(req.BaseRates[i].Source)
		if err != nil {
			return &GateResult{Reason: ReasonDecisionOutsideViewMissing, Detail: fmt.Sprintf("base_rates[%d] source cannot be resolved: %v", i, err)}
		}
		req.BaseRates[i].Source = resolved
		metadata = append(metadata, RuntimeEvidenceMetadata{Artifact: resolved, Origin: origin})
	}
	for i := range req.Assumptions {
		for j, ref := range req.Assumptions[i].EvidenceRefs {
			resolved, origin, err := resolve(ref)
			if err != nil {
				return &GateResult{Reason: ReasonDecisionOutsideViewMissing, Detail: fmt.Sprintf("assumptions[%d].evidence_refs[%d] cannot be resolved: %v", i, j, err)}
			}
			req.Assumptions[i].EvidenceRefs[j] = resolved
			metadata = append(metadata, RuntimeEvidenceMetadata{Artifact: resolved, Origin: origin})
		}
	}
	// Every resolved artifact is a possible trusted evidence origin. This must
	// include direct artifacts, base-rate sources, and assumption evidence;
	// limiting it to req.Artifacts silently drops the latter two classes from
	// independence accounting.
	req.trustedProvenance = TrustedProvenanceFromRuntimeMetadata(metadata)
	return nil
}

// sealEvidence seals the packet, or reuses the already-sealed one when its
// material content is unchanged. A material change supersedes the old hash and
// makes the earlier opinions stale rather than editing them (spec §15.4).
func (e *decisionEngine) sealEvidence(ctx context.Context, req DecisionRequest, state decisionState) (DecisionEvidencePacket, ArtifactRef, error) {
	packet := DecisionEvidencePacket{
		ID:                 req.DecisionID + "-evidence",
		Question:           req.Question,
		Options:            req.Options,
		Criteria:           req.Policy.Criteria,
		Facts:              req.Facts,
		Artifacts:          req.Artifacts,
		BaseRates:          req.BaseRates,
		Assumptions:        req.Assumptions,
		Provenance:         req.Provenance,
		RequestContractRef: req.RequestContractRef,
		CreatedAt:          e.now(),
	}
	if err := packet.Validate(); err != nil {
		return DecisionEvidencePacket{}, ArtifactRef{}, fmt.Errorf("decision %s evidence: %w", req.DecisionID, err)
	}
	sealed, err := packet.Seal()
	if err != nil {
		return DecisionEvidencePacket{}, ArtifactRef{}, err
	}
	if state.Packet.Hash == sealed.Hash {
		// Already sealed on identical material; keep the durable packet so
		// CreatedAt and ID stay exactly what the log recorded.
		if state.EvidenceArtifact.ID != "" {
			if req.EvidenceArtifactRef.ID != "" && !sameArtifactIdentity(req.EvidenceArtifactRef, state.EvidenceArtifact) {
				return DecisionEvidencePacket{}, ArtifactRef{}, fmt.Errorf("decision %s evidence artifact identity changed during resume", req.DecisionID)
			}
			return state.Packet, state.EvidenceArtifact, nil
		}
		if req.EvidenceArtifactRef.ID != "" {
			return state.Packet, req.EvidenceArtifactRef, nil
		}
		if e.services.Store == nil {
			return state.Packet, ArtifactRef{}, nil
		}
		artifact, err := persistDecisionEvidence(ctx, e.services.Store, req, state.Packet)
		if err != nil {
			return DecisionEvidencePacket{}, ArtifactRef{}, err
		}
		event := decisionEventFor(req, "evidence_sealed", state.Packet.Hash)
		event.EvidenceHash, event.Packet, event.EvidenceArtifact = state.Packet.Hash, &state.Packet, artifact
		if err := appendDecisionEvent(ctx, e.services.Journal, agent.EventDecisionEvidenceSealed, event); err != nil {
			return DecisionEvidencePacket{}, ArtifactRef{}, err
		}
		return state.Packet, artifact, nil
	}
	if e.services.Store == nil {
		if decisionRequestDeclaresEvidence(req) {
			return DecisionEvidencePacket{}, ArtifactRef{}, &GateResult{Reason: ReasonDecisionOutsideViewMissing, Detail: "decision evidence artifact store is unavailable"}
		}
		if state.Packet.Hash != "" {
			event := decisionEventFor(req, "evidence_changed", sealed.Hash)
			event.EvidenceHash, event.Reason = sealed.Hash, evidenceChangeReason(state.Packet, sealed)
			if err := appendDecisionEvent(ctx, e.services.Journal, agent.EventDecisionEvidenceChanged, event); err != nil {
				return DecisionEvidencePacket{}, ArtifactRef{}, err
			}
		}
		event := decisionEventFor(req, "evidence_sealed", sealed.Hash)
		event.EvidenceHash, event.Packet = sealed.Hash, &sealed
		if err := appendDecisionEvent(ctx, e.services.Journal, agent.EventDecisionEvidenceSealed, event); err != nil {
			return DecisionEvidencePacket{}, ArtifactRef{}, err
		}
		return sealed, ArtifactRef{}, nil
	}
	put, err := persistDecisionEvidence(ctx, e.services.Store, req, sealed)
	if err != nil {
		return DecisionEvidencePacket{}, ArtifactRef{}, err
	}
	if state.Packet.Hash != "" {
		event := decisionEventFor(req, "evidence_changed", sealed.Hash)
		event.EvidenceHash, event.Reason = sealed.Hash, evidenceChangeReason(state.Packet, sealed)
		if err := appendDecisionEvent(ctx, e.services.Journal, agent.EventDecisionEvidenceChanged, event); err != nil {
			return DecisionEvidencePacket{}, ArtifactRef{}, err
		}
	}
	event := decisionEventFor(req, "evidence_sealed", sealed.Hash)
	event.EvidenceHash, event.Packet, event.EvidenceArtifact = sealed.Hash, &sealed, put
	if err := appendDecisionEvent(ctx, e.services.Journal, agent.EventDecisionEvidenceSealed, event); err != nil {
		return DecisionEvidencePacket{}, ArtifactRef{}, err
	}
	return sealed, put, nil
}

// sealDecisionEvidence seals the packet and records the assumptions the sealed
// decision rests on. They belong together: an assumption becomes part of the
// decision at the moment its evidence is sealed, which is the beginning every
// later status change is a transition from (§18.1).
func (e *decisionEngine) sealDecisionEvidence(ctx context.Context, req DecisionRequest, state decisionState) (DecisionEvidencePacket, ArtifactRef, error) {
	packet, artifact, err := e.sealEvidence(ctx, req, state)
	if err != nil {
		return DecisionEvidencePacket{}, ArtifactRef{}, err
	}
	if err := e.declareAssumptions(ctx, req, packet); err != nil {
		return DecisionEvidencePacket{}, ArtifactRef{}, err
	}
	return packet, artifact, nil
}

// declareAssumptions records each assumption the sealed decision rests on.
// It is idempotent through the event key, so a resumed decision does not
// re-declare what it already declared.
func (e *decisionEngine) declareAssumptions(ctx context.Context, req DecisionRequest, packet DecisionEvidencePacket) error {
	if e.services.Journal == nil {
		return nil
	}
	for _, assumption := range req.Assumptions {
		id := strings.TrimSpace(assumption.ID)
		if id == "" {
			continue
		}
		event := decisionEventFor(req, "assumption_declared", packet.Hash, id)
		event.EvidenceHash = packet.Hash
		event.AssumptionID = id
		event.Reason = assumption.Statement
		event.IdempotencyKey = decisionStageEventKey(req.DecisionID, "assumption_declared", packet.Hash, id)
		if err := appendDecisionEvent(ctx, e.services.Journal, agent.EventAssumptionDeclared, event); err != nil {
			return fmt.Errorf("declaring assumption %q: %w", id, err)
		}
	}
	return nil
}

func decisionRequestDeclaresEvidence(req DecisionRequest) bool {
	if len(req.Facts) > 0 || len(req.Artifacts) > 0 || len(req.BaseRates) > 0 || len(req.Provenance) > 0 {
		return true
	}
	for _, assumption := range req.Assumptions {
		if len(assumption.EvidenceRefs) > 0 {
			return true
		}
	}
	return false
}

func persistDecisionEvidence(ctx context.Context, store ArtifactStore, req DecisionRequest, packet DecisionEvidencePacket) (ArtifactRef, error) {
	data, err := json.Marshal(packet)
	if err != nil {
		return ArtifactRef{}, fmt.Errorf("encode decision evidence artifact: %w", err)
	}
	put, err := store.Put(ctx, PutArtifactRequest{
		Kind: "decision_evidence", Role: "decision", Path: "decisions/evidence/" + packet.ID + ".json",
		Description: "sealed decision evidence " + packet.ID, MediaType: "application/json", Content: data,
		RunID: req.RunID, TaskID: req.TaskID, Attempt: req.Attempt,
	})
	if err != nil {
		return ArtifactRef{}, fmt.Errorf("persisting decision evidence: %w", err)
	}
	return put.ArtifactRef, nil
}
