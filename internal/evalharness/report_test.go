package evalharness

import (
	"bytes"
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
