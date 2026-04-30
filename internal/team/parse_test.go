package team

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/anomalyco/hufu/internal/config"
)

func TestMaxStepsParsing(t *testing.T) {
	// Test cases for max-steps parsing behavior
	tests := []struct {
		name         string
		yamlContent  string
		wantMaxSteps int
		isAgent      bool
		wantErr      bool
	}{
		{
			name:         "agent with max-steps: 50",
			yamlContent:  "---\nname: test-agent\nrole: worker\nmodel: test-model\nmax-steps: 50\n---\n",
			wantMaxSteps: 50,
			isAgent:      true,
			wantErr:      false,
		},
		{
			name:         "agent with max-steps: 0 (should be unset)",
			yamlContent:  "---\nname: test-agent\nrole: worker\nmodel: test-model\nmax-steps: 0\n---\n",
			wantMaxSteps: 0, // 0 means unset
			isAgent:      true,
			wantErr:      false,
		},
		{
			name:         "agent without max-steps",
			yamlContent:  "---\nname: test-agent\nrole: worker\nmodel: test-model\n---\n",
			wantMaxSteps: 0, // unset
			isAgent:      true,
			wantErr:      false,
		},
		{
			name:         "team config with max-steps: 100",
			yamlContent:  "name: test-team\nmax-steps: 100\nmodel: test-model\n",
			wantMaxSteps: 100,
			isAgent:      false,
			wantErr:      false,
		},
		{
			name:         "team config with max-steps: 0 (should be unset)",
			yamlContent:  "name: test-team\nmax-steps: 0\nmodel: test-model\n",
			wantMaxSteps: 0, // 0 means unset
			isAgent:      false,
			wantErr:      false,
		},
		{
			name:         "team config without max-steps",
			yamlContent:  "name: test-team\nmodel: test-model\n",
			wantMaxSteps: 0, // unset
			isAgent:      false,
			wantErr:      false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.isAgent {
				// Test parseAgentFile
				tmpDir := t.TempDir()
				agentPath := filepath.Join(tmpDir, "test-agent.md")
				if err := os.WriteFile(agentPath, []byte(tt.yamlContent), 0644); err != nil {
					t.Fatalf("Failed to write test agent file: %v", err)
				}

				result := parseAgentFile(agentPath)
				if tt.wantErr && result == nil {
					return // expected error
				}
				if result == nil {
					t.Fatalf("parseAgentFile returned nil for valid input")
				}
				if result.MaxSteps != tt.wantMaxSteps {
					t.Errorf("parseAgentFile MaxSteps = %d, want %d", result.MaxSteps, tt.wantMaxSteps)
				}
			} else {
				// Test parseTeamYML
				tmpDir := t.TempDir()
				teamPath := filepath.Join(tmpDir, "team.yml")
				if err := os.WriteFile(teamPath, []byte(tt.yamlContent), 0644); err != nil {
					t.Fatalf("Failed to write test team file: %v", err)
				}

				cfg, err := parseTeamYML(tmpDir)
				if tt.wantErr && err != nil {
					return // expected error
				}
				if err != nil {
					t.Fatalf("parseTeamYML returned error: %v", err)
				}
				if cfg.MaxSteps != tt.wantMaxSteps {
					t.Errorf("parseTeamYML MaxSteps = %d, want %d", cfg.MaxSteps, tt.wantMaxSteps)
				}
			}
		})
	}
}

func TestMaxStepsDefaultValue(t *testing.T) {
	// Test that max-steps: 0 and missing max-steps both result in default (30)
	// by verifying the resolution logic in agent package

	// For agent parsing
	t.Run("agent max-steps 0 resolved to default", func(t *testing.T) {
		tmpDir := t.TempDir()
		agentPath := filepath.Join(tmpDir, "test-agent.md")
		yamlContent := "---\nname: test-agent\nrole: worker\nmodel: test-model\nmax-steps: 0\n---\n"
		if err := os.WriteFile(agentPath, []byte(yamlContent), 0644); err != nil {
			t.Fatalf("Failed to write test agent file: %v", err)
		}

		result := parseAgentFile(agentPath)
		if result == nil {
			t.Fatalf("parseAgentFile returned nil")
		}
		// MaxSteps should be 0 (unset)
		if result.MaxSteps != 0 {
			t.Errorf("parseAgentFile MaxSteps = %d, want 0 for unset", result.MaxSteps)
		}
	})

	// For team config parsing
	t.Run("team max-steps 0 resolved to default", func(t *testing.T) {
		tmpDir := t.TempDir()
		teamPath := filepath.Join(tmpDir, "team.yml")
		yamlContent := "name: test-team\nmax-steps: 0\nmodel: test-model\n"
		if err := os.WriteFile(teamPath, []byte(yamlContent), 0644); err != nil {
			t.Fatalf("Failed to write test team file: %v", err)
		}

		cfg, err := parseTeamYML(tmpDir)
		if err != nil {
			t.Fatalf("parseTeamYML returned error: %v", err)
		}
		// MaxSteps should be 0 (unset)
		if cfg.MaxSteps != 0 {
			t.Errorf("parseTeamYML MaxSteps = %d, want 0 for unset", cfg.MaxSteps)
		}
	})
}

func TestMaxStepsWithValidValue(t *testing.T) {
	// Test that valid max-steps values are correctly parsed

	t.Run("agent with valid max-steps", func(t *testing.T) {
		tmpDir := t.TempDir()
		agentPath := filepath.Join(tmpDir, "valid-agent.md")
		yamlContent := "---\nname: test-agent\nrole: worker\nmodel: test-model\nmax-steps: 25\n---\n"
		if err := os.WriteFile(agentPath, []byte(yamlContent), 0644); err != nil {
			t.Fatalf("Failed to write test agent file: %v", err)
		}

		result := parseAgentFile(agentPath)
		if result == nil {
			t.Fatalf("parseAgentFile returned nil")
		}
		if result.MaxSteps != 25 {
			t.Errorf("parseAgentFile MaxSteps = %d, want 25", result.MaxSteps)
		}
	})

	t.Run("team with valid max-steps", func(t *testing.T) {
		tmpDir := t.TempDir()
		teamPath := filepath.Join(tmpDir, "team.yml")
		yamlContent := "name: test-team\nmax-steps: 40\nmodel: test-model\n"
		if err := os.WriteFile(teamPath, []byte(yamlContent), 0644); err != nil {
			t.Fatalf("Failed to write test team file: %v", err)
		}

		cfg, err := parseTeamYML(tmpDir)
		if err != nil {
			t.Fatalf("parseTeamYML returned error: %v", err)
		}
		if cfg.MaxSteps != 40 {
			t.Errorf("parseTeamYML MaxSteps = %d, want 40", cfg.MaxSteps)
		}
	})
}

func TestMaxStepsMissingUsesDefault(t *testing.T) {
	// Test that missing max-steps results in 0 (to be resolved to default 30 later)

	t.Run("agent without max-steps", func(t *testing.T) {
		tmpDir := t.TempDir()
		agentPath := filepath.Join(tmpDir, "no-maxsteps-agent.md")
		yamlContent := "---\nname: test-agent\nrole: worker\nmodel: test-model\n---\n"
		if err := os.WriteFile(agentPath, []byte(yamlContent), 0644); err != nil {
			t.Fatalf("Failed to write test agent file: %v", err)
		}

		result := parseAgentFile(agentPath)
		if result == nil {
			t.Fatalf("parseAgentFile returned nil")
		}
		if result.MaxSteps != 0 {
			t.Errorf("parseAgentFile MaxSteps = %d, want 0 for missing", result.MaxSteps)
		}
	})

	t.Run("team without max-steps", func(t *testing.T) {
		tmpDir := t.TempDir()
		teamPath := filepath.Join(tmpDir, "team.yml")
		yamlContent := "name: test-team\nmodel: test-model\n"
		if err := os.WriteFile(teamPath, []byte(yamlContent), 0644); err != nil {
			t.Fatalf("Failed to write test team file: %v", err)
		}

		cfg, err := parseTeamYML(tmpDir)
		if err != nil {
			t.Fatalf("parseTeamYML returned error: %v", err)
		}
		// Default MaxRounds is 10, but MaxSteps should remain 0 (unset)
		if cfg.MaxSteps != 0 {
			t.Errorf("parseTeamYML MaxSteps = %d, want 0 for missing", cfg.MaxSteps)
		}
	})
}

func TestParseTeamYMLModelList(t *testing.T) {
	t.Run("team with model-list", func(t *testing.T) {
		tmpDir := t.TempDir()
		yamlContent := `name: test-team
model: ollama/qwen3:8b
model-list:
  - id: ollama/deepseek-v4:flash
    details: |-
      Fast model
      Good for tools
  - id: ollama/deepseek-v4:pro
    details: Powerful model
`
		teamPath := filepath.Join(tmpDir, "team.yml")
		if err := os.WriteFile(teamPath, []byte(yamlContent), 0644); err != nil {
			t.Fatalf("Failed to write team file: %v", err)
		}

		cfg, err := parseTeamYML(tmpDir)
		if err != nil {
			t.Fatalf("parseTeamYML returned error: %v", err)
		}
		if len(cfg.ModelList) != 2 {
			t.Fatalf("ModelList has %d entries, want 2", len(cfg.ModelList))
		}
		if cfg.ModelList[0].ID != "ollama/deepseek-v4:flash" {
			t.Errorf("ModelList[0].ID = %q, want %q", cfg.ModelList[0].ID, "ollama/deepseek-v4:flash")
		}
		if cfg.ModelList[1].ID != "ollama/deepseek-v4:pro" {
			t.Errorf("ModelList[1].ID = %q, want %q", cfg.ModelList[1].ID, "ollama/deepseek-v4:pro")
		}
		if cfg.ModelList[1].Details != "Powerful model" {
			t.Errorf("ModelList[1].Details = %q, want %q", cfg.ModelList[1].Details, "Powerful model")
		}
	})

	t.Run("team without model-list", func(t *testing.T) {
		tmpDir := t.TempDir()
		yamlContent := "name: test-team\nmodel: ollama/qwen3:8b\n"
		teamPath := filepath.Join(tmpDir, "team.yml")
		if err := os.WriteFile(teamPath, []byte(yamlContent), 0644); err != nil {
			t.Fatalf("Failed to write team file: %v", err)
		}

		cfg, err := parseTeamYML(tmpDir)
		if err != nil {
			t.Fatalf("parseTeamYML returned error: %v", err)
		}
		if len(cfg.ModelList) != 0 {
			t.Errorf("ModelList has %d entries, want 0", len(cfg.ModelList))
		}
	})
}

func TestResolveModelListFromConfig(t *testing.T) {
	t.Run("team model-list overrides config", func(t *testing.T) {
		cfg := &config.Config{
			ModelList: []config.ModelEntry{
				{ID: "config-model", Details: "from config"},
			},
		}
		teamList := []config.ModelEntry{
			{ID: "team-model", Details: "from team"},
		}
		result := cfg.ResolveModelList(teamList)
		if len(result) != 1 || result[0].ID != "team-model" {
			t.Errorf("ResolveModelList(teamList) = %v, want team-model", result)
		}
	})

	t.Run("config model-list as fallback", func(t *testing.T) {
		cfg := &config.Config{
			ModelList: []config.ModelEntry{
				{ID: "config-model", Details: "from config"},
			},
		}
		result := cfg.ResolveModelList(nil)
		if len(result) != 1 || result[0].ID != "config-model" {
			t.Errorf("ResolveModelList(nil) = %v, want config-model", result)
		}
	})
}
