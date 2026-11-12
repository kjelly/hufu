package main

import (
	"testing"
	"time"
)

func TestResumeAcceptanceRetryFlags(t *testing.T) {
	oldRun, oldRetry, oldTimeout, oldSet, oldRepair, oldUpgrade, oldOpts := resumeRun, resumeRetryAcceptance, resumeAcceptanceTimeout, resumeAcceptanceTimeoutSet, resumeRepairResult, resumeUpgradeContracts, opts
	t.Cleanup(func() {
		resumeRun, resumeRetryAcceptance, resumeAcceptanceTimeout, resumeAcceptanceTimeoutSet, resumeRepairResult, resumeUpgradeContracts, opts = oldRun, oldRetry, oldTimeout, oldSet, oldRepair, oldUpgrade, oldOpts
	})
	for _, name := range []string{"valid", "missing_run", "negative_timeout", "collector_repair", "contract_upgrade", "timeout_without_retry", "new_image"} {
		t.Run(name, func(t *testing.T) {
			resumeRun = "exact-run"
			resumeRetryAcceptance = true
			resumeAcceptanceTimeout = time.Minute
			resumeAcceptanceTimeoutSet = true
			resumeRepairResult = ""
			resumeUpgradeContracts = false
			opts = oldOpts
			opts.imagePaths = nil
			switch name {
			case "missing_run":
				resumeRun = ""
			case "negative_timeout":
				resumeAcceptanceTimeout = -time.Second
			case "collector_repair":
				resumeRepairResult = "4"
			case "contract_upgrade":
				resumeUpgradeContracts = true
			case "timeout_without_retry":
				resumeRetryAcceptance = false
			case "new_image":
				opts.imagePaths = []string{"new.png"}
			}
			err := validateResumeRepairFlags()
			if (err == nil) != (name == "valid") {
				t.Fatalf("%s: %v", name, err)
			}
		})
	}
}
