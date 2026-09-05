package team

import (
	"fmt"
	"math"
	"sort"
)

// Numeric semantics for the decision runtime
// (docs/hufu-decision-aware-runtime-spec.md §14).
//
// Every rule here exists because "deterministic aggregation" is otherwise not
// decidable: without a fixed scale, weight normalization, missing-value
// handling, iteration order and rounding, the same opinions can produce
// different aggregates on different machines.

const (
	// decisionScoreMin/Max define the only supported score scale in V1.
	decisionScoreMin = 0.0
	decisionScoreMax = 10.0

	// decisionUnitMin/Max bound probabilities, confidence and severity.
	decisionUnitMin = 0.0
	decisionUnitMax = 1.0

	// decisionRoundDigits is the persisted precision. Values are rounded
	// half-away-from-zero before they are written or compared so a threshold
	// check never depends on the last float bit.
	decisionRoundDigits = 6
)

var decisionRoundFactor = math.Pow(10, decisionRoundDigits)

// roundDecision rounds half away from zero to the persisted precision.
// math.Round already rounds halves away from zero.
func roundDecision(v float64) float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return v
	}
	return math.Round(v*decisionRoundFactor) / decisionRoundFactor
}

// validScore reports whether v is a usable score. Out-of-range, NaN and Inf
// values are invalid and must never be clamped: clamping turns a model's
// formatting error into a plausible-looking judgment (spec §14.1).
func validScore(v float64) bool {
	return !math.IsNaN(v) && !math.IsInf(v, 0) && v >= decisionScoreMin && v <= decisionScoreMax
}

// validUnit reports whether v is a usable probability/confidence/severity.
func validUnit(v float64) bool {
	return !math.IsNaN(v) && !math.IsInf(v, 0) && v >= decisionUnitMin && v <= decisionUnitMax
}

// normalizedWeights returns w_i / sum(w_j), rounded to the persisted precision
// so the same weights are used for scoring, hashing and reporting (spec §14.2,
// §15.2). Config validation has already rejected non-positive and non-finite
// weights and guaranteed a positive sum.
func normalizedWeights(criteria []DecisionCriterion) (map[string]float64, error) {
	if len(criteria) == 0 {
		return nil, nil
	}
	var total float64
	for _, c := range criteria {
		if math.IsNaN(c.Weight) || math.IsInf(c.Weight, 0) || c.Weight <= 0 {
			return nil, fmt.Errorf("criterion %q has a non-positive or non-finite weight %v", c.ID, c.Weight)
		}
		total += c.Weight
	}
	if total <= 0 {
		return nil, fmt.Errorf("criteria weights sum to %v, want > 0", total)
	}
	weights := make(map[string]float64, len(criteria))
	for _, c := range criteria {
		weights[c.ID] = roundDecision(c.Weight / total)
	}
	return weights, nil
}

// computeOverall derives an option's overall score from its criterion scores.
// Every configured criterion must be scored; a missing or invalid entry makes
// the whole opinion invalid rather than silently scoring zero (spec §14.3).
// Iteration follows ascending criterion ID so float summation order is fixed
// across runs and platforms (spec §14.4).
func computeOverall(scores map[string]float64, weights map[string]float64) (float64, error) {
	if len(weights) == 0 {
		return 0, fmt.Errorf("no configured criteria to score against")
	}
	ids := make([]string, 0, len(weights))
	for id := range weights {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	var overall float64
	for _, id := range ids {
		score, ok := scores[id]
		if !ok {
			return 0, fmt.Errorf("criterion %q was not scored", id)
		}
		if !validScore(score) {
			return 0, fmt.Errorf("criterion %q score %v is outside [0, 10] or not finite", id, score)
		}
		overall += weights[id] * score
	}
	return roundDecision(overall), nil
}

// sortedFloats returns a sorted copy, used by the order-sensitive statistics.
func sortedFloats(values []float64) []float64 {
	out := append([]float64(nil), values...)
	sort.Float64s(out)
	return out
}

// meanOf returns the arithmetic mean. Callers guarantee a non-empty slice.
func meanOf(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}
	var sum float64
	for _, v := range sortedFloats(values) {
		sum += v
	}
	return roundDecision(sum / float64(len(values)))
}

// medianOf returns the median; an even count averages the middle two.
func medianOf(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}
	sorted := sortedFloats(values)
	mid := len(sorted) / 2
	if len(sorted)%2 == 1 {
		return roundDecision(sorted[mid])
	}
	return roundDecision((sorted[mid-1] + sorted[mid]) / 2)
}

// stdDevOf returns the population standard deviation (denominator N), in score
// units. A single judge has zero dispersion by definition (spec §14.5).
func stdDevOf(values []float64) float64 {
	if len(values) < 2 {
		return 0
	}
	mean := meanOf(values)
	var sum float64
	for _, v := range sortedFloats(values) {
		delta := v - mean
		sum += delta * delta
	}
	return roundDecision(math.Sqrt(sum / float64(len(values))))
}

// madOf returns the median absolute deviation, in score units.
func madOf(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}
	median := medianOf(values)
	deviations := make([]float64, 0, len(values))
	for _, v := range values {
		deviations = append(deviations, math.Abs(v-median))
	}
	return roundDecision(medianOf(deviations))
}

// minOf and maxOf return the extremes of a non-empty slice.
func minOf(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}
	sorted := sortedFloats(values)
	return roundDecision(sorted[0])
}

func maxOf(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}
	sorted := sortedFloats(values)
	return roundDecision(sorted[len(sorted)-1])
}
