package team

import (
	"math"
	"strings"
	"testing"
)

func TestValidScoreRejectsWithoutClamping(t *testing.T) {
	tests := []struct {
		value float64
		want  bool
	}{
		{0, true}, {5.5, true}, {10, true},
		{-0.0001, false}, {10.0001, false},
		{math.NaN(), false}, {math.Inf(1), false}, {math.Inf(-1), false},
	}
	for _, tt := range tests {
		if got := validScore(tt.value); got != tt.want {
			t.Errorf("validScore(%v) = %v, want %v", tt.value, got, tt.want)
		}
	}
}

func TestValidUnit(t *testing.T) {
	for _, tt := range []struct {
		value float64
		want  bool
	}{{0, true}, {1, true}, {0.5, true}, {1.0001, false}, {-0.1, false}, {math.NaN(), false}} {
		if got := validUnit(tt.value); got != tt.want {
			t.Errorf("validUnit(%v) = %v, want %v", tt.value, got, tt.want)
		}
	}
}

func TestRoundDecisionHalfAwayFromZero(t *testing.T) {
	tests := []struct {
		in   float64
		want float64
	}{
		{1.0000005, 1.000001},
		{-1.0000005, -1.000001},
		{2.3333333333, 2.333333},
		{10, 10},
	}
	for _, tt := range tests {
		if got := roundDecision(tt.in); got != tt.want {
			t.Errorf("roundDecision(%v) = %v, want %v", tt.in, got, tt.want)
		}
	}
	if got := roundDecision(math.NaN()); !math.IsNaN(got) {
		t.Errorf("roundDecision(NaN) = %v, want NaN preserved for the validator to reject", got)
	}
}

// Weights are normalized to sum to one so a profile's relative emphasis, not
// its absolute numbers, decides the score (spec §14.2).
func TestNormalizedWeights(t *testing.T) {
	weights, err := normalizedWeights([]DecisionCriterion{
		{ID: "cost", Weight: 2}, {ID: "risk", Weight: 1}, {ID: "speed", Weight: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]float64{"cost": 0.5, "risk": 0.25, "speed": 0.25}
	for id, expected := range want {
		if weights[id] != expected {
			t.Errorf("weights[%q] = %v, want %v", id, weights[id], expected)
		}
	}

	if _, err := normalizedWeights([]DecisionCriterion{{ID: "cost", Weight: 0}}); err == nil {
		t.Fatal("zero weight accepted")
	}
	if _, err := normalizedWeights([]DecisionCriterion{{ID: "cost", Weight: math.NaN()}}); err == nil {
		t.Fatal("NaN weight accepted")
	}
	if weights, err := normalizedWeights(nil); err != nil || weights != nil {
		t.Fatalf("normalizedWeights(nil) = %v, %v, want nil, nil", weights, err)
	}
}

func TestComputeOverall(t *testing.T) {
	weights := map[string]float64{"cost": 0.5, "risk": 0.25, "speed": 0.25}

	overall, err := computeOverall(map[string]float64{"cost": 8, "risk": 4, "speed": 6}, weights)
	if err != nil {
		t.Fatal(err)
	}
	if overall != 6.5 {
		t.Fatalf("computeOverall = %v, want 6.5", overall)
	}

	if _, err := computeOverall(map[string]float64{"cost": 8, "risk": 4}, weights); err == nil ||
		!strings.Contains(err.Error(), "was not scored") {
		t.Fatalf("missing criterion error = %v, want a not-scored rejection", err)
	}
	if _, err := computeOverall(map[string]float64{"cost": 11, "risk": 4, "speed": 6}, weights); err == nil {
		t.Fatal("out-of-range score accepted")
	}
	if _, err := computeOverall(map[string]float64{"cost": math.Inf(1), "risk": 4, "speed": 6}, weights); err == nil {
		t.Fatal("infinite score accepted")
	}
	if _, err := computeOverall(map[string]float64{"cost": 8}, nil); err == nil {
		t.Fatal("computeOverall with no criteria accepted")
	}
}

// Float summation order changes results; the aggregator therefore iterates in
// a fixed order. This asserts the property directly (spec §14.4).
func TestComputeOverallIsOrderIndependent(t *testing.T) {
	weights := map[string]float64{"a": 0.1, "b": 0.2, "c": 0.3, "d": 0.4}
	scores := map[string]float64{"a": 1.1, "b": 2.7, "c": 3.3, "d": 9.9}

	first, err := computeOverall(scores, weights)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 50; i++ {
		again, err := computeOverall(scores, weights)
		if err != nil {
			t.Fatal(err)
		}
		if again != first {
			t.Fatalf("iteration %d gave %v, want %v", i, again, first)
		}
	}
}

func TestDispersionStatistics(t *testing.T) {
	values := []float64{2, 4, 4, 4, 5, 5, 7, 9}
	if got := meanOf(values); got != 5 {
		t.Errorf("meanOf = %v, want 5", got)
	}
	if got := medianOf(values); got != 4.5 {
		t.Errorf("medianOf = %v, want 4.5", got)
	}
	if got := stdDevOf(values); got != 2 {
		t.Errorf("stdDevOf = %v, want the population deviation 2", got)
	}
	if got := minOf(values); got != 2 {
		t.Errorf("minOf = %v, want 2", got)
	}
	if got := maxOf(values); got != 9 {
		t.Errorf("maxOf = %v, want 9", got)
	}
	if got := madOf(values); got != 0.5 {
		t.Errorf("madOf = %v, want 0.5", got)
	}

	// A single judge has no dispersion by definition (spec §14.5).
	if got := stdDevOf([]float64{7}); got != 0 {
		t.Errorf("stdDevOf(single) = %v, want 0", got)
	}
	if got := medianOf([]float64{7, 8}); got != 7.5 {
		t.Errorf("medianOf(even) = %v, want the middle two averaged", got)
	}
}

func TestStatisticsAreOrderIndependent(t *testing.T) {
	ascending := []float64{1, 2, 3, 4, 5}
	descending := []float64{5, 4, 3, 2, 1}
	shuffled := []float64{3, 1, 5, 2, 4}
	for _, fn := range []struct {
		name string
		f    func([]float64) float64
	}{
		{"mean", meanOf}, {"median", medianOf}, {"stddev", stdDevOf},
		{"mad", madOf}, {"min", minOf}, {"max", maxOf},
	} {
		a, b, c := fn.f(ascending), fn.f(descending), fn.f(shuffled)
		if a != b || b != c {
			t.Errorf("%s is order dependent: %v, %v, %v", fn.name, a, b, c)
		}
	}
}
