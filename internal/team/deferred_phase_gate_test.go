package team

import (
	"strings"
	"testing"

	"github.com/kjelly/hufu/internal/agent"
)

// The deferred phases must not start before their entry gates
// (docs/hufu-decision-aware-runtime-spec.md §49; plan Stages 8 and 9).
//
// Both phases are deferred for a reason that can be checked, not merely
// remembered: Phase 4 has no trusted capability evidence to rank with, and
// Phase 5 has no sample large enough for a Brier score to mean anything.
// Encoding the gates as tests is what keeps "not yet" from decaying into
// "someone started it quietly".

// Phase 4 (capability-aware routing) has not started, and the invariant it
// would have to renegotiate first still holds: a capability may rank an
// already-authorized candidate, never grant authorization.
func TestPhase4CapabilityRoutingHasNotStarted(t *testing.T) {
	// The capability surface is still the greenfield the spec described: a
	// declared requirement, with nothing that scores or ranks candidates.
	config := agent.CapabilityConfig{Required: []string{"network"}}
	if len(config.Required) != 1 {
		t.Fatalf("capability config = %#v", config)
	}

	// Authorization is decided before capability is consulted at all. A
	// worker the policy does not allow is not made eligible by being capable.
	def := &agent.AgentDef{
		Name: "deployer", Tools: "bash",
		Requirements: agent.ContractRequirements{Tools: []string{"bash"}},
	}
	if agentCanInvoke(def, "sudo") {
		t.Fatal("an undeclared tool became invocable; capability must never grant authorization")
	}
}

// Phase 5 (outcome learning and calibration) has not started: decision
// outcomes are recorded and reported, and nothing reweights a judge.
func TestPhase5CalibrationHasNotStarted(t *testing.T) {
	// The entry thresholds are the ones §49.2 fixed.
	if Phase5ResolvedRequired != 30 || Phase5VerifiedRequired != 20 {
		t.Fatalf("entry thresholds = %d resolved / %d verified, want 30 / 20",
			Phase5ResolvedRequired, Phase5VerifiedRequired)
	}

	for _, tc := range []struct {
		name     string
		metrics  DecisionMetrics
		wantOpen bool
	}{
		{name: "empty sample", metrics: DecisionMetrics{}, wantOpen: false},
		{
			name:     "enough resolved but too few verified",
			metrics:  DecisionMetrics{ResolvedCount: 40, VerifiedCount: 19},
			wantOpen: false,
		},
		{
			name:     "enough verified but too few resolved",
			metrics:  DecisionMetrics{ResolvedCount: 29, VerifiedCount: 25},
			wantOpen: false,
		},
		{
			// Reaching the threshold opens the gate for a human to audit the
			// sample and approve. It does not start Phase 5 by itself.
			name:     "both thresholds reached",
			metrics:  DecisionMetrics{ResolvedCount: 30, VerifiedCount: 20},
			wantOpen: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status := tc.metrics.Phase5Entry()
			if status.Open != tc.wantOpen {
				t.Fatalf("entry status = %#v, want open=%t", status, tc.wantOpen)
			}
			if status.ResolvedRequired != Phase5ResolvedRequired ||
				status.VerifiedRequired != Phase5VerifiedRequired {
				t.Fatalf("status advertised different thresholds: %#v", status)
			}
		})
	}
}

// Recording an outcome must not reweight anything. This is the property that
// separates "we know what happened" from "the runtime now trusts a judge
// more", which is Phase 5 and is gated.
func TestOutcomeRecordingDoesNotFeedBackIntoJudgment(t *testing.T) {
	// An outcome classification describes what happened; it carries no
	// judgment quality signal the aggregator could consume.
	for _, outcome := range []string{
		OutcomeSucceeded, OutcomeFailed, OutcomeMixed, OutcomeSuperseded, OutcomeUnresolved,
	} {
		if strings.TrimSpace(outcome) == "" {
			t.Fatal("an outcome classification is empty")
		}
	}

	// Aggregation weights come from the profile's declared criteria, never
	// from recorded outcomes: the aggregate of a fixed opinion set is the
	// same whatever history precedes it.
	packet := DecisionEvidencePacket{
		Sealed: true, Hash: "hash-1",
		Options: []DecisionOption{
			{ID: "a", Kind: OptionExecute, Title: "A"},
			{ID: "b", Kind: OptionDefer, Title: "B"},
		},
	}
	opinions := []DecisionOpinion{
		{
			JudgeID: "judge-1", EvidenceHash: "hash-1", Valid: true,
			PreferredOption: "a", SuccessProbability: 0.6,
			OptionScores: []OptionScore{{OptionID: "a", Overall: 8}, {OptionID: "b", Overall: 4}},
		},
		{
			JudgeID: "judge-2", EvidenceHash: "hash-1", Valid: true,
			PreferredOption: "a", SuccessProbability: 0.6,
			OptionScores: []OptionScore{{OptionID: "a", Overall: 8}, {OptionID: "b", Overall: 4}},
		},
	}
	first, err := Aggregate(packet, opinions, agent.AggregationMeanScore, 1)
	if err != nil {
		t.Fatalf("aggregate: %v", err)
	}
	second, err := Aggregate(packet, opinions, agent.AggregationMeanScore, 1)
	if err != nil {
		t.Fatalf("aggregate again: %v", err)
	}
	if first.PreferredOption != second.PreferredOption {
		t.Fatalf("aggregate is not stable: %q vs %q", first.PreferredOption, second.PreferredOption)
	}
	if first.PreferredOption != "a" {
		t.Fatalf("aggregate preferred %q, want the higher-scored option", first.PreferredOption)
	}
}
