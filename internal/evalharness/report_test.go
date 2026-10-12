package evalharness

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func TestEvalResultJSONStable(t *testing.T) {
	firstSuites := []EvalSuiteResult{
		{
			SuiteName:         "core-lifecycle",
			BenchmarkRevision: "benchmark-revision",
			Cases: []EvalCaseResult{
				{
					CaseID:     "single-task-unverified",
					Passed:     true,
					RunOutcome: "unverified",
					Findings:   nil,
					Metrics:    EvalMetrics{Duration: 250 * time.Millisecond, RunID: "<run-id>"},
				},
			},
		},
	}
	secondSuites := []EvalSuiteResult{
		{
			SuiteName:         "core-lifecycle",
			BenchmarkRevision: "benchmark-revision",
			Cases: []EvalCaseResult{{
				CaseID: "single-task-unverified", Passed: true, RunOutcome: "unverified",
				Metrics: EvalMetrics{Duration: 9 * time.Second, RunID: "<run-id>"},
			}},
		},
	}

	var first, second bytes.Buffer
	if err := WriteJSONReport(&first, firstSuites); err != nil {
		t.Fatalf("WriteJSONReport (first): %v", err)
	}
	if err := WriteJSONReport(&second, secondSuites); err != nil {
		t.Fatalf("WriteJSONReport (second): %v", err)
	}
	if first.String() != second.String() {
		t.Errorf("runtime duration made deterministic JSON unstable:\nfirst:  %s\nsecond: %s", first.String(), second.String())
	}
}

func TestEvalJSONContextTimingIsStableWithoutMutatingEvidence(t *testing.T) {
	delivery := &ContextDeliveryMetrics{SchemaVersion: 1, Requests: []ContextRequestMetrics{{
		RetrievalLatencyMS: new(1.0), SelectionLatencyMS: new(4.0),
		StageLatencyMS: map[string]float64{"routing": 2, "compilation": 1, "manifest": 1},
		MetricReasons:  map[string]string{"embedding_calls": "backend_not_observed"},
	}}}
	suites := []EvalSuiteResult{{Cases: []EvalCaseResult{{Metrics: EvalMetrics{ContextDelivery: delivery}}}}}
	var first, second, measured bytes.Buffer
	if err := WriteJSONReport(&first, suites); err != nil {
		t.Fatal(err)
	}
	if delivery.Requests[0].SelectionLatencyMS == nil || delivery.Requests[0].MetricReasons["selection_latency_ms"] != "" {
		t.Fatal("stable report mutated original measurement")
	}
	delivery.Requests[0].SelectionLatencyMS = new(8.0)
	if err := WriteJSONReport(&second, suites); err != nil {
		t.Fatal(err)
	}
	if first.String() != second.String() {
		t.Fatal("timing changed stable JSON")
	}
	if err := WriteMeasuredJSONReport(&measured, suites); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(measured.String(), `"selection_latency_ms": 8`) || !strings.Contains(measured.String(), `"stage_latency_ms"`) {
		t.Fatalf("measured report lost timing: %s", measured.String())
	}
}
