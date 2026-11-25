package main

import (
	"encoding/json"
	"testing"
)

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
					sealFixtureOutputs(t, item)
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
			_, _, err := fixtureVerify(fixture, "run-1")
			if (err == nil) != test.valid {
				t.Fatalf("valid=%v error=%v", test.valid, err)
			}
		})
	}
}

func TestReviewGateChecksSealedTimeoutAndEmptyScopeGaps(t *testing.T) {
	schema, _ := contractSchema(t, "report-v3.json")
	for _, tc := range []struct {
		name, wire string
		valid      bool
	}{
		{"package deadline", `{"completed":false,"passed":false,"exit_code":1,"timed_out":true,"timeout_kind":"test","gap":"test_timeout_gap","command":["go","test"],"packages":["./target"],"diagnostic":"package deadline reached"}`, true},
		{"command deadline", `{"completed":false,"passed":false,"exit_code":-1,"timed_out":true,"timeout_kind":"command","gap":"test_timeout_gap","command":["go","test"],"packages":["./target"],"diagnostic":"command deadline reached"}`, true},
		{"preflight deadline", `{"completed":false,"passed":false,"exit_code":-1,"timed_out":true,"timeout_kind":"preflight","gap":"test_timeout_gap","command":[],"packages":["./target"],"diagnostic":"preflight deadline reached"}`, true},
		{"preflight cannot claim executed command", `{"completed":false,"passed":false,"exit_code":-1,"timed_out":true,"timeout_kind":"preflight","gap":"test_timeout_gap","command":["go","test"],"packages":["./target"],"diagnostic":"preflight deadline reached"}`, false},
		{"canceled caller", `{"completed":false,"passed":false,"exit_code":-1,"timed_out":true,"timeout_kind":"caller_deadline","gap":"test_timeout_gap","command":["go","test"],"packages":["./target"],"diagnostic":"caller canceled"}`, false},
		{"external dependency", `{"completed":false,"passed":false,"exit_code":-1,"timed_out":false,"timeout_kind":"none","gap":"test_dependency_outside_snapshot","command":[],"packages":["./target"],"diagnostic":"local dependency outside snapshot"}`, true},
		{"empty workset", `{"completed":false,"passed":false,"exit_code":-1,"timed_out":false,"timeout_kind":"none","gap":"test_scope_empty","command":[],"packages":[],"diagnostic":"no Go packages"}`, true},
		{"unclassified skip", `{"completed":false,"passed":false,"exit_code":-1,"timed_out":false,"timeout_kind":"none","command":[],"packages":[],"diagnostic":"no Go packages"}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gap, err := verifyTestEvidence([]byte(tc.wire))
			if tc.valid != (gap && err == nil) {
				t.Fatalf("gap = %v, %v", gap, err)
			}
			_, _, report := schemaFixture()
			value := report["test_verification"].(map[string]any)
			if err := json.Unmarshal([]byte(tc.wire), &value); err != nil {
				t.Fatal(err)
			}
			if err := schema.Properties["test_verification"].Validate(value); (err == nil) != tc.valid {
				t.Fatalf("report schema disagrees with the review gate: %v", err)
			}
		})
	}
}
