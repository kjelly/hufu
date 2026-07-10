package main

import (
	"testing"

	"github.com/anomalyco/hufu/internal/agent"
	"github.com/anomalyco/hufu/internal/team"
)

func TestApplyCLIModelOverrides_Empty(t *testing.T) {
	// Empty Generation.Model so the sidecar/guard fallback does not kick
	// in. Pre-existing team.yaml/hufu.yaml sidecar/guard values stay
	// intact when --model is not set.
	cfg := agent.TeamConfig{
		Generation:   agent.GenerationParams{},
		SidecarModel: "sidecar-from-yaml",
		GuardModel:   "guard-from-yaml",
	}
	applyCLIModelOverrides(&cfg, ModelCLIOverrides{})

	if cfg.Generation.Model != "" {
		t.Errorf("Generation.Model = %q, want empty", cfg.Generation.Model)
	}
	if cfg.SidecarModel != "sidecar-from-yaml" {
		t.Errorf("SidecarModel = %q, want unchanged %q", cfg.SidecarModel, "sidecar-from-yaml")
	}
	if cfg.GuardModel != "guard-from-yaml" {
		t.Errorf("GuardModel = %q, want unchanged %q", cfg.GuardModel, "guard-from-yaml")
	}
}

func TestApplyCLIModelOverrides_All(t *testing.T) {
	cfg := agent.TeamConfig{
		Generation:   agent.GenerationParams{Model: "from-yaml"},
		SidecarModel: "sidecar-from-yaml",
		GuardModel:   "guard-from-yaml",
	}
	applyCLIModelOverrides(&cfg, ModelCLIOverrides{
		Model:        "cli-model",
		Temperature:  "0.1",
		MaxTokens:    "8192",
		TopP:         "0.95",
		TopK:         "50",
		SidecarModel: "cli-sidecar",
		GuardModel:   "cli-guard",
	})

	if cfg.Generation.Model != "cli-model" {
		t.Errorf("Generation.Model = %q, want %q", cfg.Generation.Model, "cli-model")
	}
	if cfg.Generation.Temperature != "0.1" {
		t.Errorf("Generation.Temperature = %q, want %q", cfg.Generation.Temperature, "0.1")
	}
	if cfg.Generation.MaxTokens != "8192" {
		t.Errorf("Generation.MaxTokens = %q, want %q", cfg.Generation.MaxTokens, "8192")
	}
	if cfg.Generation.TopP != "0.95" {
		t.Errorf("Generation.TopP = %q, want %q", cfg.Generation.TopP, "0.95")
	}
	if cfg.Generation.TopK != "50" {
		t.Errorf("Generation.TopK = %q, want %q", cfg.Generation.TopK, "50")
	}
	if cfg.SidecarModel != "cli-sidecar" {
		t.Errorf("SidecarModel = %q, want %q", cfg.SidecarModel, "cli-sidecar")
	}
	if cfg.GuardModel != "cli-guard" {
		t.Errorf("GuardModel = %q, want %q", cfg.GuardModel, "cli-guard")
	}
}

func TestApplyCLIModelOverrides_Partial(t *testing.T) {
	cfg := agent.TeamConfig{
		Generation:   agent.GenerationParams{Model: "from-yaml", Temperature: "0.7"},
		SidecarModel: "sidecar-from-yaml",
		GuardModel:   "guard-from-yaml",
	}
	// Only --model is set. Sidecar/guard fall back to --model,
	// so they get clobbered; temperature stays unchanged.
	applyCLIModelOverrides(&cfg, ModelCLIOverrides{Model: "cli-model"})

	if cfg.Generation.Model != "cli-model" {
		t.Errorf("Generation.Model = %q, want %q", cfg.Generation.Model, "cli-model")
	}
	if cfg.Generation.Temperature != "0.7" {
		t.Errorf("Generation.Temperature = %q, want unchanged %q", cfg.Generation.Temperature, "0.7")
	}
	if cfg.SidecarModel != "cli-model" {
		t.Errorf("SidecarModel = %q, want fallback %q (--model)", cfg.SidecarModel, "cli-model")
	}
	if cfg.GuardModel != "cli-model" {
		t.Errorf("GuardModel = %q, want fallback %q (--model)", cfg.GuardModel, "cli-model")
	}
}

func TestPropagateTeamGenerationToAgents(t *testing.T) {
	session := &team.TeamSession{
		Config: agent.TeamConfig{
			Generation: agent.GenerationParams{
				Model:       "cli-model",
				Temperature: "0.2",
				MaxTokens:   "4096",
				TopP:        "0.9",
				TopK:        "40",
			},
			ProviderURL: "http://cli-host:11434/v1",
		},
		Agents: map[string]*agent.AgentDef{
			"helper": {
				Name: "Helper",
				// Intentionally stale values to verify they get overwritten.
				Generation:  agent.GenerationParams{Model: "stale"},
				ProviderURL: "http://stale:11434/v1",
			},
			"coordinator": {
				Name:       "coordinator",
				Generation: agent.GenerationParams{Model: "stale"},
			},
		},
	}

	propagateTeamGenerationToAgents(session)

	for k, def := range session.Agents {
		if def.Generation.Model != "cli-model" {
			t.Errorf("[%s] Generation.Model = %q, want %q", k, def.Generation.Model, "cli-model")
		}
		if def.Generation.Temperature != "0.2" {
			t.Errorf("[%s] Generation.Temperature = %q, want %q", k, def.Generation.Temperature, "0.2")
		}
		if def.Generation.MaxTokens != "4096" {
			t.Errorf("[%s] Generation.MaxTokens = %q, want %q", k, def.Generation.MaxTokens, "4096")
		}
		if def.Generation.TopP != "0.9" {
			t.Errorf("[%s] Generation.TopP = %q, want %q", k, def.Generation.TopP, "0.9")
		}
		if def.Generation.TopK != "40" {
			t.Errorf("[%s] Generation.TopK = %q, want %q", k, def.Generation.TopK, "40")
		}
		if def.ProviderURL != "http://cli-host:11434/v1" {
			t.Errorf("[%s] ProviderURL = %q, want %q", k, def.ProviderURL, "http://cli-host:11434/v1")
		}
	}
}

func TestPropagateTeamGenerationToAgents_EmptyConfigDoesNotClobber(t *testing.T) {
	// If team-level Generation fields are empty, per-agent values must
	// NOT be overwritten with empty strings (we'd lose the agent's own
	// model configuration).
	session := &team.TeamSession{
		Config: agent.TeamConfig{
			Generation: agent.GenerationParams{}, // all empty
		},
		Agents: map[string]*agent.AgentDef{
			"helper": {
				Name: "Helper",
				Generation: agent.GenerationParams{
					Model:       "agent-own-model",
					Temperature: "0.5",
				},
			},
		},
	}

	propagateTeamGenerationToAgents(session)

	helper := session.Agents["helper"]
	if helper.Generation.Model != "agent-own-model" {
		t.Errorf("Generation.Model = %q, want %q (must not clobber)", helper.Generation.Model, "agent-own-model")
	}
	if helper.Generation.Temperature != "0.5" {
		t.Errorf("Generation.Temperature = %q, want %q (must not clobber)", helper.Generation.Temperature, "0.5")
	}
}

func TestPropagateTeamGenerationToAgents_NilSafe(t *testing.T) {
	// Should not panic.
	propagateTeamGenerationToAgents(nil)
}

func TestApplyCLIModelOverrides_SidecarFallsBackToModel(t *testing.T) {
	cfg := agent.TeamConfig{}
	applyCLIModelOverrides(&cfg, ModelCLIOverrides{
		Model: "ollama/qwen3:8b",
		// SidecarModel intentionally empty
	})
	if cfg.SidecarModel != "ollama/qwen3:8b" {
		t.Errorf("SidecarModel = %q, want %q (fallback to --model)", cfg.SidecarModel, "ollama/qwen3:8b")
	}
}

func TestApplyCLIModelOverrides_GuardFallsBackToModel(t *testing.T) {
	cfg := agent.TeamConfig{}
	applyCLIModelOverrides(&cfg, ModelCLIOverrides{
		Model: "ollama/qwen3:8b",
		// GuardModel intentionally empty
	})
	if cfg.GuardModel != "ollama/qwen3:8b" {
		t.Errorf("GuardModel = %q, want %q (fallback to --model)", cfg.GuardModel, "ollama/qwen3:8b")
	}
}

func TestApplyCLIModelOverrides_ExplicitSidecarWins(t *testing.T) {
	// When both --model and --sidecar-model are set, explicit
	// --sidecar-model wins; no fallback.
	cfg := agent.TeamConfig{}
	applyCLIModelOverrides(&cfg, ModelCLIOverrides{
		Model:        "ollama/qwen3:8b",
		SidecarModel: "ollama/qwen3:1b",
	})
	if cfg.SidecarModel != "ollama/qwen3:1b" {
		t.Errorf("SidecarModel = %q, want explicit %q", cfg.SidecarModel, "ollama/qwen3:1b")
	}
	// And guard still falls back to --model
	if cfg.GuardModel != "ollama/qwen3:8b" {
		t.Errorf("GuardModel = %q, want fallback %q", cfg.GuardModel, "ollama/qwen3:8b")
	}
}

func TestApplyCLIModelOverrides_NoModelNoFallback(t *testing.T) {
	// No --model, no --sidecar-model, no --guard-model:
	// everything stays empty (no fallback, no clobber).
	cfg := agent.TeamConfig{}
	applyCLIModelOverrides(&cfg, ModelCLIOverrides{})
	if cfg.SidecarModel != "" {
		t.Errorf("SidecarModel = %q, want empty", cfg.SidecarModel)
	}
	if cfg.GuardModel != "" {
		t.Errorf("GuardModel = %q, want empty", cfg.GuardModel)
	}
}
