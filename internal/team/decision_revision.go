package team

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/kjelly/hufu/internal/agent"
)

// The single bounded revision round
// (docs/hufu-decision-aware-runtime-spec.md §25).
//
// Round 1 is independent judgment. After the challenge, each original judge may
// revise once, on its own. It sees the aggregate, the challenge and its own
// earlier opinion — never another judge's individual view — so the round is a
// second independent look, not a negotiation toward consensus.

// RevisionRequest is one judge's revision dispatch.
type RevisionRequest struct {
	DecisionID string
	JudgeID    string
	Packet     DecisionEvidencePacket
	Original   DecisionOpinion
	Prompt     string
	// RoutingRole selects real capability-routed execution for this
	// revision when non-nil. It is JudgeRolePolicy, not a separate
	// revision-role: REVISE reuses JUDGE's binding rather than re-resolving
	// (spec2.md §8) — resolving the same (role, ordinal) pair a second time
	// against the unchanged, deterministic candidate list yields the exact
	// same candidate JUDGE round 1 did, with no persisted binding required.
	RoutingRole *agent.JudgeRolePolicy
}

// RevisionRunner executes one judge's revision.
type RevisionRunner interface {
	RunRevision(ctx context.Context, req RevisionRequest) (DecisionRevision, error)
}

// BuildRevisionPrompt renders one judge's revision input.
func BuildRevisionPrompt(
	packet DecisionEvidencePacket,
	aggregate DecisionAggregate,
	challenges []DecisionChallenge,
	original DecisionOpinion,
) string {
	var b strings.Builder
	fmt.Fprintf(&b, "You are judge %s revising your own judgment once (round 2).\n", original.JudgeID)
	b.WriteString("You are seeing the runtime's aggregate and the challenge, plus your own earlier scores.\n")
	b.WriteString("You are not seeing any other judge's individual view: this is a second independent look, not a negotiation.\n")
	b.WriteString("Changing nothing is a valid outcome.\n\n")
	fmt.Fprintf(&b, "Evidence hash: %s\n\n", packet.Hash)
	b.WriteString(renderSealedEvidence(packet))

	b.WriteString("## Aggregate of round 1 (computed by the runtime)\n")
	for _, id := range packet.OptionIDs() {
		mean, ok := aggregate.MeanScores[id]
		if !ok {
			continue
		}
		fmt.Fprintf(&b, "- %s: mean %.4f, stddev %.4f\n", id, mean, aggregate.StdDev[id])
	}
	fmt.Fprintf(&b, "Leading option: %s\n\n", aggregate.PreferredOption)

	if len(challenges) > 0 {
		b.WriteString("## Challenge\n")
		ordered := append([]DecisionChallenge(nil), challenges...)
		sort.Slice(ordered, func(i, j int) bool { return ordered[i].ID < ordered[j].ID })
		for _, challenge := range ordered {
			fmt.Fprintf(&b, "- against %s (severity %.4f): %s\n",
				challenge.TargetOption, challenge.Severity, strings.TrimSpace(challenge.StrongestCountercase))
			for _, assumption := range challenge.FragileAssumptions {
				fmt.Fprintf(&b, "  fragile assumption: %s\n", strings.TrimSpace(assumption))
			}
			for _, missing := range challenge.MissingEvidence {
				fmt.Fprintf(&b, "  missing evidence: %s\n", strings.TrimSpace(missing))
			}
		}
		b.WriteString("\n")
	}

	b.WriteString("## Your round 1 scores\n")
	for _, score := range original.OptionScores {
		fmt.Fprintf(&b, "- %s: overall %.4f\n", score.OptionID, score.Overall)
	}
	fmt.Fprintf(&b, "You preferred %s with probability %.4f.\n\n", original.PreferredOption, original.SuccessProbability)

	b.WriteString("## Required response\nReturn a single JSON object, no prose around it:\n")
	b.WriteString("{\n")
	b.WriteString(`  "revised_scores": [{"option_id": "...", "criteria": {"<criterion id>": 0.0}}],` + "\n")
	b.WriteString(`  "revised_probability": 0.0,` + "\n")
	b.WriteString(`  "changed": false,` + "\n")
	b.WriteString(`  "reason": "..."` + "\n")
	b.WriteString("}\n")
	b.WriteString("Set changed to false and repeat your scores if the challenge did not move you.\n")
	return b.String()
}

// ValidateRevision checks a revision's structured output and derives overall
// scores the same way round 1 does.
func ValidateRevision(revision *DecisionRevision, packet DecisionEvidencePacket, weights map[string]float64, original DecisionOpinion) error {
	if revision == nil {
		return fmt.Errorf("nil revision")
	}
	revision.EvidenceHash = packet.Hash
	revision.JudgeID = original.JudgeID
	revision.OriginalOpinionID = original.ID

	if !validUnit(revision.RevisedProbability) {
		return fmt.Errorf("revised_probability %v is outside [0, 1] or not finite", revision.RevisedProbability)
	}
	// An unchanged revision keeps the original scores rather than requiring the
	// judge to retype them.
	if !revision.Changed && len(revision.RevisedScores) == 0 {
		revision.RevisedScores = append([]OptionScore(nil), original.OptionScores...)
		if revision.RevisedProbability == 0 {
			revision.RevisedProbability = original.SuccessProbability
		}
		return nil
	}

	probe := DecisionOpinion{
		JudgeID:            original.JudgeID,
		EvidenceHash:       packet.Hash,
		Round:              2,
		OptionScores:       revision.RevisedScores,
		PreferredOption:    original.PreferredOption,
		SuccessProbability: revision.RevisedProbability,
	}
	if err := ValidateOpinion(&probe, packet, weights); err != nil {
		return err
	}
	revision.RevisedScores = probe.OptionScores
	return nil
}

// RevisedOpinions projects round 2 as a full set of opinions so the same
// deterministic aggregator handles both rounds. A judge that did not revise
// carries its round 1 scores forward unchanged.
func RevisedOpinions(originals []DecisionOpinion, revisions []DecisionRevision) []DecisionOpinion {
	byJudge := make(map[string]DecisionRevision, len(revisions))
	for _, revision := range revisions {
		byJudge[revision.JudgeID] = revision
	}

	out := make([]DecisionOpinion, 0, len(originals))
	for _, original := range originals {
		if !original.Valid {
			continue
		}
		revised := original
		revised.Round = 2
		if revision, ok := byJudge[original.JudgeID]; ok {
			revised.OptionScores = revision.RevisedScores
			revised.SuccessProbability = revision.RevisedProbability
			if revision.Changed {
				revised.PreferredOption = preferredFromScores(revision.RevisedScores, original.PreferredOption)
			}
		}
		out = append(out, revised)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].JudgeID < out[j].JudgeID })
	return out
}

// preferredFromScores re-derives a judge's preference from its revised scores,
// keeping the deterministic tie-break: highest overall, then smallest ID.
func preferredFromScores(scores []OptionScore, fallback string) string {
	best := ""
	var bestScore float64
	ordered := append([]OptionScore(nil), scores...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].OptionID < ordered[j].OptionID })
	for _, score := range ordered {
		if best == "" || score.Overall > bestScore {
			best, bestScore = score.OptionID, score.Overall
		}
	}
	if best == "" {
		return fallback
	}
	return best
}
