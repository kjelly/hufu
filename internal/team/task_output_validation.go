package team

import (
	"fmt"
	"strings"
)

var unfinishedOutputPrefixes = []string{
	"let me ",
	"now let me ",
	"i'll ",
	"i will ",
}

var verificationGoalHints = []string{
	"execute all verification steps",
	"for each step",
	"actual output",
	"raw output",
	"mark pass or fail",
	"mark pass/fail",
	"do not skip any steps",
	"compare actual",
	"verification document",
}

func validateTaskOutput(task TaskDef, output string) error {
	trimmed := strings.TrimSpace(output)
	if trimmed == "" {
		return fmt.Errorf("task returned empty output")
	}

	lowerOutput := strings.ToLower(trimmed)
	for _, prefix := range unfinishedOutputPrefixes {
		if strings.HasPrefix(lowerOutput, prefix) {
			return fmt.Errorf("task returned an unfinished progress update instead of a final result")
		}
	}

	if !isVerificationTask(task) {
		return nil
	}

	if !containsAnySubstring(lowerOutput, "pass", "fail", "match", "mismatch") {
		return fmt.Errorf("verification task output is missing pass/fail-style status markers")
	}

	if !containsAnySubstring(lowerOutput, "```", "command:", "actual output", "`kvmforge", "`virsh", "### 1.", "1. `") {
		return fmt.Errorf("verification task output is missing concrete command/output evidence")
	}

	return nil
}

func isVerificationTask(task TaskDef) bool {
	goal := strings.ToLower(task.Goal + "\n" + task.Constraints)
	for _, hint := range verificationGoalHints {
		if strings.Contains(goal, hint) {
			return true
		}
	}
	return false
}

func containsAnySubstring(s string, parts ...string) bool {
	for _, part := range parts {
		if strings.Contains(s, strings.ToLower(part)) {
			return true
		}
	}
	return false
}
