package team

import (
	"encoding/json"
	"math"
	"strings"
	"testing"

	"github.com/kjelly/hufu/internal/agent"
)

func aggregatePacket(t *testing.T) DecisionEvidencePacket {
	t.Helper()
	return mustSeal(t, DecisionEvidencePacket{
		Question: "Ship?",
		Options: []DecisionOption{
			{ID: "a", Kind: OptionExecute},
			{ID: "b", Kind: OptionDefer},
		},
		Criteria: []DecisionCriterion{{ID: "cost", Weight: 1}, {ID: "risk", Weight: 1}},
	})
}

func opinion(judge string, hash string, a, b [2]float64, preferred string, probability float64) DecisionOpinion {
	return DecisionOpinion{
		JudgeID: judge, EvidenceHash: hash, Round: 1,
		OptionScores: []OptionScore{
			{OptionID: "a", Criteria: map[string]float64{"cost": a[0], "risk": a[1]}},
			{OptionID: "b", Criteria: map[string]float64{"cost": b[0], "risk": b[1]}},
		},
		PreferredOption: preferred, SuccessProbability: probability,
	}
}

func validated(t *testing.T, packet DecisionEvidencePacket, weights map[string]float64, opinions ...DecisionOpinion) []DecisionOpinion {
	t.Helper()
	out := make([]DecisionOpinion, 0, len(opinions))
	for i := range opinions {
		o := opinions[i]
		if err := ValidateOpinion(&o, packet, weights); err != nil {
			t.Fatalf("ValidateOpinion(%s) = %v", o.JudgeID, err)
		}
		o.Valid = true
		out = append(out, o)
	}
	return out
}

// A judge-supplied overall is ignored when criteria are configured: the runtime
// owns the arithmetic (spec §14.2).
func TestValidateOpinionComputesOverall(t *testing.T) {
	packet := aggregatePacket(t)
	weights, err := normalizedWeights(packet.Criteria)
	if err != nil {
		t.Fatal(err)
	}
	o := opinion("judge-1", packet.Hash, [2]float64{8, 6}, [2]float64{4, 4}, "a", 0.7)
	o.OptionScores[0].Overall = 9.9

	if !JudgeSuppliedOverall(o, weights) {
		t.Fatal("JudgeSuppliedOverall = false, want true so the runtime can record that it ignored it")
	}
	if err := ValidateOpinion(&o, packet, weights); err != nil {
		t.Fatal(err)
	}
	if o.OptionScores[0].Overall != 7 {
		t.Fatalf("overall = %v, want the runtime-computed 7", o.OptionScores[0].Overall)
	}
}

func TestValidateOpinionRejections(t *testing.T) {
	packet := aggregatePacket(t)
	weights, _ := normalizedWeights(packet.Criteria)

	tests := []struct {
		name    string
		mutate  func(*DecisionOpinion)
		wantErr string
	}{
		{name: "baseline", mutate: func(*DecisionOpinion) {}},
		{
			name:    "wrong evidence hash",
			mutate:  func(o *DecisionOpinion) { o.EvidenceHash = "stale" },
			wantErr: "not the sealed",
		},
		{
			name:    "probability out of range",
			mutate:  func(o *DecisionOpinion) { o.SuccessProbability = 1.5 },
			wantErr: "success_probability",
		},
		{
			name:    "NaN probability",
			mutate:  func(o *DecisionOpinion) { o.SuccessProbability = math.NaN() },
			wantErr: "success_probability",
		},
		{
			name:    "confidence out of range",
			mutate:  func(o *DecisionOpinion) { o.Confidence = 2 },
			wantErr: "confidence",
		},
		{
			name:    "unknown preferred option",
			mutate:  func(o *DecisionOpinion) { o.PreferredOption = "c" },
			wantErr: "not an option in the sealed evidence",
		},
		{
			name:    "unknown scored option",
			mutate:  func(o *DecisionOpinion) { o.OptionScores[0].OptionID = "c" },
			wantErr: "unknown option",
		},
		{
			name: "duplicate option score",
			mutate: func(o *DecisionOpinion) {
				o.OptionScores = append(o.OptionScores, o.OptionScores[0])
			},
			wantErr: "scored twice",
		},
		{
			name:    "missing criterion",
			mutate:  func(o *DecisionOpinion) { delete(o.OptionScores[0].Criteria, "risk") },
			wantErr: "was not scored",
		},
		{
			name:    "missing option",
			mutate:  func(o *DecisionOpinion) { o.OptionScores = o.OptionScores[:1] },
			wantErr: `option "b" was not scored`,
		},
		{
			name:    "score above scale",
			mutate:  func(o *DecisionOpinion) { o.OptionScores[0].Criteria["cost"] = 11 },
			wantErr: "outside [0, 10]",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			o := opinion("judge-1", packet.Hash, [2]float64{8, 6}, [2]float64{4, 4}, "a", 0.7)
			tt.mutate(&o)
			err := ValidateOpinion(&o, packet, weights)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("ValidateOpinion = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("ValidateOpinion = %v, want %q", err, tt.wantErr)
			}
		})
	}
}

// Out-of-range values must be rejected, never clamped into a plausible score.
func TestValidateOpinionNeverClamps(t *testing.T) {
	packet := aggregatePacket(t)
	weights, _ := normalizedWeights(packet.Criteria)
	o := opinion("judge-1", packet.Hash, [2]float64{12, 6}, [2]float64{4, 4}, "a", 0.7)
	if err := ValidateOpinion(&o, packet, weights); err == nil {
		t.Fatal("out-of-range score accepted")
	}
	if o.OptionScores[0].Criteria["cost"] != 12 {
		t.Fatalf("score was mutated to %v; rejection must not clamp", o.OptionScores[0].Criteria["cost"])
	}
}

func TestAggregateStatistics(t *testing.T) {
	packet := aggregatePacket(t)
	weights, _ := normalizedWeights(packet.Criteria)
	opinions := validated(t, packet, weights,
		opinion("judge-1", packet.Hash, [2]float64{8, 8}, [2]float64{4, 4}, "a", 0.8),
		opinion("judge-2", packet.Hash, [2]float64{6, 6}, [2]float64{5, 5}, "a", 0.6),
		opinion("judge-3", packet.Hash, [2]float64{4, 4}, [2]float64{6, 6}, "b", 0.4),
	)

	aggregate, err := Aggregate(packet, opinions, agent.AggregationMeanScore, 1)
	if err != nil {
		t.Fatal(err)
	}
	if aggregate.JudgeCount != 3 {
		t.Fatalf("JudgeCount = %d, want 3", aggregate.JudgeCount)
	}
	if aggregate.MeanScores["a"] != 6 || aggregate.MeanScores["b"] != 5 {
		t.Fatalf("mean scores = %v", aggregate.MeanScores)
	}
	if aggregate.MedianScores["a"] != 6 || aggregate.MinScores["a"] != 4 || aggregate.MaxScores["a"] != 8 {
		t.Fatalf("distribution for a = median %v min %v max %v",
			aggregate.MedianScores["a"], aggregate.MinScores["a"], aggregate.MaxScores["a"])
	}
	if got := aggregate.StdDev["a"]; math.Abs(got-1.632993) > 1e-6 {
		t.Fatalf("StdDev[a] = %v, want the population deviation", got)
	}
	if aggregate.PreferredOption != "a" {
		t.Fatalf("PreferredOption = %q, want a", aggregate.PreferredOption)
	}
	// Probability is grouped under the option each judge actually preferred.
	if aggregate.MeanProbability["a"] != 0.7 || aggregate.MeanProbability["b"] != 0.4 {
		t.Fatalf("mean probability = %v", aggregate.MeanProbability)
	}
}

// The same opinions must produce a byte-identical aggregate regardless of the
// order they arrive in (spec §14.4, test matrix C).
func TestAggregateIsDeterministic(t *testing.T) {
	packet := aggregatePacket(t)
	weights, _ := normalizedWeights(packet.Criteria)
	opinions := validated(t, packet, weights,
		opinion("judge-1", packet.Hash, [2]float64{8, 3}, [2]float64{4, 9}, "a", 0.8),
		opinion("judge-2", packet.Hash, [2]float64{6, 7}, [2]float64{5, 2}, "a", 0.6),
		opinion("judge-3", packet.Hash, [2]float64{4, 1}, [2]float64{6, 6}, "b", 0.4),
	)

	first, err := Aggregate(packet, opinions, agent.AggregationMeanScore, 1)
	if err != nil {
		t.Fatal(err)
	}
	want, err := json.Marshal(first)
	if err != nil {
		t.Fatal(err)
	}

	reversed := []DecisionOpinion{opinions[2], opinions[1], opinions[0]}
	shuffled := []DecisionOpinion{opinions[1], opinions[2], opinions[0]}
	for name, ordering := range map[string][]DecisionOpinion{"reversed": reversed, "shuffled": shuffled} {
		aggregate, err := Aggregate(packet, ordering, agent.AggregationMeanScore, 1)
		if err != nil {
			t.Fatal(err)
		}
		got, err := json.Marshal(aggregate)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != string(want) {
			t.Fatalf("%s ordering changed the aggregate:\n got %s\nwant %s", name, got, want)
		}
	}
}

func TestAggregateMethods(t *testing.T) {
	packet := aggregatePacket(t)
	weights, _ := normalizedWeights(packet.Criteria)
	// a wins on mean, b wins on majority vote.
	opinions := validated(t, packet, weights,
		opinion("judge-1", packet.Hash, [2]float64{10, 10}, [2]float64{0, 0}, "a", 0.9),
		opinion("judge-2", packet.Hash, [2]float64{1, 1}, [2]float64{2, 2}, "b", 0.5),
		opinion("judge-3", packet.Hash, [2]float64{1, 1}, [2]float64{2, 2}, "b", 0.5),
	)

	for method, want := range map[string]string{
		agent.AggregationMeanScore:       "a",
		agent.AggregationMedianScore:     "b",
		agent.AggregationMajority:        "b",
		agent.AggregationMeanProbability: "a",
	} {
		aggregate, err := Aggregate(packet, opinions, method, 1)
		if err != nil {
			t.Fatalf("%s: %v", method, err)
		}
		if aggregate.PreferredOption != want {
			t.Errorf("%s preferred %q, want %q", method, aggregate.PreferredOption, want)
		}
	}

	if _, err := Aggregate(packet, opinions, "vibes", 1); err == nil {
		t.Fatal("unsupported aggregation method accepted")
	}
}

// Ties resolve by dispersion, then by the byte-wise smallest option ID, so the
// same inputs never pick different winners on different runs (spec §14.6).
func TestAggregateTieBreak(t *testing.T) {
	packet := aggregatePacket(t)
	weights, _ := normalizedWeights(packet.Criteria)

	// Equal means, b has lower dispersion: b wins on the second level.
	opinions := validated(t, packet, weights,
		opinion("judge-1", packet.Hash, [2]float64{8, 8}, [2]float64{5, 5}, "a", 0.5),
		opinion("judge-2", packet.Hash, [2]float64{2, 2}, [2]float64{5, 5}, "b", 0.5),
	)
	aggregate, err := Aggregate(packet, opinions, agent.AggregationMeanScore, 1)
	if err != nil {
		t.Fatal(err)
	}
	if aggregate.MeanScores["a"] != aggregate.MeanScores["b"] {
		t.Fatalf("fixture is not tied: %v", aggregate.MeanScores)
	}
	if aggregate.PreferredOption != "b" {
		t.Fatalf("PreferredOption = %q, want b (lower dispersion)", aggregate.PreferredOption)
	}

	// Fully identical: the smallest option ID wins.
	identical := validated(t, packet, weights,
		opinion("judge-1", packet.Hash, [2]float64{5, 5}, [2]float64{5, 5}, "a", 0.5),
		opinion("judge-2", packet.Hash, [2]float64{5, 5}, [2]float64{5, 5}, "b", 0.5),
	)
	aggregate, err = Aggregate(packet, identical, agent.AggregationMeanScore, 1)
	if err != nil {
		t.Fatal(err)
	}
	if aggregate.PreferredOption != "a" {
		t.Fatalf("PreferredOption = %q, want the smallest ID a", aggregate.PreferredOption)
	}
}

func TestAggregateExcludesRejectedOpinions(t *testing.T) {
	packet := aggregatePacket(t)
	weights, _ := normalizedWeights(packet.Criteria)
	opinions := validated(t, packet, weights,
		opinion("judge-1", packet.Hash, [2]float64{8, 8}, [2]float64{2, 2}, "a", 0.8),
	)
	opinions = append(opinions, DecisionOpinion{JudgeID: "judge-2", Valid: false, RejectedReason: "invalid"})

	aggregate, err := Aggregate(packet, opinions, agent.AggregationMeanScore, 1)
	if err != nil {
		t.Fatal(err)
	}
	if aggregate.JudgeCount != 1 {
		t.Fatalf("JudgeCount = %d, want only the valid opinion counted", aggregate.JudgeCount)
	}

	if _, err := Aggregate(packet, opinions[1:], agent.AggregationMeanScore, 1); err == nil ||
		!strings.Contains(err.Error(), ReasonDecisionInsufficientValidOpinions) {
		t.Fatalf("Aggregate with no valid opinions = %v, want %s", err, ReasonDecisionInsufficientValidOpinions)
	}
}

func TestChallengeTrigger(t *testing.T) {
	aggregate := DecisionAggregate{PreferredOption: "a", StdDev: map[string]float64{"a": 2.5}}

	if run, _ := ChallengeShouldRun(ChallengePolicy{Enabled: false, Count: 1}, aggregate); run {
		t.Fatal("disabled challenge ran")
	}
	if run, _ := ChallengeShouldRun(ChallengePolicy{Enabled: true, Count: 0}, aggregate); run {
		t.Fatal("zero-count challenge ran")
	}
	if run, _ := ChallengeShouldRun(ChallengePolicy{Enabled: true, Count: 1}, aggregate); !run {
		t.Fatal("challenge without a trigger did not run unconditionally")
	}
	if run, _ := ChallengeShouldRun(ChallengePolicy{Enabled: true, Count: 1, Trigger: &ChallengeTrigger{DispersionAbove: 2.0}}, aggregate); !run {
		t.Fatal("dispersion above the threshold did not trigger challenge")
	}
	run, reason := ChallengeShouldRun(ChallengePolicy{Enabled: true, Count: 1, Trigger: &ChallengeTrigger{DispersionAbove: 3.0}}, aggregate)
	if run {
		t.Fatal("dispersion below the threshold triggered challenge")
	}
	if !strings.Contains(reason, "2.500000") {
		t.Fatalf("skip reason = %q, want the observed dispersion recorded", reason)
	}
	if got := DispersionOf(aggregate); got != 2.5 {
		t.Fatalf("DispersionOf = %v, want the preferred option's deviation", got)
	}
}
