package team

import (
	"fmt"
	"sort"

	"github.com/kjelly/hufu/internal/agent"
)

// Deterministic aggregation (docs/architecture/decision-runtime.md §19-§22).
//
// Aggregation makes zero LLM calls. Given the same opinions it must produce a
// byte-identical DecisionAggregate on any machine, so every collection is
// iterated in ascending ID order and every persisted float is rounded before it
// is stored or compared (spec §14.4).

// ValidateOpinion checks one judge's structured output against the sealed
// packet and the configured criteria, and derives Overall where criteria are
// configured. A returned error makes the opinion invalid; callers repair once
// and then reject it rather than clamping values into range (spec §14.3).
func ValidateOpinion(opinion *DecisionOpinion, packet DecisionEvidencePacket, weights map[string]float64) error {
	if opinion == nil {
		return fmt.Errorf("nil opinion")
	}
	if opinion.EvidenceHash != packet.Hash {
		return fmt.Errorf("opinion %q was formed on evidence %q, not the sealed %q",
			opinion.JudgeID, opinion.EvidenceHash, packet.Hash)
	}
	if !validUnit(opinion.SuccessProbability) {
		return fmt.Errorf("success_probability %v is outside [0, 1] or not finite", opinion.SuccessProbability)
	}
	if opinion.Confidence != 0 && !validUnit(opinion.Confidence) {
		return fmt.Errorf("confidence %v is outside [0, 1] or not finite", opinion.Confidence)
	}
	if opinion.PreferredOption != "" && !packet.HasOption(opinion.PreferredOption) {
		return fmt.Errorf("preferred_option %q is not an option in the sealed evidence", opinion.PreferredOption)
	}

	scored := make(map[string]bool, len(opinion.OptionScores))
	for i := range opinion.OptionScores {
		score := &opinion.OptionScores[i]
		if !packet.HasOption(score.OptionID) {
			return fmt.Errorf("option_scores[%d] scores unknown option %q", i, score.OptionID)
		}
		if scored[score.OptionID] {
			return fmt.Errorf("option %q is scored twice", score.OptionID)
		}
		scored[score.OptionID] = true

		if len(weights) > 0 {
			overall, err := computeOverall(score.Criteria, weights)
			if err != nil {
				return fmt.Errorf("option %q: %w", score.OptionID, err)
			}
			// A judge-supplied Overall is ignored when criteria are
			// configured; the runtime owns the arithmetic (spec §14.2).
			score.Overall = overall
			continue
		}
		if !validScore(score.Overall) {
			return fmt.Errorf("option %q overall %v is outside [0, 10] or not finite", score.OptionID, score.Overall)
		}
		score.Overall = roundDecision(score.Overall)
	}

	for _, id := range packet.OptionIDs() {
		if !scored[id] {
			return fmt.Errorf("option %q was not scored", id)
		}
	}
	sort.Slice(opinion.OptionScores, func(i, j int) bool {
		return opinion.OptionScores[i].OptionID < opinion.OptionScores[j].OptionID
	})
	return nil
}

// JudgeSuppliedOverall reports whether an opinion carried its own Overall value
// while criteria were configured, so the runtime can record that it ignored it.
func JudgeSuppliedOverall(opinion DecisionOpinion, weights map[string]float64) bool {
	if len(weights) == 0 {
		return false
	}
	for _, score := range opinion.OptionScores {
		if score.Overall != 0 {
			return true
		}
	}
	return false
}

// Aggregate computes the deterministic aggregate for one judgment round.
// Only valid opinions participate; rejected ones stay durable for audit but
// never influence the result (spec §14.3).
func Aggregate(packet DecisionEvidencePacket, opinions []DecisionOpinion, method string, round int) (DecisionAggregate, error) {
	valid := make([]DecisionOpinion, 0, len(opinions))
	for _, opinion := range opinions {
		if opinion.Valid {
			valid = append(valid, opinion)
		}
	}
	if len(valid) == 0 {
		return DecisionAggregate{}, fmt.Errorf("%s: no valid opinions to aggregate", ReasonDecisionInsufficientValidOpinions)
	}
	sort.Slice(valid, func(i, j int) bool { return valid[i].JudgeID < valid[j].JudgeID })

	if method == "" {
		method = agent.AggregationMeanScore
	}

	aggregate := DecisionAggregate{
		EvidenceHash:    packet.Hash,
		Round:           round,
		Method:          method,
		JudgeCount:      len(valid),
		MeanScores:      map[string]float64{},
		MedianScores:    map[string]float64{},
		MinScores:       map[string]float64{},
		MaxScores:       map[string]float64{},
		StdDev:          map[string]float64{},
		MAD:             map[string]float64{},
		MeanProbability: map[string]float64{},
	}

	optionIDs := packet.OptionIDs()
	for _, id := range optionIDs {
		overalls := make([]float64, 0, len(valid))
		for _, opinion := range valid {
			for _, score := range opinion.OptionScores {
				if score.OptionID == id {
					overalls = append(overalls, score.Overall)
					break
				}
			}
		}
		if len(overalls) == 0 {
			continue
		}
		aggregate.MeanScores[id] = meanOf(overalls)
		aggregate.MedianScores[id] = medianOf(overalls)
		aggregate.MinScores[id] = minOf(overalls)
		aggregate.MaxScores[id] = maxOf(overalls)
		aggregate.StdDev[id] = stdDevOf(overalls)
		aggregate.MAD[id] = madOf(overalls)
	}

	// MeanProbability groups each judge's success probability under the option
	// that judge actually preferred: a probability of success is a statement
	// about a chosen course of action, not about every option on the table.
	probabilities := map[string][]float64{}
	for _, opinion := range valid {
		if opinion.PreferredOption == "" {
			continue
		}
		probabilities[opinion.PreferredOption] = append(probabilities[opinion.PreferredOption], opinion.SuccessProbability)
	}
	for _, id := range optionIDs {
		if values := probabilities[id]; len(values) > 0 {
			aggregate.MeanProbability[id] = meanOf(values)
		}
	}

	preferred, err := preferredOption(aggregate, valid, method, optionIDs)
	if err != nil {
		return DecisionAggregate{}, err
	}
	aggregate.PreferredOption = preferred
	return aggregate, nil
}

// preferredOption applies the configured aggregator and the deterministic
// three-level tie-break: primary metric, then lowest dispersion, then the
// byte-wise smallest option ID (spec §14.6).
func preferredOption(aggregate DecisionAggregate, valid []DecisionOpinion, method string, optionIDs []string) (string, error) {
	var primary map[string]float64
	switch method {
	case agent.AggregationMeanScore:
		primary = aggregate.MeanScores
	case agent.AggregationMedianScore:
		primary = aggregate.MedianScores
	case agent.AggregationMeanProbability:
		primary = aggregate.MeanProbability
	case agent.AggregationMajority:
		votes := map[string]float64{}
		for _, opinion := range valid {
			if opinion.PreferredOption != "" {
				votes[opinion.PreferredOption]++
			}
		}
		primary = votes
	default:
		return "", fmt.Errorf("aggregation method %q is not supported", method)
	}

	best := ""
	for _, id := range optionIDs {
		value, ok := primary[id]
		if !ok {
			continue
		}
		if best == "" {
			best = id
			continue
		}
		switch {
		case value > primary[best]:
			best = id
		case value == primary[best]:
			// Tie on the primary metric: prefer the option judges agreed on.
			bestDispersion, currentDispersion := aggregate.StdDev[best], aggregate.StdDev[id]
			switch {
			case currentDispersion < bestDispersion:
				best = id
			case currentDispersion == bestDispersion:
				// Still tied: mean score, then the smaller ID. optionIDs is
				// already ascending, so "keep best" resolves the final tie.
				if aggregate.MeanScores[id] > aggregate.MeanScores[best] {
					best = id
				}
			}
		}
	}
	if best == "" {
		return "", fmt.Errorf("%s: no option received a usable aggregate value", ReasonDecisionInsufficientValidOpinions)
	}
	return best, nil
}

// DispersionOf returns the standard deviation of the aggregate's preferred
// option, the value a challenge trigger compares against (spec §14.5, §22).
func DispersionOf(aggregate DecisionAggregate) float64 {
	if aggregate.PreferredOption == "" {
		return 0
	}
	return aggregate.StdDev[aggregate.PreferredOption]
}

// ChallengeShouldRun reports whether the configured challenge stage applies to
// this aggregate, and why it was skipped when it does not (spec §22).
func ChallengeShouldRun(policy ChallengePolicy, aggregate DecisionAggregate) (bool, string) {
	if !policy.Enabled || policy.Count <= 0 {
		return false, ""
	}
	if policy.Trigger == nil {
		return true, ""
	}
	dispersion := DispersionOf(aggregate)
	if dispersion > policy.Trigger.DispersionAbove {
		return true, ""
	}
	return false, fmt.Sprintf("dispersion %.6f did not exceed the configured threshold %.6f",
		dispersion, policy.Trigger.DispersionAbove)
}
