package main

import (
	"testing"
)

func TestResumeReferenceSelectionFlags(t *testing.T) {
	originalOpts := opts
	oldRefs, oldResult, oldModel, oldUpgrade, oldRetry := resumeRepairReferences, resumeRepairResult, resumeRepairModel, resumeUpgradeContracts, resumeRetryAcceptance
	t.Cleanup(func() {
		opts = originalOpts
		resumeRepairReferences, resumeRepairResult, resumeRepairModel, resumeUpgradeContracts, resumeRetryAcceptance = oldRefs, oldResult, oldModel, oldUpgrade, oldRetry
	})
	for _, tc := range []struct {
		name, result, model   string
		upgrade, retry, valid bool
		refs                  []string
	}{
		{name: "exact_choice", result: "26", refs: []string{"/findings/0=ev_observed"}, valid: true},
		{name: "no_task", refs: []string{"/findings/0=ev_observed"}},
		{name: "model_override", result: "26", model: "ollama/other", refs: []string{"/findings/0=ev_observed"}},
		{name: "publication", result: "26", upgrade: true, refs: []string{"/findings/0=ev_observed"}},
		{name: "acceptance_retry", result: "26", retry: true, refs: []string{"/findings/0=ev_observed"}},
		{name: "duplicate", result: "26", refs: []string{"/a=one", "/a=two"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts = runOptions{}
			resumeRepairResult, resumeRepairModel, resumeUpgradeContracts, resumeRetryAcceptance, resumeRepairReferences = tc.result, tc.model, tc.upgrade, tc.retry, tc.refs
			if err := validateResumeRepairFlags(); (err == nil) != tc.valid {
				t.Fatalf("flag error=%v", err)
			}
		})
	}
}
