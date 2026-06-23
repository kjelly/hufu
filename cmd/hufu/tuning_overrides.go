package main

import (
	"github.com/anomalyco/hufu/internal/team"
)

// TuningCLIOverrides collects team-tuning CLI flag values. A value of 0
// means "no override" and the underlying config keeps its current value.
// Negative values are also treated as "no override" to avoid accidentally
// clearing these limits.
type TuningCLIOverrides struct {
	MaxRounds     int
	MaxConcurrent int
	MaxSteps      int
}

// applyCLITuningOverrides mutates the session in place to apply non-zero
// CLI overrides for --max-rounds, --max-concurrent, --max-steps. The
// override is the highest-priority layer (above agent .md frontmatter and
// team.yaml).
//
// MaxRounds controls coordinator rounds, MaxConcurrent controls parallel
// worker dispatch, and MaxSteps controls per-agent step budget.
func applyCLITuningOverrides(session *team.TeamSession, overrides TuningCLIOverrides) {
	if session == nil {
		return
	}
	if overrides.MaxRounds > 0 {
		session.Config.MaxRounds = overrides.MaxRounds
	}
	if overrides.MaxConcurrent > 0 {
		session.Config.MaxConcurrent = overrides.MaxConcurrent
	}
	if overrides.MaxSteps > 0 {
		session.Config.MaxSteps = overrides.MaxSteps
		for _, def := range session.Agents {
			if def == nil {
				continue
			}
			if def.MaxSteps <= 0 || def.MaxSteps == session.Config.MaxSteps {
				def.MaxSteps = overrides.MaxSteps
			}
		}
	}
}

// currentTuningOverrides returns the live CLI flag values as a
// TuningCLIOverrides struct. Flags that were not set stay 0, signalling
// "no override".
func currentTuningOverrides() TuningCLIOverrides {
	return TuningCLIOverrides{
		MaxRounds:     maxRoundsOverride,
		MaxConcurrent: maxConcurrentOverride,
		MaxSteps:      maxStepsOverride,
	}
}
