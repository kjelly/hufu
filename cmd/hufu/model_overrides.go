package main

import (
	"github.com/anomalyco/hufu/internal/agent"
	"github.com/anomalyco/hufu/internal/team"
)

// ModelCLIOverrides collects model-related CLI flag values. Empty fields
// mean "no override" and the underlying config keeps its current value.
type ModelCLIOverrides struct {
	Model        string
	Temperature  string
	MaxTokens    string
	TopP         string
	TopK         string
	SidecarModel string
	GuardModel   string
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
	if overrides.SidecarModel != "" {
		cfg.SidecarModel = overrides.SidecarModel
	} else if cfg.Generation.Model != "" {
		cfg.SidecarModel = cfg.Generation.Model
	}
	if overrides.GuardModel != "" {
		cfg.GuardModel = overrides.GuardModel
	} else if cfg.Generation.Model != "" {
		cfg.GuardModel = cfg.Generation.Model
	}
}

// applyOverridesToAgents propagates team-level config overrides to every
// agent in the session. Agents built with stale Generation values from
// LoadTeam/LoadDefaultTime inherit the new values here.
func applyOverridesToAgents(session interface {
	GetConfig() agent.TeamConfig
	GetAgents() map[string]*agent.AgentDef
}) {
	cfg := session.GetConfig()
	for _, def := range session.GetAgents() {
		if def == nil {
			continue
		}
		if cfg.Generation.Model != "" {
			def.Generation.Model = cfg.Generation.Model
		}
		if cfg.Generation.Temperature != "" {
			def.Generation.Temperature = cfg.Generation.Temperature
		}
		if cfg.Generation.MaxTokens != "" {
			def.Generation.MaxTokens = cfg.Generation.MaxTokens
		}
		if cfg.Generation.TopP != "" {
			def.Generation.TopP = cfg.Generation.TopP
		}
		if cfg.Generation.TopK != "" {
			def.Generation.TopK = cfg.Generation.TopK
		}
		if cfg.ProviderURL != "" {
			def.ProviderURL = cfg.ProviderURL
		}
	}
}

// currentModelOverrides returns the live CLI flag values as a
// ModelCLIOverrides struct. Flags that were not set on the command line
// stay empty, signalling "no override" to applyCLIModelOverrides.
func currentModelOverrides() ModelCLIOverrides {
	return ModelCLIOverrides{
		Model:        modelOverride,
		Temperature:  temperatureOverride,
		MaxTokens:    maxTokensOverride,
		TopP:         topPOverride,
		TopK:         topKOverride,
		SidecarModel: sidecarModelOverride,
		GuardModel:   guardModelOverride,
	}
}

// propagateTeamGenerationToAgents copies the team-level Generation and
// ProviderURL from session.Config into every agent's Generation and
// ProviderURL. Used after CLI overrides to ensure per-agent dispatch
// uses the user-requested model.
func propagateTeamGenerationToAgents(session *team.TeamSession) {
	if session == nil {
		return
	}
	for _, def := range session.Agents {
		if def == nil {
			continue
		}
		if session.Config.Generation.Model != "" {
			def.Generation.Model = session.Config.Generation.Model
		}
		if session.Config.Generation.Temperature != "" {
			def.Generation.Temperature = session.Config.Generation.Temperature
		}
		if session.Config.Generation.MaxTokens != "" {
			def.Generation.MaxTokens = session.Config.Generation.MaxTokens
		}
		if session.Config.Generation.TopP != "" {
			def.Generation.TopP = session.Config.Generation.TopP
		}
		if session.Config.Generation.TopK != "" {
			def.Generation.TopK = session.Config.Generation.TopK
		}
		if session.Config.ProviderURL != "" {
			def.ProviderURL = session.Config.ProviderURL
		}
	}
}
