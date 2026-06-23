package team

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/anomalyco/hufu/internal/agent"
	"github.com/anomalyco/hufu/internal/config"
)

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
			yamlContent:  "name: test-team\nmax-steps: 100\nmodel: test-model\n",
			wantMaxSteps: 100,
			isAgent:      false,
			wantErr:      false,
		},
		{
			name:         "team config with max-steps: 0 (should be unset)",
			yamlContent:  "name: test-team\nmax-steps: 0\nmodel: test-model\n",
			wantMaxSteps: 0,
			isAgent:      false,
			wantErr:      false,
		},
		{
			name:         "team config without max-steps",
			yamlContent:  "name: test-team\nmodel: test-model\n",
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

func TestParseAgentGuardRules(t *testing.T) {
	tests := []struct {
		name        string
		yamlContent string
		wantGuard   []string
	}{
		{
			name:        "agent with guard rules",
			yamlContent: "---\nname: test-agent\nrole: worker\nguard:\n  - Never rm -rf\n  - No sudo commands\n---\n",
			wantGuard:   []string{"Never rm -rf", "No sudo commands"},
		},
		{
			name:        "agent without guard rules",
			yamlContent: "---\nname: test-agent\nrole: worker\n---\n",
			wantGuard:   nil,
		},
		{
			name:        "agent with empty guard list",
			yamlContent: "---\nname: test-agent\nrole: worker\nguard: []\n---\n",
			wantGuard:   []string{},
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

			if len(tt.wantGuard) == 0 && len(result.Guard) == 0 {
				return
			}
			if len(result.Guard) != len(tt.wantGuard) {
				t.Errorf("parseAgentFile Guard = %v, want %v", result.Guard, tt.wantGuard)
				return
			}
			for i, g := range result.Guard {
				if g != tt.wantGuard[i] {
					t.Errorf("Guard[%d] = %q, want %q", i, g, tt.wantGuard[i])
				}
			}
		})
	}
}

func TestMaxStepsDefaultValue(t *testing.T) {
	t.Run("agent max-steps 0 resolved to default", func(t *testing.T) {
		tmpDir := t.TempDir()
		agentPath := filepath.Join(tmpDir, "test-agent.md")
		yamlContent := "---\nname: test-agent\nrole: worker\nmodel: test-model\nmax-steps: 0\n---\n"
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
		yamlContent := "name: test-team\nmax-steps: 0\nmodel: test-model\n"
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
		yamlContent := "name: test-team\nmax-steps: 40\nmodel: test-model\n"
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
		yamlContent := "name: test-team\nmodel: test-model\n"
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
		yamlContent := "name: test-team\nmodel: ollama/qwen3:8b\n"
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

func TestLoadTeam_NoYAMLDirName(t *testing.T) {
	tmpDir := t.TempDir()
	teamDir := filepath.Join(tmpDir, "myteam")
	if err := os.MkdirAll(teamDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Provide one valid agent .md so LoadTeam has at least one agent.
	agentPath := filepath.Join(teamDir, "worker.md")
	agentContent := "---\nname: worker\nrole: worker\n---\nI am a worker."
	if err := os.WriteFile(agentPath, []byte(agentContent), 0o644); err != nil {
		t.Fatal(err)
	}

	session, err := LoadTeam(teamDir, nil, nil)
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
	agentPath := filepath.Join(teamDir, "worker.md")
	agentContent := "---\nname: worker\nrole: worker\n---\nI am a worker."
	if err := os.WriteFile(agentPath, []byte(agentContent), 0o644); err != nil {
		t.Fatal(err)
	}

	session, err := LoadTeam(teamDir, nil, nil)
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
	agentPath := filepath.Join(teamDir, "worker.md")
	agentContent := "---\nname: worker\nrole: worker\n---\nI am a worker."
	if err := os.WriteFile(agentPath, []byte(agentContent), 0o644); err != nil {
		t.Fatal(err)
	}

	session, err := LoadTeam(teamDir, nil, nil)
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
	agentPath := filepath.Join(teamDir, "worker.md")
	agentContent := "---\nname: worker\nrole: worker\n---\nI am a worker."
	if err := os.WriteFile(agentPath, []byte(agentContent), 0o644); err != nil {
		t.Fatal(err)
	}

	session, err := LoadTeam(teamDir, nil, nil)
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
