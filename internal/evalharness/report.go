package evalharness

import (
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"slices"
)

// WriteTextReport writes a human-readable pass/fail report, one line per
// case plus one line per failed dimension.
func WriteTextReport(w io.Writer, suites []EvalSuiteResult) {
	for _, suite := range suites {
		for _, c := range suite.Cases {
			status := "PASS"
			if !c.Passed {
				status = "FAIL"
			}
			_, _ = fmt.Fprintf(w, "%s %s/%s (%s)\n", status, suite.SuiteName, c.CaseID, c.RunOutcome)
			for _, f := range c.Findings {
				_, _ = fmt.Fprintf(w, "  - %s: expected %q, got %q\n", f.Dimension, f.Expected, f.Actual)
			}
		}
	}
}

// WriteJSONReport writes the full suite results as a stable, indented JSON
// array (`hufu eval run ... --format json`).
func WriteJSONReport(w io.Writer, suites []EvalSuiteResult) error {
	return writeJSONReport(w, suites, false)
}

// WriteMeasuredJSONReport preserves real diagnostic timing for benchmark
// artifacts. Selection-only CI reports use WriteJSONReport for stable bytes.
func WriteMeasuredJSONReport(w io.Writer, suites []EvalSuiteResult) error {
	return writeJSONReport(w, suites, true)
}

func writeJSONReport(w io.Writer, suites []EvalSuiteResult, measured bool) error {
	stable := make([]EvalSuiteResult, len(suites))
	for suiteIndex, suite := range suites {
		stable[suiteIndex] = suite
		stable[suiteIndex].Cases = make([]EvalCaseResult, len(suite.Cases))
		for caseIndex, result := range suite.Cases {
			stable[suiteIndex].Cases[caseIndex] = result
			stable[suiteIndex].Cases[caseIndex].Metrics.Duration = 0
			if delivery := result.Metrics.ContextDelivery; delivery != nil && !measured {
				copyDelivery := *delivery
				copyDelivery.Requests = slices.Clone(delivery.Requests)
				for index := range copyDelivery.Requests {
					request := &copyDelivery.Requests[index]
					request.RetrievalLatencyMS, request.SelectionLatencyMS, request.StageLatencyMS = nil, nil, nil
					request.MetricReasons = maps.Clone(request.MetricReasons)
					if request.MetricReasons == nil {
						request.MetricReasons = make(map[string]string)
					}
					request.MetricReasons["retrieval_latency_ms"] = "timing_omitted"
					request.MetricReasons["selection_latency_ms"] = "timing_omitted"
				}
				stable[suiteIndex].Cases[caseIndex].Metrics.ContextDelivery = &copyDelivery
			}
		}
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(stable)
}

// AllPassed reports whether every case across every suite passed.
func AllPassed(suites []EvalSuiteResult) bool {
	for _, s := range suites {
		if !s.Passed() {
			return false
		}
	}
	return true
}
