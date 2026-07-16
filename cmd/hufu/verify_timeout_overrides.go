package main

import (
	"github.com/anomalyco/hufu/internal/team"
)

// VerifyTimeoutCLIOverrides collects verification-timeout CLI flag values.
// A value of 0 means "no override" and the underlying config keeps its
// current value. Negative values are also treated as "no override" to avoid
// accidentally clearing the timeout.
type VerifyTimeoutCLIOverrides struct {
	VerifyTimeout int64
}

// applyCLIVerifyTimeoutOverrides mutates the session in place to apply a
// non-zero --verify-timeout override. The override is the highest-priority
// verify timeout configuration layer (above agent frontmatter and team.yaml).
func applyCLIVerifyTimeoutOverrides(session *team.TeamSession, overrides VerifyTimeoutCLIOverrides) {
	if overrides.VerifyTimeout <= 0 {
		return
	}
	if session == nil {
		return
	}
	session.Config.VerifyTimeout = overrides.VerifyTimeout
}

// currentVerifyTimeoutOverrides returns the live CLI flag values as a
// VerifyTimeoutCLIOverrides struct. Flags that were not set on the command
// line stay 0, signalling "no override".
func currentVerifyTimeoutOverrides() VerifyTimeoutCLIOverrides {
	return VerifyTimeoutCLIOverrides{VerifyTimeout: opts.verifyTimeoutOverride}
}
