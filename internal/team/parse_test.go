package team

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/config"
	"github.com/kjelly/hufu/internal/skill"
)

func TestParseTeamDelegationPolicy(t *testing.T) {
	tmpDir := t.TempDir()
	yamlContent := `name: test-team
delegation:
  allowed-workers: [reader, probe]
  initial-batch:
    agents: [reader, probe]
    exact: true
    first-tool: agent
    bind-contracts: true
  bind-task-goal-contracts: true
  no-redispatch-after-success: [reader, probe]
  forbid-context-files: true
`
	if err := os.WriteFile(filepath.Join(tmpDir, "team.yaml"), []byte(yamlContent), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := parseTeamYML(tmpDir, nil)
	if err != nil {
		t.Fatalf("parseTeamYML: %v", err)
	}
	if !cfg.Delegation.RequireExactInitialBatch {
		t.Fatal("RequireExactInitialBatch = false, want true")
	}
	if want := []string{"reader", "probe"}; !reflect.DeepEqual(cfg.Delegation.AllowedWorkers, want) {
		t.Fatalf("AllowedWorkers = %v, want %v", cfg.Delegation.AllowedWorkers, want)
	}
	if want := []string{"reader", "probe"}; !reflect.DeepEqual(cfg.Delegation.InitialBatch, want) {
		t.Fatalf("InitialBatch = %v, want %v", cfg.Delegation.InitialBatch, want)
	}
	if got, want := cfg.Delegation.InitialCoordinatorTool, "agent"; got != want {
		t.Fatalf("InitialCoordinatorTool = %q, want %q", got, want)
	}
	if !cfg.Delegation.BindInitialTaskContracts {
		t.Fatal("BindInitialTaskContracts = false, want true")
	}
	if !cfg.Delegation.BindTaskGoalContracts {
		t.Fatal("BindTaskGoalContracts = false, want true")
	}
	if want := []string{"reader", "probe"}; !reflect.DeepEqual(cfg.Delegation.NoRedispatchAfterSuccess, want) {
		t.Fatalf("NoRedispatchAfterSuccess = %v, want %v", cfg.Delegation.NoRedispatchAfterSuccess, want)
	}
	if !cfg.Delegation.ForbidContextFiles {
		t.Fatal("ForbidContextFiles = false, want true")
	}
}

func TestParseTeamRuntimeWorkflowContract(t *testing.T) {
	tmpDir := t.TempDir()
	yamlContent := `workflow:
  phases: [prepare, audit, execute, verify]
policies:
  require_phase_success: true
  allow_phase_skip: false
capabilities:
  required: [structured-actions]
verification:
  required: true
retry:
  transient:
    max_attempts: 2
  repair:
    max_attempts_per_failure_signature: 3
`
	if err := os.WriteFile(filepath.Join(tmpDir, "team.yaml"), []byte(yamlContent), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := parseTeamYML(tmpDir, nil)
	if err != nil {
		t.Fatalf("parseTeamYML: %v", err)
	}
	if want := []string{"prepare", "audit", "execute", "verify"}; !reflect.DeepEqual(cfg.Workflow.Phases, want) {
		t.Fatalf("workflow phases = %#v, want %#v", cfg.Workflow.Phases, want)
	}
	if !cfg.Policies.RequirePhaseSuccess || cfg.Policies.AllowPhaseSkip {
		t.Fatalf("workflow policies = %#v", cfg.Policies)
	}
	if want := []string{"structured-actions"}; !reflect.DeepEqual(cfg.Capabilities.Required, want) {
		t.Fatalf("workflow capabilities = %#v, want %#v", cfg.Capabilities.Required, want)
	}
	if !cfg.Verification.Required {
		t.Fatal("verification.required = false, want true")
	}
	if cfg.Retry.Transient.MaxAttempts != 2 || cfg.Retry.Repair.MaxAttemptsPerFailureSignature != 3 {
		t.Fatalf("workflow retry policy = %#v", cfg.Retry)
	}
}

func TestParseTeamYMLRejectsUnknownRuntimeSchemaFields(t *testing.T) {
	tmpDir := t.TempDir()
	yamlContent := `workflow:
  phases: [prepare, audit, execute, verify]
policies:
  fail_fats: true
`
	if err := os.WriteFile(filepath.Join(tmpDir, "team.yaml"), []byte(yamlContent), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := parseTeamYML(tmpDir, nil); err == nil || !strings.Contains(err.Error(), "fail_fats") {
		t.Fatalf("parseTeamYML error = %v, want unknown-field validation for fail_fats", err)
	}
}

func TestParseTeamYMLMinimumCoordinatorRounds(t *testing.T) {
	tmpDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmpDir, "team.yaml"), []byte("max-rounds: 12\nminimum-coordinator-rounds: 8\nmax-retries: 0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := parseTeamYML(tmpDir, nil)
	if err != nil {
		t.Fatalf("parseTeamYML: %v", err)
	}
	if cfg.MaxRounds != 12 || cfg.MinimumCoordinatorRounds != 8 || cfg.MaxRetries != 0 {
		t.Fatalf("parsed coordinator/retry budgets = (%d, %d, %d), want (12, 8, 0)", cfg.MaxRounds, cfg.MinimumCoordinatorRounds, cfg.MaxRetries)
	}
}

func TestMaxStepsParsing(t *testing.T) {
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
			wantMaxSteps: 0,
			isAgent:      true,
			wantErr:      false,
		},
		{
			name:         "agent without max-steps",
			yamlContent:  "---\nname: test-agent\nrole: worker\nmodel: test-model\n---\n",
			wantMaxSteps: 0,
			isAgent:      true,
			wantErr:      false,
		},
		{
			name:         "team config with max-steps: 100",
			yamlContent:  "name: test-team\nacceptance: 'true'\nmax-steps: 100\nmodel: test-model\n",
			wantMaxSteps: 100,
			isAgent:      false,
			wantErr:      false,
		},
		{
			name:         "team config with max-steps: 0 (should be unset)",
			yamlContent:  "name: test-team\nacceptance: 'true'\nmax-steps: 0\nmodel: test-model\n",
			wantMaxSteps: 0,
			isAgent:      false,
			wantErr:      false,
		},
		{
			name:         "team config without max-steps",
			yamlContent:  "name: test-team\nacceptance: 'true'\nmodel: test-model\n",
			wantMaxSteps: 0,
			isAgent:      false,
			wantErr:      false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.isAgent {
				tmpDir := t.TempDir()
				agentPath := filepath.Join(tmpDir, "test-agent.md")
				if err := os.WriteFile(agentPath, []byte(tt.yamlContent), 0644); err != nil {
					t.Fatalf("Failed to write test agent file: %v", err)
				}

				result, err := parseAgentFile(agentPath, nil)
				if tt.wantErr && err != nil {
					return
				}
				if err != nil {
					t.Fatalf("parseAgentFile returned error: %v", err)
				}
				if result == nil {
					t.Fatalf("parseAgentFile returned nil for valid input")
				}
				if result.MaxSteps != tt.wantMaxSteps {
					t.Errorf("parseAgentFile MaxSteps = %d, want %d", result.MaxSteps, tt.wantMaxSteps)
				}
			} else {
				tmpDir := t.TempDir()
				teamPath := filepath.Join(tmpDir, "team.yml")
				if err := os.WriteFile(teamPath, []byte(tt.yamlContent), 0644); err != nil {
					t.Fatalf("Failed to write test team file: %v", err)
				}

				cfg, err := parseTeamYML(tmpDir, nil)
				if tt.wantErr && err != nil {
					return
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

func TestVerifyTimeoutParsing(t *testing.T) {
	tmpDir := t.TempDir()
	teamPath := filepath.Join(tmpDir, "team.yml")
	yamlContent := "name: test-team\nacceptance: 'true'\nverify-timeout: 45\nmodel: test-model\n"
	if err := os.WriteFile(teamPath, []byte(yamlContent), 0o644); err != nil {
		t.Fatalf("Failed to write test team file: %v", err)
	}

	cfg, err := parseTeamYML(tmpDir, nil)
	if err != nil {
		t.Fatalf("parseTeamYML returned error: %v", err)
	}
	if cfg.VerifyTimeout != 45 {
		t.Fatalf("parseTeamYML VerifyTimeout = %d, want 45", cfg.VerifyTimeout)
	}
}

func TestParseAgentGuardRules(t *testing.T) {
	tests := []struct {
		name        string
		yamlContent string
		wantGuard   []string
	}{
		{
			name:        "agent with guard rules",
			yamlContent: "---\nname: test-agent\nrole: worker\nguard:\n  - no secret leakage\n  - strictly follow format\n---\n",
			wantGuard:   []string{"no secret leakage", "strictly follow format"},
		},
		{
			name:        "agent without guard rules",
			yamlContent: "---\nname: test-agent\nrole: worker\n---\n",
			wantGuard:   nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tmpDir := t.TempDir()
			agentPath := filepath.Join(tmpDir, "test-agent.md")
			if err := os.WriteFile(agentPath, []byte(tt.yamlContent), 0644); err != nil {
				t.Fatalf("Failed to write test agent file: %v", err)
			}

			result, err := parseAgentFile(agentPath, nil)
			if err != nil {
				t.Fatalf("parseAgentFile returned error: %v", err)
			}
			if result == nil {
				t.Fatalf("parseAgentFile returned nil for valid input")
			}

			if len(result.Guard) != len(tt.wantGuard) {
				t.Errorf("parseAgentFile Guard length = %d, want %d", len(result.Guard), len(tt.wantGuard))
			}
			for i, rule := range result.Guard {
				if rule != tt.wantGuard[i] {
					t.Errorf("parseAgentFile Guard[%d] = %q, want %q", i, rule, tt.wantGuard[i])
				}
			}
		})
	}
}

func TestMaxStepsDefaultValue(t *testing.T) {
	t.Run("agent max-steps 0 resolved to default", func(t *testing.T) {
		tmpDir := t.TempDir()
		agentPath := filepath.Join(tmpDir, "agent.md")
		yamlContent := "---\nname: test-agent\nrole: worker\nmax-steps: 0\n---\n"
		if err := os.WriteFile(agentPath, []byte(yamlContent), 0644); err != nil {
			t.Fatalf("Failed to write test agent file: %v", err)
		}

		result, err := parseAgentFile(agentPath, nil)
		if err != nil {
			t.Fatalf("parseAgentFile returned error: %v", err)
		}
		if result == nil {
			t.Fatalf("parseAgentFile returned nil")
		}
		if result.MaxSteps != 0 {
			t.Errorf("parseAgentFile MaxSteps = %d, want 0 for unset", result.MaxSteps)
		}
	})

	t.Run("team max-steps 0 resolved to default", func(t *testing.T) {
		tmpDir := t.TempDir()
		teamPath := filepath.Join(tmpDir, "team.yml")
		yamlContent := "name: test-team\nacceptance: 'true'\nmax-steps: 0\nmodel: test-model\n"
		if err := os.WriteFile(teamPath, []byte(yamlContent), 0644); err != nil {
			t.Fatalf("Failed to write test team file: %v", err)
		}

		cfg, err := parseTeamYML(tmpDir, nil)
		if err != nil {
			t.Fatalf("parseTeamYML returned error: %v", err)
		}
		if cfg.MaxSteps != 0 {
			t.Errorf("parseTeamYML MaxSteps = %d, want 0 for unset", cfg.MaxSteps)
		}
	})
}

func TestMaxStepsWithValidValue(t *testing.T) {
	t.Run("agent with valid max-steps", func(t *testing.T) {
		tmpDir := t.TempDir()
		agentPath := filepath.Join(tmpDir, "valid-agent.md")
		yamlContent := "---\nname: test-agent\nrole: worker\nmodel: test-model\nmax-steps: 25\n---\n"
		if err := os.WriteFile(agentPath, []byte(yamlContent), 0644); err != nil {
			t.Fatalf("Failed to write test agent file: %v", err)
		}

		result, err := parseAgentFile(agentPath, nil)
		if err != nil {
			t.Fatalf("parseAgentFile returned error: %v", err)
		}
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
		yamlContent := "name: test-team\nacceptance: 'true'\nmax-steps: 40\nmodel: test-model\n"
		if err := os.WriteFile(teamPath, []byte(yamlContent), 0644); err != nil {
			t.Fatalf("Failed to write test team file: %v", err)
		}

		cfg, err := parseTeamYML(tmpDir, nil)
		if err != nil {
			t.Fatalf("parseTeamYML returned error: %v", err)
		}
		if cfg.MaxSteps != 40 {
			t.Errorf("parseTeamYML MaxSteps = %d, want 40", cfg.MaxSteps)
		}
	})
}

func TestMaxStepsMissingUsesDefault(t *testing.T) {
	t.Run("agent without max-steps", func(t *testing.T) {
		tmpDir := t.TempDir()
		agentPath := filepath.Join(tmpDir, "no-maxsteps-agent.md")
		yamlContent := "---\nname: test-agent\nrole: worker\nmodel: test-model\n---\n"
		if err := os.WriteFile(agentPath, []byte(yamlContent), 0644); err != nil {
			t.Fatalf("Failed to write test agent file: %v", err)
		}

		result, err := parseAgentFile(agentPath, nil)
		if err != nil {
			t.Fatalf("parseAgentFile returned error: %v", err)
		}
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
		yamlContent := "name: test-team\nacceptance: 'true'\nmodel: test-model\n"
		if err := os.WriteFile(teamPath, []byte(yamlContent), 0644); err != nil {
			t.Fatalf("Failed to write test team file: %v", err)
		}

		cfg, err := parseTeamYML(tmpDir, nil)
		if err != nil {
			t.Fatalf("parseTeamYML returned error: %v", err)
		}
		if cfg.MaxSteps != 0 {
			t.Errorf("parseTeamYML MaxSteps = %d, want 0 for missing", cfg.MaxSteps)
		}
	})
}

func TestParseTeamYMLModelList(t *testing.T) {
	t.Run("team with model-list", func(t *testing.T) {
		tmpDir := t.TempDir()
		yamlContent := `name: test-team
acceptance: 'true'
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

		cfg, err := parseTeamYML(tmpDir, nil)
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
		yamlContent := "name: test-team\nacceptance: 'true'\nmodel: ollama/qwen3:8b\n"
		teamPath := filepath.Join(tmpDir, "team.yml")
		if err := os.WriteFile(teamPath, []byte(yamlContent), 0644); err != nil {
			t.Fatalf("Failed to write team file: %v", err)
		}

		cfg, err := parseTeamYML(tmpDir, nil)
		if err != nil {
			t.Fatalf("parseTeamYML returned error: %v", err)
		}
		if len(cfg.ModelList) != 0 {
			t.Errorf("ModelList has %d entries, want 0", len(cfg.ModelList))
		}
	})
}

func TestParseAgentFile_MissingName(t *testing.T) {
	t.Run("no name field returns error", func(t *testing.T) {
		tmpDir := t.TempDir()
		agentPath := filepath.Join(tmpDir, "no-name.md")
		content := "---\nrole: worker\ndescription: agent without name\n---\nSome content"
		if err := os.WriteFile(agentPath, []byte(content), 0644); err != nil {
			t.Fatalf("Failed to write test agent file: %v", err)
		}

		_, err := parseAgentFile(agentPath, nil)
		if err == nil {
			t.Error("expected error for agent file without name, got nil")
		}
		if err != nil && !strings.Contains(err.Error(), "missing required 'name'") {
			t.Errorf("expected error about missing 'name', got: %v", err)
		}
	})

	t.Run("empty name field returns error", func(t *testing.T) {
		tmpDir := t.TempDir()
		agentPath := filepath.Join(tmpDir, "empty-name.md")
		content := "---\nname: \"\"\nrole: worker\n---\nSome content"
		if err := os.WriteFile(agentPath, []byte(content), 0644); err != nil {
			t.Fatalf("Failed to write test agent file: %v", err)
		}

		_, err := parseAgentFile(agentPath, nil)
		if err == nil {
			t.Error("expected error for agent file with empty name, got nil")
		}
	})
}

func TestParseAgentFile_PlainMarkdown(t *testing.T) {
	t.Run("plain markdown without frontmatter returns nil", func(t *testing.T) {
		tmpDir := t.TempDir()
		agentPath := filepath.Join(tmpDir, "readme.md")
		content := "# Just a regular markdown file\n\nSome content here."
		if err := os.WriteFile(agentPath, []byte(content), 0644); err != nil {
			t.Fatalf("Failed to write test file: %v", err)
		}

		def, err := parseAgentFile(agentPath, nil)
		if err != nil {
			t.Errorf("expected nil error for plain markdown, got: %v", err)
		}
		if def != nil {
			t.Errorf("expected nil def for plain markdown, got: %v", def)
		}
	})

	t.Run("malformed frontmatter returns error", func(t *testing.T) {
		tmpDir := t.TempDir()
		agentPath := filepath.Join(tmpDir, "bad.md")
		content := "---\nname: test\nrole: worker\n"
		if err := os.WriteFile(agentPath, []byte(content), 0644); err != nil {
			t.Fatalf("Failed to write test file: %v", err)
		}

		_, err := parseAgentFile(agentPath, nil)
		if err == nil {
			t.Error("expected error for malformed frontmatter, got nil")
		}
		if err != nil && !strings.Contains(err.Error(), "malformed frontmatter") {
			t.Errorf("expected error about malformed frontmatter, got: %v", err)
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

func TestParseTeamYML_MissingFile(t *testing.T) {
	tmpDir := t.TempDir()
	// No team.yml / team.yaml in tmpDir

	cfg, err := parseTeamYML(tmpDir, nil)
	if err != nil {
		t.Fatalf("parseTeamYML returned error for missing file: %v", err)
	}
	if cfg.Name != "" {
		t.Errorf("cfg.Name = %q, want empty (LoadTeam falls back to dir name)", cfg.Name)
	}
	if cfg.MaxRounds != 10 {
		t.Errorf("cfg.MaxRounds = %d, want default 10", cfg.MaxRounds)
	}
	if cfg.Generation.Temperature != agent.DefaultTemperature {
		t.Errorf("cfg.Generation.Temperature = %q, want default %q", cfg.Generation.Temperature, agent.DefaultTemperature)
	}
	if cfg.Generation.MaxTokens != agent.DefaultMaxTokens {
		t.Errorf("cfg.Generation.MaxTokens = %q, want default %q", cfg.Generation.MaxTokens, agent.DefaultMaxTokens)
	}
	if cfg.Generation.TopP != agent.DefaultTopP {
		t.Errorf("cfg.Generation.TopP = %q, want default %q", cfg.Generation.TopP, agent.DefaultTopP)
	}
	if cfg.WorkspaceDir != "workspace" {
		t.Errorf("cfg.WorkspaceDir = %q, want default %q", cfg.WorkspaceDir, "workspace")
	}
	if cfg.Timeout != 600 {
		t.Errorf("cfg.Timeout = %d, want default 600", cfg.Timeout)
	}
	if cfg.MaxRetries != 2 {
		t.Errorf("cfg.MaxRetries = %d, want default 2", cfg.MaxRetries)
	}
	if cfg.Generation.Model != "" {
		t.Errorf("cfg.Generation.Model = %q, want empty", cfg.Generation.Model)
	}
}

func TestParseTeamYML_GenerationOverrides(t *testing.T) {
	tmpDir := t.TempDir()
	content := "name: custom\ntemperature: \"0.7\"\ntop-p: \"0.8\"\n"
	if err := os.WriteFile(filepath.Join(tmpDir, "team.yaml"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := parseTeamYML(tmpDir, nil)
	if err != nil {
		t.Fatalf("parseTeamYML returned error: %v", err)
	}
	if cfg.Generation.Temperature != "0.7" {
		t.Errorf("cfg.Generation.Temperature = %q, want %q", cfg.Generation.Temperature, "0.7")
	}
	if cfg.Generation.TopP != "0.8" {
		t.Errorf("cfg.Generation.TopP = %q, want %q", cfg.Generation.TopP, "0.8")
	}
}

func TestLoadTeam_NoYAMLDirName(t *testing.T) {
	tmpDir := t.TempDir()
	teamDir := filepath.Join(tmpDir, "myteam")
	if err := os.MkdirAll(teamDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(teamDir, "team.yml"), []byte("name: myteam\ngoal-mode: exploratory\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Provide one valid agent .md so LoadTeam has at least one agent.
	agentPath := filepath.Join(teamDir, "worker.md")
	agentContent := "---\nname: worker\nrole: worker\n---\nI am a worker."
	if err := os.WriteFile(agentPath, []byte(agentContent), 0o644); err != nil {
		t.Fatal(err)
	}

	session, err := LoadTeam(teamDir, nil, nil, DefaultProviderRegistry)
	if err != nil {
		t.Fatalf("LoadTeam returned error: %v", err)
	}
	if session.Config.Name != "myteam" {
		t.Errorf("session.Config.Name = %q, want %q (directory basename)", session.Config.Name, "myteam")
	}
	if session.Dir == "" {
		t.Error("session.Dir is empty, want absolute path")
	}
	if _, ok := session.Agents["worker"]; !ok {
		t.Errorf("session.Agents missing %q, got keys: %v", "worker", agentKeys(session.Agents))
	}
}

func TestLoadTeam_DiscoversProjectSkillsNotTeamAgentSkills(t *testing.T) {
	projectDir := t.TempDir()
	teamDir := filepath.Join(projectDir, "team")
	if err := os.MkdirAll(teamDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(teamDir, "team.yml"), []byte("name: test-team\nskills: project-skill\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(teamDir, "worker.md"), []byte("---\nname: worker\nrole: worker\n---\nI am a worker."), 0o644); err != nil {
		t.Fatal(err)
	}

	projectSkillDir := filepath.Join(projectDir, ".agents", "skills", "project-skill")
	if err := os.MkdirAll(projectSkillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectSkillDir, "SKILL.md"), []byte("---\nname: project-skill\ndescription: Project skill\n---\nProject skill body. See .agents/skills/project-dependency/SKILL.md."), 0o644); err != nil {
		t.Fatal(err)
	}
	dependencyDir := filepath.Join(projectDir, ".agents", "skills", "project-dependency")
	if err := os.MkdirAll(dependencyDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dependencyDir, "SKILL.md"), []byte("---\nname: project-dependency\ndescription: Project dependency\n---\nDependency body."), 0o644); err != nil {
		t.Fatal(err)
	}

	legacySkillDir := filepath.Join(teamDir, ".agents", "skills", "legacy-skill")
	if err := os.MkdirAll(legacySkillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacySkillDir, "SKILL.md"), []byte("---\nname: legacy-skill\ndescription: Legacy skill\n---\nLegacy skill body."), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Chdir(projectDir)
	t.Setenv("HOME", t.TempDir())
	session, err := LoadTeam(teamDir, nil, nil, DefaultProviderRegistry)
	if err != nil {
		t.Fatalf("LoadTeam returned error: %v", err)
	}

	if !hasSkill(session.Skills, "project-skill") {
		t.Errorf("session.Skills does not include project skill")
	}
	if !hasSkill(session.Skills, "project-dependency") {
		t.Errorf("session.Skills does not include recursively referenced project skill")
	}
	if hasSkill(session.Skills, "legacy-skill") {
		t.Errorf("session.Skills still includes removed team-local .agents skill")
	}
}

func hasSkill(skills []*skill.SkillDef, name string) bool {
	for _, s := range skills {
		if s.Name == name {
			return true
		}
	}
	return false
}

func agentKeys(m map[string]*agent.AgentDef) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

func TestLoadTeam_Helper_NoDuplicates(t *testing.T) {
	tmpDir := t.TempDir()
	teamDir := filepath.Join(tmpDir, "myteam")
	if err := os.MkdirAll(teamDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(teamDir, "team.yml"), []byte("name: myteam\ngoal-mode: exploratory\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	agentPath := filepath.Join(teamDir, "worker.md")
	agentContent := "---\nname: worker\nrole: worker\n---\nI am a worker."
	if err := os.WriteFile(agentPath, []byte(agentContent), 0o644); err != nil {
		t.Fatal(err)
	}

	session, err := LoadTeam(teamDir, nil, nil, DefaultProviderRegistry)
	if err != nil {
		t.Fatalf("LoadTeam returned error: %v", err)
	}

	// Built-in Helper is registered under the "helper" key only.
	if _, ok := session.Agents["helper"]; !ok {
		t.Errorf("session.Agents missing %q, got keys: %v", "helper", agentKeys(session.Agents))
	}
	// The legacy double-registration must be gone.
	if _, ok := session.Agents["general-purpose"]; ok {
		t.Error("session.Agents still has legacy key 'general-purpose' — rename did not take effect")
	}
	if _, ok := session.Agents["general-purpose agent"]; ok {
		t.Error("session.Agents still has legacy key 'general-purpose agent' — double-registration regression")
	}

	// Iterating the map must yield each unique Name exactly once.
	seen := map[string]int{}
	for _, def := range session.Agents {
		seen[def.Name]++
	}
	for name, count := range seen {
		if count > 1 {
			t.Errorf("agent %q appears %d times in session.Agents — duplicate registration", name, count)
		}
	}
	if seen["Helper"] != 1 {
		t.Errorf("Helper appears %d time(s), want 1", seen["Helper"])
	}
}

func TestLoadTeam_AGENT_NAMES_NoDuplicates(t *testing.T) {
	tmpDir := t.TempDir()
	teamDir := filepath.Join(tmpDir, "myteam")
	if err := os.MkdirAll(teamDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(teamDir, "team.yml"), []byte("name: myteam\ngoal-mode: exploratory\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	agentPath := filepath.Join(teamDir, "worker.md")
	agentContent := "---\nname: worker\nrole: worker\n---\nI am a worker."
	if err := os.WriteFile(agentPath, []byte(agentContent), 0o644); err != nil {
		t.Fatal(err)
	}

	session, err := LoadTeam(teamDir, nil, nil, DefaultProviderRegistry)
	if err != nil {
		t.Fatalf("LoadTeam returned error: %v", err)
	}

	// Read AGENT_NAMES from session.Config.Vars — the load-team bug fix
	// (parse.go) ensures this map is populated, not nil.
	namesRaw, ok := session.Config.Vars["AGENT_NAMES"]
	if !ok {
		t.Fatalf("session.Config.Vars[AGENT_NAMES] not set (got %d entries)", len(session.Config.Vars))
	}
	names, ok := namesRaw.(string)
	if !ok {
		t.Fatalf("AGENT_NAMES type = %T, want string", namesRaw)
	}

	counts := map[string]int{}
	for _, n := range strings.Split(names, ",") {
		n = strings.TrimSpace(n)
		if n != "" {
			counts[n]++
		}
	}
	for n, c := range counts {
		if c > 1 {
			t.Errorf("AGENT_NAMES contains %q %d times (full: %q) — duplicate", n, c, names)
		}
	}
	if counts["Helper"] != 1 {
		t.Errorf("AGENT_NAMES contains Helper %d times, want 1 (full: %q)", counts["Helper"], names)
	}
}

func TestLoadTeam_SessionConfigVars_Populated(t *testing.T) {
	tmpDir := t.TempDir()
	teamDir := filepath.Join(tmpDir, "myteam")
	if err := os.MkdirAll(teamDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(teamDir, "team.yml"), []byte("name: myteam\ngoal-mode: exploratory\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	agentPath := filepath.Join(teamDir, "worker.md")
	agentContent := "---\nname: worker\nrole: worker\n---\nI am a worker."
	if err := os.WriteFile(agentPath, []byte(agentContent), 0o644); err != nil {
		t.Fatal(err)
	}

	session, err := LoadTeam(teamDir, nil, nil, DefaultProviderRegistry)
	if err != nil {
		t.Fatalf("LoadTeam returned error: %v", err)
	}

	if session.Config.Vars == nil {
		t.Fatal("session.Config.Vars is nil; LoadTeam must populate it after the latent-bug fix")
	}

	// TEAM_NAME is injected by LoadTeam and equals the directory basename.
	teamName, ok := session.Config.Vars["TEAM_NAME"]
	if !ok {
		t.Fatal("session.Config.Vars[TEAM_NAME] not set")
	}
	if teamName != "myteam" {
		t.Errorf("TEAM_NAME = %v, want %q", teamName, "myteam")
	}

	// AGENT_COUNT is the count of non-coordinator agents (worker + Helper = 2).
	agentCount, ok := session.Config.Vars["AGENT_COUNT"]
	if !ok {
		t.Fatal("session.Config.Vars[AGENT_COUNT] not set")
	}
	if agentCount != "2" {
		t.Errorf("AGENT_COUNT = %v, want %q", agentCount, "2")
	}

	// AGENT_NAMES is a comma-separated list of worker agent names.
	agentNames, ok := session.Config.Vars["AGENT_NAMES"]
	if !ok {
		t.Fatal("session.Config.Vars[AGENT_NAMES] not set")
	}
	agentNamesStr, ok := agentNames.(string)
	if !ok {
		t.Fatalf("AGENT_NAMES type = %T, want string", agentNames)
	}
	parts := strings.Split(agentNamesStr, ",")
	if len(parts) != 2 {
		t.Errorf("AGENT_NAMES = %q, expected 2 comma-separated entries (worker + Helper), got %d",
			agentNamesStr, len(parts))
	}
}

func TestParseAgentFile_ToolsGetImpliedWaitFor(t *testing.T) {
	tmpDir := t.TempDir()
	agentPath := filepath.Join(tmpDir, "deployer.md")
	content := "---\nname: deployer\nrole: worker\ntools: bash,sudo,view\n---\nBody"
	if err := os.WriteFile(agentPath, []byte(content), 0644); err != nil {
		t.Fatalf("Failed to write test agent file: %v", err)
	}

	def, err := parseAgentFile(agentPath, nil)
	if err != nil {
		t.Fatalf("parseAgentFile() error = %v", err)
	}
	if !strings.Contains(def.Tools, "wait_for") {
		t.Errorf("Tools = %q, want it to include wait_for (implied by bash/sudo)", def.Tools)
	}
	if !strings.Contains(def.Tools, "bash") || !strings.Contains(def.Tools, "sudo") || !strings.Contains(def.Tools, "view") {
		t.Errorf("Tools = %q, expected original tools preserved", def.Tools)
	}
}

func TestParseAgentFile_ToolsWithoutBashOrSudoUnaffected(t *testing.T) {
	tmpDir := t.TempDir()
	agentPath := filepath.Join(tmpDir, "writer.md")
	content := "---\nname: writer\nrole: worker\ntools: view,write,edit\n---\nBody"
	if err := os.WriteFile(agentPath, []byte(content), 0644); err != nil {
		t.Fatalf("Failed to write test agent file: %v", err)
	}

	def, err := parseAgentFile(agentPath, nil)
	if err != nil {
		t.Fatalf("parseAgentFile() error = %v", err)
	}
	if strings.Contains(def.Tools, "wait_for") {
		t.Errorf("Tools = %q, wait_for should not be implied without bash/sudo", def.Tools)
	}
}

func TestParseTeamYML_GoalMode(t *testing.T) {
	t.Run("valid goal_mode outcome", func(t *testing.T) {
		tmpDir := t.TempDir()
		teamPath := filepath.Join(tmpDir, "team.yml")
		yamlContent := "name: test-team\ngoal-mode: outcome\nacceptance: 'true'\n"
		if err := os.WriteFile(teamPath, []byte(yamlContent), 0644); err != nil {
			t.Fatal(err)
		}
		cfg, err := parseTeamYML(tmpDir, nil)
		if err != nil {
			t.Fatalf("parseTeamYML returned error: %v", err)
		}
		if cfg.GoalMode != "outcome" {
			t.Errorf("cfg.GoalMode = %q, want %q", cfg.GoalMode, "outcome")
		}
	})

	t.Run("valid goal_mode exploratory", func(t *testing.T) {
		tmpDir := t.TempDir()
		teamPath := filepath.Join(tmpDir, "team.yml")
		yamlContent := "name: test-team\ngoal-mode: exploratory\n"
		if err := os.WriteFile(teamPath, []byte(yamlContent), 0644); err != nil {
			t.Fatal(err)
		}
		cfg, err := parseTeamYML(tmpDir, nil)
		if err != nil {
			t.Fatalf("parseTeamYML returned error: %v", err)
		}
		if cfg.GoalMode != "exploratory" {
			t.Errorf("cfg.GoalMode = %q, want %q", cfg.GoalMode, "exploratory")
		}
	})

	t.Run("invalid goal_mode produces error", func(t *testing.T) {
		tmpDir := t.TempDir()
		teamPath := filepath.Join(tmpDir, "team.yml")
		yamlContent := "name: test-team\ngoal-mode: typo\n"
		if err := os.WriteFile(teamPath, []byte(yamlContent), 0644); err != nil {
			t.Fatal(err)
		}
		_, err := parseTeamYML(tmpDir, nil)
		if err == nil {
			t.Error("expected error for invalid goal-mode, got nil")
		}
	})
}

func TestParseTeamYML_AcceptanceMode(t *testing.T) {
	tmpDir := t.TempDir()
	teamPath := filepath.Join(tmpDir, "team.yml")
	yamlContent := "name: test-team\nacceptance:\n  mode: exploratory\n  criteria:\n    - id: ready\n      required: true\n      verify:\n        type: command_exit\n        command: 'true'\n"
	if err := os.WriteFile(teamPath, []byte(yamlContent), 0644); err != nil {
		t.Fatal(err)
	}
	cfg, err := parseTeamYML(tmpDir, nil)
	if err != nil {
		t.Fatalf("parseTeamYML returned error: %v", err)
	}
	if cfg.GoalMode != "exploratory" {
		t.Fatalf("cfg.GoalMode = %q, want exploratory", cfg.GoalMode)
	}
	if cfg.AcceptanceSpec == nil || cfg.AcceptanceSpec.Mode != "exploratory" || len(cfg.AcceptanceSpec.Criteria) != 1 {
		t.Fatalf("acceptance mode/criteria not parsed: %#v", cfg.AcceptanceSpec)
	}
}

func TestParseTeamYMLAcceptanceTranslation(t *testing.T) {
	t.Run("legacy string becomes command_exit acceptance", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "team.yml"), []byte("name: test\nacceptance: test -f report.md\n"), 0o644); err != nil {
			t.Fatal(err)
		}

		cfg, err := parseTeamYML(dir, nil)
		if err != nil {
			t.Fatalf("parseTeamYML: %v", err)
		}
		if cfg.Acceptance != "test -f report.md" {
			t.Fatalf("legacy acceptance command = %q", cfg.Acceptance)
		}
		if cfg.AcceptanceSpec == nil || len(cfg.AcceptanceSpec.Commands) != 1 || cfg.AcceptanceSpec.Commands[0] != cfg.Acceptance {
			t.Fatalf("legacy acceptance must retain its structured command translation: %#v", cfg.AcceptanceSpec)
		}
	})

	t.Run("structured typed assertions survive parsing", func(t *testing.T) {
		dir := t.TempDir()
		yaml := `name: test
acceptance:
  required-artifacts: [report.md]
  verifications:
    - type: json_assert
      path: summary.json
      assertions:
        - path: status
          equals: ok
`
		if err := os.WriteFile(filepath.Join(dir, "team.yml"), []byte(yaml), 0o644); err != nil {
			t.Fatal(err)
		}

		cfg, err := parseTeamYML(dir, nil)
		if err != nil {
			t.Fatalf("parseTeamYML: %v", err)
		}
		if cfg.AcceptanceSpec == nil {
			t.Fatal("expected structured acceptance spec")
		}
		if got := cfg.AcceptanceSpec.RequiredArtifacts; len(got) != 1 || got[0] != "report.md" {
			t.Fatalf("required artifacts = %#v", got)
		}
		if got := cfg.AcceptanceSpec.Verifications; len(got) != 1 || got[0].Type != agent.VerifyJSONAssert || got[0].Path != "summary.json" || len(got[0].Assertions) != 1 || got[0].Assertions[0].Path != "status" || got[0].Assertions[0].Equals != "ok" {
			t.Fatalf("typed verification = %#v", got)
		}
	})
}

func TestResolveTeamTemplateVars(t *testing.T) {
	t.Run("resolves vars from team.yaml and builtins", func(t *testing.T) {
		dir := t.TempDir()
		yaml := `name: my-templated-team
vars:
  project_name: test-proj
  env: staging
`
		if err := os.WriteFile(filepath.Join(dir, "team.yaml"), []byte(yaml), 0o644); err != nil {
			t.Fatal(err)
		}
		vars, err := ResolveTeamTemplateVars(dir, map[string]string{"env": "prod", "cli_extra": "foo"})
		if err != nil {
			t.Fatalf("ResolveTeamTemplateVars: %v", err)
		}
		if vars["project_name"] != "test-proj" {
			t.Fatalf("project_name = %q, want %q", vars["project_name"], "test-proj")
		}
		if vars["env"] != "prod" {
			t.Fatalf("env = %q, want CLI override %q", vars["env"], "prod")
		}
		if vars["cli_extra"] != "foo" {
			t.Fatalf("cli_extra = %q, want %q", vars["cli_extra"], "foo")
		}
		if vars["TEAM_NAME"] != "my-templated-team" {
			t.Fatalf("TEAM_NAME = %q, want %q", vars["TEAM_NAME"], "my-templated-team")
		}
	})

	t.Run("handles directory without team.yaml", func(t *testing.T) {
		dir := t.TempDir()
		vars, err := ResolveTeamTemplateVars(dir, map[string]string{"foo": "bar"})
		if err != nil {
			t.Fatalf("ResolveTeamTemplateVars: %v", err)
		}
		if vars["foo"] != "bar" {
			t.Fatalf("foo = %q", vars["foo"])
		}
		if vars["TEAM_NAME"] != filepath.Base(dir) {
			t.Fatalf("TEAM_NAME = %q, want %q", vars["TEAM_NAME"], filepath.Base(dir))
		}
	})
}

func TestValidateAgentFileWithVars(t *testing.T) {
	dir := t.TempDir()
	agentContent := `---
name: templated-worker
role: worker
tools: ask_user
---
Worker for {@ .project_name @} in {@ .env @}.
`
	agentPath := filepath.Join(dir, "worker.md")
	if err := os.WriteFile(agentPath, []byte(agentContent), 0o644); err != nil {
		t.Fatal(err)
	}

	// When vars is provided but missing required template keys, validation returns an error
	missingVars := map[string]string{"unrelated_var": "val"}
	if _, err := ValidateAgentFileWithVars(agentPath, missingVars); err == nil {
		t.Fatal("expected error with missing template vars")
	}

	// With matching vars, validation succeeds
	vars := map[string]string{
		"project_name": "hufu-app",
		"env":          "production",
	}
	def, err := ValidateAgentFileWithVars(agentPath, vars)
	if err != nil {
		t.Fatalf("ValidateAgentFileWithVars: %v", err)
	}
	if !strings.Contains(def.System, "hufu-app") {
		t.Fatalf("system prompt did not interpolate vars: %s", def.System)
	}

	// Test ValidateAgentContentWithVars
	if _, err := ValidateAgentContentWithVars([]byte(agentContent), agentPath, vars); err != nil {
		t.Fatalf("ValidateAgentContentWithVars: %v", err)
	}
}
