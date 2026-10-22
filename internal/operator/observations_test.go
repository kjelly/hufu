package operator

import (
	"math"
	"strings"
	"testing"

	"github.com/kjelly/hufu/internal/cost"
)

func testObservations() *ObservationSummary {
	return &ObservationSummary{
		Status: "available", ComparisonScope: "single_run_observed",
		SourceHead: cost.Freshness{EventID: "event-1", EventHash: "hash-1"},
		TaskCount:  new(2), VerifiedTasks: new(1), UnavailableTasks: new(1),
		UsageCostMicros: new(int64(21)), KnownCostCoverage: new(0.5),
		ReasonCodes: []string{"unknown_cost"},
		Routes:      []ObservedRoute{{Kind: "unbound", Target: "ollama/fixture", Attempts: 2, VerifiedTasks: 1}},
	}
}

func TestOperatorObservationsCloneHashAndHead(t *testing.T) {
	base := testSnapshot()
	legacy, err := FinalizeSnapshot(base)
	if err != nil {
		t.Fatal(err)
	}
	base.Observations = testObservations()
	frozen, err := FinalizeSnapshot(base)
	if err != nil {
		t.Fatal(err)
	}
	*base.Observations.UsageCostMicros = 999
	base.Observations.Routes[0].Target = "changed"
	base.Observations.ReasonCodes[0] = "changed"
	if *frozen.Observations.UsageCostMicros != 21 || frozen.Observations.Routes[0].Target != "ollama/fixture" ||
		frozen.Observations.ReasonCodes[0] != "unknown_cost" || frozen.SnapshotID == legacy.SnapshotID {
		t.Fatal("observations are aliased or omitted from snapshot identity")
	}
	for _, modify := range []func(*ObservationSummary){
		func(s *ObservationSummary) { s.SourceHead.EventHash = "another-head" },
		func(s *ObservationSummary) { s.KnownCostCoverage = new(math.NaN()) },
		func(s *ObservationSummary) { s.UsageCostMicros = new(int64(-1)) },
		func(s *ObservationSummary) { s.VerifiedTasks = new(3) },
		func(s *ObservationSummary) { s.Status = "unavailable" },
	} {
		invalid := frozen
		invalid.Observations = frozen.Observations.Clone()
		modify(invalid.Observations)
		if _, err := FinalizeSnapshot(invalid); err == nil {
			t.Fatal("invalid observations accepted")
		}
	}
}

func TestOperatorObservationsPlainUnknownAndSanitized(t *testing.T) {
	if got := RenderObservationSummary(nil); got != "Observations: unavailable\n" {
		t.Fatalf("legacy snapshot invented observations: %q", got)
	}
	summary := testObservations()
	summary.UsageCostMicros, summary.KnownCostCoverage = nil, nil
	summary.Routes[0].Target = "ollama/\x1b[31mfixture\x1b[0m"
	text := RenderObservationSummary(summary)
	for _, want := range []string{"Usage subtotal (USD micros): unknown", "Recorded invocation coverage: unknown",
		"Verified: 1", "Evidence unavailable: 1", "ollama/fixture", "unknown_cost"} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q: %s", want, text)
		}
	}
	if strings.ContainsRune(text, '\x1b') {
		t.Fatal("summary emitted terminal controls")
	}
}
