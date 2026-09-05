package team

import (
	"encoding/json"
	"testing"

	"github.com/kjelly/hufu/internal/agent"
)

func metricEvent(t *testing.T, eventType string, payload decisionEvent) RunEvent {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	return RunEvent{Type: eventType, Actor: decisionActor, Payload: raw}
}

// A workspace with no decisions reports zeroes, not an error.
func TestComputeDecisionMetricsEmpty(t *testing.T) {
	metrics := ComputeDecisionMetrics(nil, nil)
	if metrics.DecisionCount != 0 || metrics.ReplanCount != 0 || metrics.DecisionRevisionRate != 0 {
		t.Fatalf("metrics = %#v, want zeroes", metrics)
	}
	if got := metrics.MeanDispersion(); got != 0 {
		t.Fatalf("MeanDispersion = %v, want 0", got)
	}
	if entry := metrics.Phase5Entry(); entry.Open {
		t.Fatal("Phase 5 reported open on an empty sample")
	}
}

func TestComputeDecisionMetricsFromEvents(t *testing.T) {
	events := []RunEvent{
		metricEvent(t, agent.EventDecisionFinalized, decisionEvent{DecisionID: "dec-1"}),
		metricEvent(t, agent.EventDecisionFinalized, decisionEvent{DecisionID: "dec-2"}),
		metricEvent(t, agent.EventDecisionRevisionSubmitted, decisionEvent{DecisionID: "dec-1"}),
		metricEvent(t, agent.EventDecisionAggregateComputed, decisionEvent{
			DecisionID: "dec-1",
			Aggregate:  &DecisionAggregate{PreferredOption: "a", StdDev: map[string]float64{"a": 2}},
		}),
		metricEvent(t, agent.EventDecisionAggregateComputed, decisionEvent{
			DecisionID: "dec-2",
			Aggregate:  &DecisionAggregate{PreferredOption: "b", StdDev: map[string]float64{"b": 4}},
		}),
		metricEvent(t, agent.EventDecisionOpinionRejected, decisionEvent{DecisionID: "dec-1"}),
		metricEvent(t, agent.EventDecisionBudgetDegraded, decisionEvent{DecisionID: "dec-1"}),
		metricEvent(t, agent.EventDecisionPremortemSubmitted, decisionEvent{
			DecisionID: "dec-1",
			Premortem:  &PremortemResult{FailureModes: []FailureMode{{ID: "F1"}, {ID: "F2"}}},
		}),
		metricEvent(t, agent.EventDecisionEvidenceSharedOrigin, decisionEvent{DecisionID: "dec-1"}),
		metricEvent(t, agent.EventDecisionInvalidated, decisionEvent{DecisionID: "dec-1"}),
		metricEvent(t, agent.EventAssumptionContradicted, decisionEvent{DecisionID: "dec-1"}),
		metricEvent(t, agent.EventReplanRequested, decisionEvent{DecisionID: "dec-1"}),
		metricEvent(t, agent.EventKillCriterionTriggered, decisionEvent{DecisionID: "dec-2"}),
		metricEvent(t, agent.EventCommitGateBlocked, decisionEvent{DecisionID: "dec-2"}),
		metricEvent(t, agent.EventDecisionEvidenceChanged, decisionEvent{
			DecisionID: "dec-2", Reason: ReasonDecisionOutsideViewMissing,
		}),
		metricEvent(t, agent.EventDecisionEvidenceChanged, decisionEvent{
			DecisionID: "dec-2", Reason: ReasonDecisionNoNoGoOption,
		}),
	}

	metrics := ComputeDecisionMetrics(events, nil)
	checks := map[string]int{
		"opinions rejected":       metrics.DecisionOpinionRejectedCount,
		"budget degradations":     metrics.DecisionBudgetDegradedCount,
		"shared origin warnings":  metrics.SharedOriginWarnings,
		"stale decisions":         metrics.DecisionStaleCount,
		"assumption invalidation": metrics.AssumptionInvalidations,
		"replans":                 metrics.ReplanCount,
		"kill criteria":           metrics.KillCriteriaTriggered,
		"commit gate blocks":      metrics.CommitGateBlocked,
		"outside view failures":   metrics.OutsideViewGateFailures,
		"no-go missing":           metrics.NoGoOptionMissing,
	}
	for name, got := range checks {
		if got != 1 {
			t.Errorf("%s = %d, want 1", name, got)
		}
	}
	if metrics.PremortemFailureModes != 2 {
		t.Errorf("PremortemFailureModes = %d, want 2", metrics.PremortemFailureModes)
	}
	// One of two finalized decisions produced a revision.
	if metrics.DecisionRevisionRate != 0.5 {
		t.Errorf("DecisionRevisionRate = %v, want 0.5", metrics.DecisionRevisionRate)
	}
	if got := metrics.MeanDispersion(); got != 3 {
		t.Errorf("MeanDispersion = %v, want 3", got)
	}
}

// Foreign events share the log; they must contribute nothing rather than
// breaking the projection.
func TestComputeDecisionMetricsIgnoresForeignEvents(t *testing.T) {
	events := []RunEvent{
		{Type: "task_completed", Payload: json.RawMessage(`{"task_id":"t1","status":"done"}`)},
		{Type: "artifact_created", Payload: json.RawMessage(`not json`)},
		{Type: agent.EventDecisionOpinionRejected, Payload: json.RawMessage(`{"decision_id":"dec-1"}`)},
	}
	metrics := ComputeDecisionMetrics(events, nil)
	if metrics.DecisionOpinionRejectedCount != 1 {
		t.Fatalf("DecisionOpinionRejectedCount = %d, want 1", metrics.DecisionOpinionRejectedCount)
	}
}

func TestComputeDecisionMetricsFromIndex(t *testing.T) {
	entries := []DecisionIndexEntry{
		{DecisionID: "dec-1", Profile: "standard", IndependenceGroupCount: 2},
		{DecisionID: "dec-2", Profile: "standard", IndependenceGroupCount: 1,
			Outcome: &DecisionOutcomeRecord{ResolvedOutcome: OutcomeSucceeded, Verified: true}},
		{DecisionID: "dec-3", Profile: "high-stakes",
			Outcome: &DecisionOutcomeRecord{ResolvedOutcome: OutcomeFailed}},
	}
	metrics := ComputeDecisionMetrics(nil, entries)

	if metrics.DecisionCount != 3 {
		t.Fatalf("DecisionCount = %d, want 3", metrics.DecisionCount)
	}
	if metrics.DecisionProfileCount["standard"] != 2 || metrics.DecisionProfileCount["high-stakes"] != 1 {
		t.Fatalf("profiles = %v", metrics.DecisionProfileCount)
	}
	if metrics.IndependentEvidenceGroupCount != 3 {
		t.Fatalf("IndependentEvidenceGroupCount = %d, want 3", metrics.IndependentEvidenceGroupCount)
	}
	// An unverified outcome is resolved but does not count as verified: that
	// distinction is the whole point of the Phase 5 threshold (§49.2).
	if metrics.ResolvedCount != 2 || metrics.VerifiedCount != 1 {
		t.Fatalf("resolved = %d, verified = %d, want 2 and 1", metrics.ResolvedCount, metrics.VerifiedCount)
	}
	if got := metrics.SortedProfiles(); len(got) != 2 || got[0] != "high-stakes" {
		t.Fatalf("SortedProfiles = %v, want a stable order", got)
	}
}

// The Phase 5 gate needs both thresholds; resolved alone does not open it.
func TestPhase5Entry(t *testing.T) {
	tests := []struct {
		resolved, verified int
		open               bool
	}{
		{0, 0, false},
		{Phase5ResolvedRequired, Phase5VerifiedRequired - 1, false},
		{Phase5ResolvedRequired - 1, Phase5VerifiedRequired, false},
		{Phase5ResolvedRequired, Phase5VerifiedRequired, true},
		{Phase5ResolvedRequired + 10, Phase5VerifiedRequired + 10, true},
	}
	for _, tt := range tests {
		metrics := DecisionMetrics{ResolvedCount: tt.resolved, VerifiedCount: tt.verified}
		if got := metrics.Phase5Entry(); got.Open != tt.open {
			t.Errorf("Phase5Entry(%d resolved, %d verified).Open = %v, want %v",
				tt.resolved, tt.verified, got.Open, tt.open)
		}
	}
}

// The projection is deterministic: the same log always yields the same numbers.
func TestComputeDecisionMetricsIsDeterministic(t *testing.T) {
	events := []RunEvent{
		metricEvent(t, agent.EventDecisionAggregateComputed, decisionEvent{
			DecisionID: "dec-1", Aggregate: &DecisionAggregate{PreferredOption: "a", StdDev: map[string]float64{"a": 1.1}},
		}),
		metricEvent(t, agent.EventDecisionAggregateComputed, decisionEvent{
			DecisionID: "dec-2", Aggregate: &DecisionAggregate{PreferredOption: "b", StdDev: map[string]float64{"b": 2.7}},
		}),
		metricEvent(t, agent.EventDecisionAggregateComputed, decisionEvent{
			DecisionID: "dec-3", Aggregate: &DecisionAggregate{PreferredOption: "c", StdDev: map[string]float64{"c": 3.3}},
		}),
	}
	first := ComputeDecisionMetrics(events, nil)
	want, err := json.Marshal(first)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		got, err := json.Marshal(ComputeDecisionMetrics(events, nil))
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != string(want) {
			t.Fatalf("iteration %d differs:\n got %s\nwant %s", i, got, want)
		}
		if ComputeDecisionMetrics(events, nil).MeanDispersion() != first.MeanDispersion() {
			t.Fatalf("MeanDispersion is not deterministic")
		}
	}
}
