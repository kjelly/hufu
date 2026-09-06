package team

import (
	"context"
	"fmt"

	"github.com/kjelly/hufu/internal/agent"
)

// Post-aggregate decision stages
// (docs/hufu-decision-aware-runtime-spec.md §22-§26).
//
// These run in a fixed order: premortem before the evidence is sealed,
// challenge only after deterministic aggregation, and at most one independent
// revision round. Each stage is resumable: a durable result is reused rather
// than regenerated.

// runPremortem discovers risk before judgment starts. A missing runner is only
// an error when the profile requires a premortem before commit (spec §24).
func (e *decisionEngine) runPremortem(ctx context.Context, req DecisionRequest, policy DecisionPolicy, state decisionState) (*PremortemResult, error) {
	if !policy.Premortem.Enabled {
		return nil, nil
	}
	if state.Premortem != nil {
		return state.Premortem, nil
	}
	if e.services.Premortems == nil {
		if policy.Premortem.RequiredBeforeCommit {
			return nil, &GateResult{
				Reason: ReasonDecisionPremortemRequired,
				Detail: "profile requires a premortem before commit but no premortem runner is configured",
			}
		}
		return nil, nil
	}

	result, err := e.services.Premortems.RunPremortem(ctx, PremortemRequest{
		DecisionID: req.DecisionID,
		Question:   req.Question,
		Options:    req.Options,
		Prompt:     BuildPremortemPrompt(req.Question, req.Options),
	})
	if err != nil {
		if policy.Premortem.RequiredBeforeCommit {
			return nil, fmt.Errorf("%s: %w", ReasonDecisionPremortemRequired, err)
		}
		return nil, nil
	}
	if err := ValidatePremortem(&result); err != nil {
		if policy.Premortem.RequiredBeforeCommit {
			return nil, &GateResult{Reason: ReasonDecisionPremortemRequired, Detail: err.Error()}
		}
		return nil, nil
	}
	result.ID = e.newID("premortem")
	event := decisionEventFor(req, "premortem")
	event.Premortem = &result
	if err := appendDecisionEvent(ctx, e.services.Journal, agent.EventDecisionPremortemSubmitted, event); err != nil {
		return nil, err
	}
	return &result, nil
}

// runChallenges runs the configured challengers after aggregation. A trigger
// that does not fire is recorded with the observed dispersion, so a skipped
// challenge is visible rather than merely absent (spec §22).
func (e *decisionEngine) runChallenges(
	ctx context.Context,
	req DecisionRequest,
	policy DecisionPolicy,
	packet DecisionEvidencePacket,
	aggregate DecisionAggregate,
	opinions []DecisionOpinion,
	state decisionState,
) ([]DecisionChallenge, error) {
	shouldRun, skipReason := ChallengeShouldRun(policy.Challenge, aggregate)
	if !shouldRun {
		if state.ChallengeSkipReason != "" && state.ChallengeSkipEvidenceHash == packet.Hash {
			return nil, nil
		}
		if policy.Challenge.Enabled && skipReason != "" {
			event := decisionEventFor(req, "challenge_skipped", packet.Hash)
			event.EvidenceHash, event.Reason = packet.Hash, skipReason
			if err := appendDecisionEvent(ctx, e.services.Journal, agent.EventDecisionChallengeSkipped, event); err != nil {
				return nil, err
			}
		}
		return nil, nil
	}
	if existing := state.ChallengesForHash(); len(existing) >= policy.Challenge.Count {
		return existing, nil
	}
	if e.services.Challengers == nil {
		return nil, fmt.Errorf("decision %s: challenge is enabled but no challenge runner is configured", req.DecisionID)
	}

	prompt, aliases := BuildChallengePrompt(packet, aggregate, opinions)
	challenges := state.ChallengesForHash()
	for i := len(challenges); i < policy.Challenge.Count; i++ {
		challengerID := fmt.Sprintf("challenger-%d", i+1)
		challenge, err := e.services.Challengers.RunChallenge(ctx, ChallengeRequest{
			DecisionID: req.DecisionID, ChallengerID: challengerID,
			Packet: packet, Aggregate: aggregate, Prompt: prompt,
		})
		if err != nil {
			return nil, fmt.Errorf("decision %s challenge %s: %w", req.DecisionID, challengerID, err)
		}
		if err := ValidateChallenge(&challenge, packet); err != nil {
			return nil, fmt.Errorf("decision %s challenge %s: %w", req.DecisionID, challengerID, err)
		}
		challenge.ID = e.newID("challenge")
		event := decisionEventFor(req, "challenge", packet.Hash, challengerID)
		event.EvidenceHash, event.Challenge, event.JudgeAliases = packet.Hash, &challenge, aliases
		if err := appendDecisionEvent(ctx, e.services.Journal, agent.EventDecisionChallengeSubmitted, event); err != nil {
			return nil, err
		}
		challenges = append(challenges, challenge)
	}
	return challenges, nil
}

// runRevisions runs the single bounded revision round and re-aggregates it.
// Round 2 uses the same deterministic aggregator as round 1 (spec §25).
func (e *decisionEngine) runRevisions(
	ctx context.Context,
	req DecisionRequest,
	policy DecisionPolicy,
	packet DecisionEvidencePacket,
	weights map[string]float64,
	round1 DecisionAggregate,
	challenges []DecisionChallenge,
	opinions []DecisionOpinion,
	state decisionState,
) ([]DecisionRevision, *DecisionAggregate, error) {
	if !policy.Revision.Enabled || policy.EffectiveMaxRounds() < 2 || len(challenges) == 0 {
		return nil, nil, nil
	}
	if aggregate, ok := state.Aggregates[2]; ok && aggregate.EvidenceHash == packet.Hash {
		return state.RevisionsForHash(), &aggregate, nil
	}
	if e.services.Revisions == nil {
		return nil, nil, fmt.Errorf("decision %s: revision is enabled but no revision runner is configured", req.DecisionID)
	}

	existing := map[string]DecisionRevision{}
	for _, revision := range state.RevisionsForHash() {
		existing[revision.JudgeID] = revision
	}

	var revisions []DecisionRevision
	for _, original := range opinions {
		if !original.Valid {
			continue
		}
		if revision, ok := existing[original.JudgeID]; ok {
			revisions = append(revisions, revision)
			continue
		}
		revision, err := e.services.Revisions.RunRevision(ctx, RevisionRequest{
			DecisionID: req.DecisionID, JudgeID: original.JudgeID, Packet: packet, Original: original,
			Prompt: BuildRevisionPrompt(packet, round1, challenges, original),
		})
		if err != nil {
			return nil, nil, fmt.Errorf("decision %s revision %s: %w", req.DecisionID, original.JudgeID, err)
		}
		if err := ValidateRevision(&revision, packet, weights, original); err != nil {
			return nil, nil, fmt.Errorf("decision %s revision %s: %w", req.DecisionID, original.JudgeID, err)
		}
		revision.ID = e.newID("revision")
		event := decisionEventFor(req, "revision", packet.Hash, original.JudgeID, "2")
		event.EvidenceHash, event.JudgeID, event.Round, event.Revision = packet.Hash, original.JudgeID, 2, &revision
		if err := appendDecisionEvent(ctx, e.services.Journal, agent.EventDecisionRevisionSubmitted, event); err != nil {
			return nil, nil, err
		}
		revisions = append(revisions, revision)
	}

	revised := RevisedOpinions(opinions, revisions)
	for i := range revised {
		revised[i].Valid = true
	}
	aggregate, err := Aggregate(packet, revised, policy.EffectiveAggregation(), 2)
	if err != nil {
		return nil, nil, fmt.Errorf("decision %s round 2: %w", req.DecisionID, err)
	}
	aggregate.ID = e.newID("aggregate")
	event := decisionEventFor(req, "aggregate", packet.Hash, "2")
	event.EvidenceHash, event.Round, event.Aggregate = packet.Hash, 2, &aggregate
	if err := appendDecisionEvent(ctx, e.services.Journal, agent.EventDecisionAggregateComputed, event); err != nil {
		return nil, nil, err
	}
	return revisions, &aggregate, nil
}

// applyProvenance records source counts and independence groups. In V1 this is
// advisory: the count is reported, and a shared origin warns rather than blocks
// unless the team explicitly configured a group requirement (spec §28.2).
func (e *decisionEngine) applyProvenance(
	ctx context.Context,
	req DecisionRequest,
	policy DecisionPolicy,
	packet DecisionEvidencePacket,
	record *DecisionRecord,
) error {
	// Request-declared provenance is retained in the sealed packet for audit,
	// but cannot affect runtime grouping. Only provenance derived from verified
	// artifact content is trusted for independence calculations.
	sources := ProvenanceFromArtifacts(packet.Artifacts)
	if len(sources) == 0 {
		return nil
	}
	independence := GroupEvidence(sources, policy.Discipline.Evidence)
	record.SourceCount = independence.SourceCount
	record.IndependenceGroupCount = independence.IndependenceGroupCount
	record.SharedOriginWarnings = independence.SharedOriginWarnings

	for _, warning := range independence.SharedOriginWarnings {
		event := decisionEventFor(req, "shared_origin", packet.Hash, warning)
		event.EvidenceHash, event.Reason = packet.Hash, warning
		if err := appendDecisionEvent(ctx, e.services.Journal, agent.EventDecisionEvidenceSharedOrigin, event); err != nil {
			return err
		}
	}

	required := policy.Discipline.Evidence.RequiredIndependentGroups
	if required > 0 && independence.IndependenceGroupCount < required {
		return &GateResult{
			Reason: ReasonDecisionMissingAlternative,
			Detail: fmt.Sprintf("evidence forms %d independent groups from %d sources, profile requires %d",
				independence.IndependenceGroupCount, independence.SourceCount, required),
		}
	}
	return nil
}
