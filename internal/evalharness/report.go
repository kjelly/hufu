package evalharness

import (
	"encoding/json"
	"fmt"
	"io"
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
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(suites)
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
