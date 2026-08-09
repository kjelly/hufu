package main

import (
	"github.com/kjelly/hufu/internal/team"
)

// TimeoutCLIOverrides collects timeout-related CLI flag values. A value
// of 0 means "no override" and the underlying config keeps its current
// value. Negative values are also treated as "no override" to avoid
// accidentally clearing timeouts.
type TimeoutCLIOverrides struct {
	// Timeout is the agent/coordinator timeout in seconds.
	// When > 0, it overrides session.Config.Timeout and every
	// agent's Timeout in the session.
	Timeout int64
}

// applyCLITimeoutOverrides mutates the session in place to apply a
// non-zero --timeout override. The override is the highest-priority
// timeout configuration layer (above agent .md frontmatter and
// team.yaml).
//
// If the user passes only --timeout, both worker agents and the
// coordinator's session-level timeout are updated. Per-agent overrides
// (orchDef.Timeout) are also cleared so the new value takes effect
// uniformly. The coordinator's MaxRounds-derived timeout falls through
// to its existing formula using the new Config.Timeout.
func applyCLITimeoutOverrides(session *team.TeamSession, overrides TimeoutCLIOverrides) {
	if overrides.Timeout <= 0 {
		return
	}
	if session == nil {
		return
	}
	session.Config.Timeout = overrides.Timeout
	for _, def := range session.Agents {
		if def == nil {
			continue
		}
		def.Timeout = overrides.Timeout
	}
}

// currentTimeoutOverrides returns the live CLI flag values as a
// TimeoutCLIOverrides struct. Flags that were not set on the command
// line stay 0, signalling "no override" to applyCLITimeoutOverrides.
func currentTimeoutOverrides() TimeoutCLIOverrides {
	return TimeoutCLIOverrides{Timeout: opts.timeoutOverride}
}
