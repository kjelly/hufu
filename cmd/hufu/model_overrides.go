package main

import (
	"strings"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/team"
)

// ModelCLIOverrides collects model-related CLI flag values. Empty fields
// mean "no override" and the underlying config keeps its current value.
type ModelCLIOverrides struct {
	Model             string
	CoordinatorModel  string
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
	// WorkerModels holds parsed --worker-model entries (already merged with
	// the selected profile and deduplicated by agent). They are resolved
	// against the loaded team by applyCLIGenerationOverridesToAgents.
	WorkerModels []WorkerModelOverride
}

// applyCLIModelOverrides mutates cfg in place to apply non-empty CLI
// overrides. This is the highest-priority model configuration layer
// (above agent .md frontmatter, team.yaml, and hufu.yaml).
//
// --model is a worker-only override. Coordinator and auxiliary role models
// keep their independently configured targets unless their dedicated flag is
// supplied.
func applyCLIModelOverrides(cfg *agent.TeamConfig, overrides ModelCLIOverrides) {
	if overrides.Model != "" {
		cfg.WorkerModel = overrides.Model
	}
	if overrides.CoordinatorModel != "" {
		cfg.CoordinatorModel = overrides.CoordinatorModel
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
	}
	if overrides.GuardModel != "" {
		cfg.GuardModel = overrides.GuardModel
	}
	// Judge deliberately has no --model fallback: it falls back to the
	// sidecar model at resolve time instead, preserving the cheap-by-default
	// property (judging with the main model would double main-model cost).
	if overrides.JudgeModel != "" {
		cfg.JudgeModel = overrides.JudgeModel
	}
	if overrides.PlanReviewerModel != "" {
		cfg.PlanReviewerModel = overrides.PlanReviewerModel
	}
}

// isCoordinatorRole reports whether role names the team coordinator. Worker
// execution-target overrides never apply to such an agent: its target is owned
// by --coordinator-model. The role set matches Coordinator.resolveAgentModel,
// which treats coordinator and orchestrator as equivalent.
func isCoordinatorRole(role string) bool {
	role = strings.TrimSpace(role)
	return strings.EqualFold(role, "coordinator") || strings.EqualFold(role, "orchestrator")
}

// currentModelOverrides returns the live CLI flag values as a
// ModelCLIOverrides struct. Flags that were not set on the command line
// stay empty, signalling "no override" to applyCLIModelOverrides. Malformed
// --worker-model entries are reported rather than dropped.
func currentModelOverrides() (ModelCLIOverrides, error) {
	workerModels, err := parseWorkerModelOverrides(opts.workerModelOverrides)
	if err != nil {
		return ModelCLIOverrides{}, err
	}
	return ModelCLIOverrides{
		Model:             opts.modelOverride,
		WorkerModels:      workerModels,
		CoordinatorModel:  opts.coordinatorModelOverride,
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
	}, nil
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
//
// A --worker-model entry is more specific than --model and wins for its
// worker. Only the execution target changes: prompts, roles, tools, and the
// coordinator's own target are never touched. Invalid worker entries fail
// before any agent is modified.
func applyCLIGenerationOverridesToAgents(session *team.TeamSession, overrides ModelCLIOverrides) error {
	if session == nil {
		return nil
	}
	workerTargets, err := resolveWorkerModelTargets(session, overrides.WorkerModels)
	if err != nil {
		return err
	}
	for _, def := range session.Agents {
		if def == nil {
			continue
		}
		if target, ok := workerTargets[def]; ok {
			def.Generation.Model = target
		} else if overrides.Model != "" && !isCoordinatorRole(def.Role) && !strings.EqualFold(def.Name, "coordinator") {
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
	return nil
}
