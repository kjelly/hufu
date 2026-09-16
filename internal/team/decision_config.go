package team

import (
	"fmt"
	"strings"

	"github.com/kjelly/hufu/internal/agent"
)

// Decision profile resolution (docs/architecture/decision-runtime.md §8).
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
func ResolveDecisionProfile(cfg DecisionConfig, requestOverride string, task any) (DecisionProfileResolution, error) {
	_, resolution, err := ResolveMaterializedDecisionProfile(cfg, requestOverride, task, agent.BuiltInDecisionProfileCatalog())
	return resolution, err
}

// ResolveMaterializedDecisionProfile applies precedence and freezes a complete
// normalized policy before any decision stage or provider dispatch.
func ResolveMaterializedDecisionProfile(
	cfg DecisionConfig,
	requestOverride string,
	task any,
	catalog agent.DecisionProfileCatalog,
) (agent.MaterializedDecisionProfile, DecisionProfileResolution, error) {
	taskProfile, err := decisionProfileForInput(task)
	if err != nil {
		return agent.MaterializedDecisionProfile{}, DecisionProfileResolution{}, err
	}
	candidates := []DecisionProfileResolution{
		{Profile: strings.TrimSpace(requestOverride), Source: DecisionProfileSourceRequest},
		{Profile: taskProfile, Source: DecisionProfileSourceTask},
		{Profile: strings.TrimSpace(cfg.DefaultProfile), Source: DecisionProfileSourceTeam},
	}
	for _, candidate := range candidates {
		if candidate.Profile == "" {
			continue
		}
		if candidate.Profile == DecisionProfileOff {
			return agent.MaterializedDecisionProfile{}, candidate, nil
		}
		policy, metadata, ok, resolveErr := agent.ResolveDecisionProfileSpec(cfg, candidate.Profile, catalog)
		if resolveErr != nil || !ok {
			if resolveErr != nil {
				return agent.MaterializedDecisionProfile{}, DecisionProfileResolution{}, fmt.Errorf(
					"%s: decision profile %q requested by %s: %w",
					ReasonDecisionProfileUnknown, candidate.Profile, candidate.Source, resolveErr)
			}
			return agent.MaterializedDecisionProfile{}, DecisionProfileResolution{}, fmt.Errorf(
				"%s: decision profile %q requested by %s is not defined in this team",
				ReasonDecisionProfileUnknown, candidate.Profile, candidate.Source)
		}
		normalized, normalizeErr := agent.NormalizeDecisionPolicy(policy)
		if normalizeErr != nil {
			return agent.MaterializedDecisionProfile{}, DecisionProfileResolution{}, fmt.Errorf("decision profile %q: %w", candidate.Profile, normalizeErr)
		}
		digest, digestErr := agent.DecisionPolicyDigest(normalized)
		if digestErr != nil {
			return agent.MaterializedDecisionProfile{}, DecisionProfileResolution{}, fmt.Errorf("decision profile %q digest: %w", candidate.Profile, digestErr)
		}
		return agent.MaterializedDecisionProfile{
			RequestedName: candidate.Profile,
			Ref:           metadata.Ref, Origin: metadata.Origin, Version: metadata.Version,
			Policy: normalized, PolicyDigest: digest,
		}, candidate, nil
	}
	resolution := DecisionProfileResolution{Profile: DecisionProfileOff, Source: DecisionProfileSourceDefault}
	return agent.MaterializedDecisionProfile{}, resolution, nil
}

func decisionProfileForInput(input any) (string, error) {
	switch value := input.(type) {
	case TaskDef:
		return strings.TrimSpace(value.DecisionProfile), nil
	case TaskOccurrenceProjection:
		return strings.TrimSpace(value.DecisionProfile), nil
	case *TaskOccurrenceProjection:
		if value == nil {
			return "", fmt.Errorf("decision profile input is nil")
		}
		return strings.TrimSpace(value.DecisionProfile), nil
	case *TodoItem:
		if value == nil {
			return "", fmt.Errorf("decision profile input is nil")
		}
		return strings.TrimSpace(value.DecisionProfile), nil
	default:
		return "", fmt.Errorf("unsupported decision profile input %T", input)
	}
}

// DecisionPolicyFor returns the resolved policy for a profile name. The
// reserved "off" profile has no policy; callers must check Enabled first.
func DecisionPolicyFor(cfg DecisionConfig, profile string) (DecisionPolicy, bool) {
	if profile == "" || profile == DecisionProfileOff {
		return DecisionPolicy{}, false
	}
	policy, _, ok, err := agent.ResolveDecisionProfileSpec(cfg, profile, agent.BuiltInDecisionProfileCatalog())
	return policy, ok && err == nil
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
