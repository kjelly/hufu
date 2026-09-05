package team

import (
	"fmt"
	"strings"

	"github.com/kjelly/hufu/internal/agent"
)

// Decision profile resolution (docs/hufu-decision-aware-runtime-spec.md §8).
//
// Precedence, highest first:
//
//	CLI / request override
//	Task contract override (configuration-only, never coordinator payload)
//	Team default
//	Runtime built-in default ("off")
//
// "off" never disables runtime correctness or safety; it only means "do not run
// structured multi-agent decision formation".

// DecisionProfileOff is the reserved profile name (spec §8).
const DecisionProfileOff = agent.DecisionProfileOff

// DecisionProfileResolution explains which layer supplied the profile so the
// choice is auditable rather than implicit.
type DecisionProfileResolution struct {
	Profile string
	Source  string // request | task | team | default
}

// Profile resolution sources.
const (
	DecisionProfileSourceRequest = "request"
	DecisionProfileSourceTask    = "task"
	DecisionProfileSourceTeam    = "team"
	DecisionProfileSourceDefault = "default"
)

// ResolveDecisionProfile applies the precedence chain. requestOverride is the
// CLI/request-scoped override and wins outright; task is the configuration-only
// TaskDef.DecisionProfile. An unknown name is an error, never a silent
// fallback to a weaker profile (spec §8, §9).
func ResolveDecisionProfile(cfg DecisionConfig, requestOverride string, task TaskDef) (DecisionProfileResolution, error) {
	candidates := []DecisionProfileResolution{
		{Profile: strings.TrimSpace(requestOverride), Source: DecisionProfileSourceRequest},
		{Profile: strings.TrimSpace(task.DecisionProfile), Source: DecisionProfileSourceTask},
		{Profile: strings.TrimSpace(cfg.DefaultProfile), Source: DecisionProfileSourceTeam},
	}
	for _, candidate := range candidates {
		if candidate.Profile == "" {
			continue
		}
		if !cfg.HasProfile(candidate.Profile) {
			return DecisionProfileResolution{}, fmt.Errorf(
				"%s: decision profile %q requested by %s is not defined in this team",
				ReasonDecisionProfileUnknown, candidate.Profile, candidate.Source)
		}
		return candidate, nil
	}
	return DecisionProfileResolution{Profile: DecisionProfileOff, Source: DecisionProfileSourceDefault}, nil
}

// DecisionPolicyFor returns the resolved policy for a profile name. The
// reserved "off" profile has no policy; callers must check Enabled first.
func DecisionPolicyFor(cfg DecisionConfig, profile string) (DecisionPolicy, bool) {
	if profile == "" || profile == DecisionProfileOff {
		return DecisionPolicy{}, false
	}
	policy, ok := cfg.Profiles[profile]
	return policy, ok
}

// Enabled reports whether the resolution selects structured decision
// formation. Everything else in the runtime stays active either way (spec §8).
func (r DecisionProfileResolution) Enabled() bool {
	return r.Profile != "" && r.Profile != DecisionProfileOff
}

// ValidateTaskDecisionProfiles checks every task's configuration-only profile
// reference at load time so an unknown name fails closed before dispatch
// (spec §9).
func ValidateTaskDecisionProfiles(cfg DecisionConfig, tasks []TaskDef) error {
	for i, task := range tasks {
		name := strings.TrimSpace(task.DecisionProfile)
		if name == "" {
			continue
		}
		if !cfg.HasProfile(name) {
			id := task.ID
			if id == "" {
				id = fmt.Sprintf("index %d", i)
			}
			return fmt.Errorf("%s: task %s references undefined decision profile %q",
				ReasonDecisionProfileUnknown, id, name)
		}
	}
	return nil
}
