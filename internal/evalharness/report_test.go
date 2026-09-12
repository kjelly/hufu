package evalharness

import (
	"bytes"
	"testing"
	"time"
)

func TestEvalResultJSONStable(t *testing.T) {
	suites := []EvalSuiteResult{
		{
			SuiteName: "core-lifecycle",
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

	var first, second bytes.Buffer
	if err := WriteJSONReport(&first, suites); err != nil {
		t.Fatalf("WriteJSONReport (first): %v", err)
	}
	if err := WriteJSONReport(&second, suites); err != nil {
		t.Fatalf("WriteJSONReport (second): %v", err)
	}
	if first.String() != second.String() {
		t.Errorf("encoding the same result twice produced different JSON:\nfirst:  %s\nsecond: %s", first.String(), second.String())
	}
}
