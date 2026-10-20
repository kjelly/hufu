package paired

import "testing"

func evaluationFixture() Evaluation {
	return Evaluation{
		ExecutionMode: "llm", ComparisonContextDigest: "context",
		Cases: []Case{{ID: "case_a"}}, Replicates: 1, Candidates: []string{"candidate_1"},
		Objective:             Objective{Kind: "quality_gain", MinPassGain: new(int64(1))},
		ProtectedSurfaceValid: true, ResumeConsistent: true,
		Budget: BudgetSummary{SpentTokens: 240, LimitTokens: 1000},
		Observations: []Observation{
			{PairSlot{"baseline", "case_a", 1}, "context", "failed", true, 120, true},
			{PairSlot{"candidate_1", "case_a", 1}, "context", "passed", true, 120, true},
		},
	}
}

func TestFunctionalBaselineFailureCanImprove(t *testing.T) {
	input := evaluationFixture()
	report, err := Evaluate(input)
	if err != nil {
		t.Fatal(err)
	}
	if report.Decision != "eligible_for_review" || report.Coverage.FunctionalFailed != 1 || report.Coverage.Scored != 2 {
		t.Fatalf("functional failure dropped: %+v", report)
	}
	input.ExecutionMode = "fixture"
	report, err = Evaluate(input)
	if err != nil || report.Decision != "simulation_only" || report.SimulationDecision != "eligible_for_review" {
		t.Fatalf("fixture acquired real eligibility: %+v, %v", report, err)
	}
}

func TestIncompleteOrConflictingEvidenceNeverImproves(t *testing.T) {
	for _, test := range []struct {
		name string
		edit func(*Evaluation)
		want string
	}{
		{"missing", func(e *Evaluation) { e.Observations = e.Observations[1:] }, "inconclusive"},
		{"duplicate", func(e *Evaluation) { e.Observations = append(e.Observations, e.Observations[0]) }, "inconclusive"},
		{"foreign", func(e *Evaluation) { e.Observations[0].Slot.Replicate = 2 }, "inconclusive"},
		{"invalid_baseline", func(e *Evaluation) { e.Observations[0].EvidenceValid = false }, "inconclusive"},
		{"unknown_usage", func(e *Evaluation) { e.Observations[0].UsageKnown = false }, "inconclusive"},
		{"mismatch", func(e *Evaluation) { e.Observations[1].ComparisonContextDigest = "other" }, "inconclusive"},
		{"unaccounted_cost", func(e *Evaluation) { e.Budget.SpentTokens = 120 }, "inconclusive"},
		{"inflight", func(e *Evaluation) { e.Budget.ActiveStages = 1 }, "blocked"},
		{"resume", func(e *Evaluation) { e.ResumeConsistent = false }, "blocked"},
		{"changed_tools", func(e *Evaluation) { e.ProtectedSurfaceValid = false }, "rejected"},
		{"overspent", func(e *Evaluation) { e.Budget.SpentTokens = 1001 }, "rejected"},
	} {
		t.Run(test.name, func(t *testing.T) {
			input := evaluationFixture()
			test.edit(&input)
			report, err := Evaluate(input)
			if err != nil || report.Decision != test.want {
				t.Fatalf("got %s, %v; want %s", report.Decision, err, test.want)
			}
		})
	}
}

func TestNoAverageCanHidePairRegression(t *testing.T) {
	input := evaluationFixture()
	input.Cases = append(input.Cases, Case{ID: "case_b"}, Case{ID: "case_c"})
	input.Observations = append(input.Observations,
		Observation{PairSlot{"baseline", "case_b", 1}, "context", "passed", true, 10, true},
		Observation{PairSlot{"candidate_1", "case_b", 1}, "context", "failed", true, 10, true},
		Observation{PairSlot{"baseline", "case_c", 1}, "context", "failed", true, 10, true},
		Observation{PairSlot{"candidate_1", "case_c", 1}, "context", "passed", true, 10, true})
	input.Budget.SpentTokens = 280
	report, err := Evaluate(input)
	if err != nil || report.Decision != "rejected" {
		t.Fatalf("average masked regression: %+v, %v", report, err)
	}
}

func TestBaselineReuseAndStableTieBreak(t *testing.T) {
	input := evaluationFixture()
	input.Candidates = append(input.Candidates, "candidate_2")
	input.Observations = append(input.Observations,
		Observation{PairSlot{"candidate_2", "case_a", 1}, "context", "passed", true, 120, true})
	input.Budget.SpentTokens = 360
	report, err := Evaluate(input)
	if err != nil || report.SelectedVariant != "candidate_1" || report.Coverage.Scored != 3 || report.Budget.SpentTokens != 360 {
		t.Fatalf("reuse or tie-break failed: %+v, %v", report, err)
	}
	input.Objective = Objective{Kind: "token_reduction", MinTokenReductionPercent: new(int64(10))}
	input.Observations[1].AttributableTokens = 108
	report, err = Evaluate(input)
	if err != nil || report.SelectedVariant != "candidate_1" {
		t.Fatalf("integer threshold failed: %+v, %v", report, err)
	}
	input.Observations[1].AttributableTokens = 109
	report, err = Evaluate(input)
	if err != nil || report.Decision != "no_change" || report.SelectedVariant != "baseline" {
		t.Fatalf("below threshold accepted: %+v, %v", report, err)
	}
}
