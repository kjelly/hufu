package main

import (
	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/team"
)

// ModelCLIOverrides collects model-related CLI flag values. Empty fields
// mean "no override" and the underlying config keeps its current value.
type ModelCLIOverrides struct {
	Model             string
	ContextWindow     int
	Temperature       string
	MaxTokens         string
	TopP              string
	TopK              string
	ReasoningEffort   string
	SidecarModel      string
	GuardModel        string
	JudgeModel        string
	PlanReviewerModel string
}

// applyCLIModelOverrides mutates cfg in place to apply non-empty CLI
// overrides. This is the highest-priority model configuration layer
// (above agent .md frontmatter, team.yaml, and hufu.yaml).
//
// Sidecar/guard model fallback: if --sidecar-model or --guard-model are
// not set explicitly but --model is, the sidecar/guard values default to
// the value of --model. This lets a user set a single --model and have
// all three roles (main, sidecar, guard) use it without typing the
// model name three times.
func applyCLIModelOverrides(cfg *agent.TeamConfig, overrides ModelCLIOverrides) {
	if overrides.Model != "" {
		cfg.Generation.Model = overrides.Model
	}
	if overrides.ContextWindow > 0 {
		cfg.Generation.ContextWindow = overrides.ContextWindow
	}
	if overrides.Temperature != "" {
		cfg.Generation.Temperature = overrides.Temperature
	}
	if overrides.MaxTokens != "" {
		cfg.Generation.MaxTokens = overrides.MaxTokens
	}
	if overrides.TopP != "" {
		cfg.Generation.TopP = overrides.TopP
	}
	if overrides.TopK != "" {
		cfg.Generation.TopK = overrides.TopK
	}
	if overrides.ReasoningEffort != "" {
		cfg.Generation.ReasoningEffort = overrides.ReasoningEffort
	}
	if overrides.SidecarModel != "" {
		cfg.SidecarModel = overrides.SidecarModel
	} else if overrides.Model != "" {
		cfg.SidecarModel = overrides.Model
	}
	if overrides.GuardModel != "" {
		cfg.GuardModel = overrides.GuardModel
	} else if overrides.Model != "" {
		cfg.GuardModel = overrides.Model
	}
	// Judge deliberately has no --model fallback: it falls back to the
	// sidecar model at resolve time instead, preserving the cheap-by-default
	// property (judging with the main model would double main-model cost).
	if overrides.JudgeModel != "" {
		cfg.JudgeModel = overrides.JudgeModel
	}
	if overrides.PlanReviewerModel != "" {
		cfg.PlanReviewerModel = overrides.PlanReviewerModel
	} else if overrides.Model != "" {
		cfg.PlanReviewerModel = overrides.Model
	}
}

// currentModelOverrides returns the live CLI flag values as a
// ModelCLIOverrides struct. Flags that were not set on the command line
// stay empty, signalling "no override" to applyCLIModelOverrides.
func currentModelOverrides() ModelCLIOverrides {
	return ModelCLIOverrides{
		Model:             opts.modelOverride,
		ContextWindow:     opts.contextWindowOverride,
		Temperature:       opts.temperatureOverride,
		MaxTokens:         opts.maxTokensOverride,
		TopP:              opts.topPOverride,
		TopK:              opts.topKOverride,
		ReasoningEffort:   opts.reasoningEffortOverride,
		SidecarModel:      opts.sidecarModelOverride,
		GuardModel:        opts.guardModelOverride,
		JudgeModel:        opts.judgeModelOverride,
		PlanReviewerModel: opts.planReviewerModelOverride,
	}
}

// applyCLIGenerationOverridesToAgents forces CLI-supplied generation flags
// onto every agent's Generation, since a CLI flag is the highest-priority
// configuration layer and must beat both team.yaml and the agent's own
// frontmatter. Fields left empty in overrides (i.e. not passed on the
// command line) are left untouched here: the team.yaml/global-default value
// for those fields already reaches each agent through the normal
// agent-first/team-fallback resolution in CreateAgent, so force-copying it
// onto every AgentDef would silently override values an agent's .md
// frontmatter set intentionally (this used to be the case and was a bug —
// see spec.md item 1).
//
// ProviderURL has no CLI override in this flow, so it only fills in the
// team-level value when the agent hasn't set its own.
func applyCLIGenerationOverridesToAgents(session *team.TeamSession, overrides ModelCLIOverrides) {
	if session == nil {
		return
	}
	for _, def := range session.Agents {
		if def == nil {
			continue
		}
		if overrides.Model != "" {
			def.Generation.Model = overrides.Model
		}
		if overrides.Temperature != "" {
			def.Generation.Temperature = overrides.Temperature
		}
		if overrides.MaxTokens != "" {
			def.Generation.MaxTokens = overrides.MaxTokens
		}
		if overrides.TopP != "" {
			def.Generation.TopP = overrides.TopP
		}
		if overrides.TopK != "" {
			def.Generation.TopK = overrides.TopK
		}
		if overrides.ReasoningEffort != "" {
			def.Generation.ReasoningEffort = overrides.ReasoningEffort
		}
		if def.ProviderURL == "" && session.Config.ProviderURL != "" {
			def.ProviderURL = session.Config.ProviderURL
		}
	}
}
