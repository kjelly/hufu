package main

import (
	"bytes"
	"strings"
	"testing"

	inspectpkg "github.com/kjelly/hufu/internal/inspect"
)

func TestRenderInspectTaskTextShowsReplanComparison(t *testing.T) {
	data := inspectpkg.TaskData{RunID: "run-1", TaskID: "2", Status: "error", Replans: []inspectpkg.ReplanData{
		{PreviousTaskID: "1", Link: "verification", Mode: "warn", Phase: "planned", UnknownDimensions: []string{"tool_sequence"}},
		{PreviousTaskID: "1", Link: "verification", Mode: "warn", Phase: "executed", Attempt: 1, MateriallyDifferent: true, ChangedDimensions: []string{"tool_sequence"}},
	}}
	var out bytes.Buffer
	if err := renderInspectTaskText(&out, "main", data); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"Replaces task 1 (verification link, material-replan warn): planned repeats the failed strategy changed=none unknown=tool_sequence",
		"Replaces task 1 (verification link, material-replan warn): executed (attempt 1) changes the strategy changed=tool_sequence unknown=none",
	} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("output missing %q:\n%s", want, out.String())
		}
	}
}

func TestRenderInspectTaskTextShowsRecoveryComparison(t *testing.T) {
	cases := []struct {
		name     string
		recovery *inspectpkg.AttemptRecoveryData
		want     string
	}{
		{name: "first attempt", recovery: &inspectpkg.AttemptRecoveryData{Comparison: "unknown", Reason: "no_prior_attempt", NotTracked: []string{"tool_sequence"}}, want: "  Recovery: unknown (no_prior_attempt) changed=none unknown=none not_tracked=tool_sequence"},
		{name: "blind retry", recovery: &inspectpkg.AttemptRecoveryData{Comparison: "no_structural_change", PreviousAttempt: 1}, want: "  Recovery: no_structural_change (vs attempt 1) changed=none unknown=none not_tracked=none"},
		{name: "changed target", recovery: &inspectpkg.AttemptRecoveryData{Comparison: "change_detected", PreviousAttempt: 2, ChangedDimensions: []string{"execution_target", "model_execution"}}, want: "  Recovery: change_detected (vs attempt 2) changed=execution_target, model_execution unknown=none"},
		{name: "no observation"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			data := inspectpkg.TaskData{RunID: "run-1", TaskID: "1", Status: "error", Attempts: []inspectpkg.AttemptData{{Attempt: 1, VerificationStatus: "failed", Recovery: tc.recovery}}}
			if err := renderInspectTaskText(&out, "main", data); err != nil {
				t.Fatal(err)
			}
			if tc.want == "" {
				if strings.Contains(out.String(), "Recovery:") {
					t.Fatalf("output without an observation shows a recovery line:\n%s", out.String())
				}
				return
			}
			if !strings.Contains(out.String(), tc.want) {
				t.Fatalf("output missing %q:\n%s", tc.want, out.String())
			}
		})
	}
}
