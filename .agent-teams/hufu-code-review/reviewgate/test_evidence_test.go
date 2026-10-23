package main

import "testing"

func TestReviewGateRequiresHonestTestOutcomeCoverage(t *testing.T) {
	for _, test := range []struct {
		name       string
		completed  bool
		passed     bool
		exitCode   int
		timedOut   bool
		truncated  bool
		diagnostic string
		limited    bool
		valid      bool
	}{
		{name: "passed", completed: true, passed: true, valid: true},
		{name: "failed_with_gap", completed: true, exitCode: 1, diagnostic: "FAIL: target or environment", limited: true, valid: true},
		{name: "failed_concealed", completed: true, exitCode: 1, diagnostic: "FAIL: target or environment"},
		{name: "truncated_with_gap", completed: true, passed: true, truncated: true, limited: true, valid: true},
		{name: "truncated_concealed", completed: true, passed: true, truncated: true},
		{name: "not_completed", passed: true},
		{name: "timed_out", completed: true, passed: true, timedOut: true},
		{name: "contradictory_outcome", completed: true, passed: true, exitCode: 1},
		{name: "invocation_error", completed: true, exitCode: 2, diagnostic: "bad invocation", limited: true},
		{name: "missing_failure_diagnostic", completed: true, exitCode: 1, limited: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := completeReviewFixture(t)
			evidence := raw(t, map[string]any{
				"completed": test.completed, "passed": test.passed, "exit_code": test.exitCode,
				"timed_out": test.timedOut, "output_truncated": test.truncated, "diagnostic": test.diagnostic,
			})
			for i := range fixture.Tasks {
				item := &fixture.Tasks[i]
				if item.ContractID == "verify-targeted-go-tests" {
					item.Result.RuntimeOutputs["targeted_go_tests"] = evidence
				}
				if item.ContractID == "synthesize-review" {
					replacePayload(t, item, func(p *payload) {
						p.Tests = evidence
						if test.limited {
							p.Coverage = "completed_with_gaps"
						}
					})
					if test.limited {
						item.Result.Status = "completed_with_gaps"
					}
				}
			}
			_, _, err := verify(fixture, "run-1")
			if (err == nil) != test.valid {
				t.Fatalf("valid=%v error=%v", test.valid, err)
			}
		})
	}
}
