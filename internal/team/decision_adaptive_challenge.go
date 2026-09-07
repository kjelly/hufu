package team

import (
	"math"
	"sort"

	"github.com/kjelly/hufu/internal/agent"
)

// Adaptive disagreement-driven challenger routing (spec.md v2 §39-40).
//
// The runtime never picks "an agent who will oppose the winner" — that would
// be routing on a specific agent's identity or opinion, not on capability.
// Instead it looks at where JUDGE round 1's opinions disagreed most (which
// scoring criterion has the highest spread across judges for the leading
// option) and widens CHALLENGE's preferred-capability list with whatever a
// team declares maps to that criterion. Same effect as a goal-driven routing
// hint (decision_routing_hints.go), just keyed by observed disagreement
// instead of the question's own text.

// criterionDispersion computes, for every criterion ID present in every
// valid opinion's score for preferredOption, the population standard
// deviation of that criterion's value across those opinions. A criterion
// missing from even one opinion is excluded rather than guessed at. Fewer
// than two valid opinions produce no meaningful dispersion, so the result is
// nil.
func criterionDispersion(opinions []DecisionOpinion, preferredOption string) map[string]float64 {
	var criteriaPerOpinion []map[string]float64
	for _, opinion := range opinions {
		if !opinion.Valid {
			continue
		}
		for _, score := range opinion.OptionScores {
			if score.OptionID == preferredOption {
				criteriaPerOpinion = append(criteriaPerOpinion, score.Criteria)
				break
			}
		}
	}
	if len(criteriaPerOpinion) < 2 {
		return nil
	}

	presentIn := map[string]int{}
	for _, criteria := range criteriaPerOpinion {
		for id := range criteria {
			presentIn[id]++
		}
	}

	dispersion := make(map[string]float64, len(presentIn))
	for id, count := range presentIn {
		if count != len(criteriaPerOpinion) {
			continue
		}
		var sum float64
		for _, criteria := range criteriaPerOpinion {
			sum += criteria[id]
		}
		mean := sum / float64(len(criteriaPerOpinion))
		var variance float64
		for _, criteria := range criteriaPerOpinion {
			d := criteria[id] - mean
			variance += d * d
		}
		dispersion[id] = math.Sqrt(variance / float64(len(criteriaPerOpinion)))
	}
	return dispersion
}

// mostContestedCriterion returns the criterion ID with the highest
// dispersion, ties broken by criterion ID ascending for determinism. Empty
// or nil input returns "".
func mostContestedCriterion(dispersion map[string]float64) string {
	ids := make([]string, 0, len(dispersion))
	for id := range dispersion {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	best := ""
	bestValue := -1.0
	for _, id := range ids {
		if dispersion[id] > bestValue {
			best, bestValue = id, dispersion[id]
		}
	}
	return best
}

// adaptiveChallengePreferred appends the capabilities mapping maps
// criterionID to onto preferred. It never mutates preferred, and is a no-op
// when criterionID is empty or absent from mapping — the same "pure,
// additive-only" shape applyRoutingHints uses for goal-driven hints.
func adaptiveChallengePreferred(mapping map[string][]string, criterionID string, preferred []string) []string {
	if criterionID == "" || len(mapping) == 0 {
		return preferred
	}
	extra, ok := mapping[criterionID]
	if !ok || len(extra) == 0 {
		return preferred
	}
	augmented := append([]string(nil), preferred...)
	augmented = append(augmented, extra...)
	return augmented
}

// adaptiveChallengeRole returns role with PreferredCapabilities widened by
// whichever capabilities role.AdaptiveCapabilities maps the most contested
// criterion to, or role unchanged if there is no configured mapping or no
// criterion to act on. It never modifies the value role points to.
func adaptiveChallengeRole(role *agent.ChallengeRolePolicy, opinions []DecisionOpinion, aggregate DecisionAggregate) *agent.ChallengeRolePolicy {
	if role == nil || len(role.AdaptiveCapabilities) == 0 {
		return role
	}
	criterionID := mostContestedCriterion(criterionDispersion(opinions, aggregate.PreferredOption))
	if criterionID == "" {
		return role
	}
	augmented := *role
	augmented.PreferredCapabilities = adaptiveChallengePreferred(role.AdaptiveCapabilities, criterionID, role.PreferredCapabilities)
	return &augmented
}
