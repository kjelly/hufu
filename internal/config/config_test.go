package config

import (
	"os"
	"path/filepath"
	"testing"
)

// TestLoadConfig tests the LoadConfig function
func TestLoadConfig(t *testing.T) {
	// Temporarily set HOME to a temp dir so the home config file is not found.
	t.Setenv("HOME", t.TempDir())

	// Test with no config files - should return default config
	cfg := LoadConfig()
	if cfg == nil {
		t.Fatal("LoadConfig() returned nil")
	}

	// ProviderURL should be empty when no config files exist
	if cfg.ProviderURL != "" {
		t.Errorf("LoadConfig() ProviderURL = %q, want empty string", cfg.ProviderURL)
	}
}

// TestLoadConfigWithProviderURL tests LoadConfig with provider-url set
func TestLoadConfigProfiles(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // isolate from a real ~/.config/hufu
	tmpDir := t.TempDir()
	configContent := `profiles:
  batch:
    unattended: "true"
    max-duration: "600"
  safe:
    no-net: "true"
`
	if err := os.WriteFile(filepath.Join(tmpDir, "hufu.yaml"), []byte(configContent), 0644); err != nil {
		t.Fatalf("Failed to write config file: %v", err)
	}
	originalDir, _ := os.Getwd()
	if err := os.Chdir(tmpDir); err != nil {
		t.Fatalf("failed to chdir: %v", err)
	}
	defer func() { _ = os.Chdir(originalDir) }()

	cfg := LoadConfig()
	if len(cfg.Profiles) != 2 {
		t.Fatalf("expected 2 profiles, got %d (%v)", len(cfg.Profiles), cfg.Profiles)
	}
	if cfg.Profiles["batch"]["unattended"] != "true" {
		t.Errorf("batch.unattended = %q, want true", cfg.Profiles["batch"]["unattended"])
	}
	if cfg.Profiles["batch"]["max-duration"] != "600" {
		t.Errorf("batch.max-duration = %q, want 600", cfg.Profiles["batch"]["max-duration"])
	}
	if cfg.Profiles["safe"]["no-net"] != "true" {
		t.Errorf("safe.no-net = %q, want true", cfg.Profiles["safe"]["no-net"])
	}
}

func TestLoadConfigWithProviderURL(t *testing.T) {
	// Create a temporary directory for testing
	tmpDir := t.TempDir()

	// Create a config file with provider-url
	configContent := `provider-url: http://custom-provider:11434/v1
`
	configPath := filepath.Join(tmpDir, "hufu.yaml")
	if err := os.WriteFile(configPath, []byte(configContent), 0644); err != nil {
		t.Fatalf("Failed to write config file: %v", err)
	}

	// Change to the temp directory
	originalDir, _ := os.Getwd()
	if err := os.Chdir(tmpDir); err != nil {
		t.Fatalf("failed to chdir: %v", err)
	}
	defer func() {
		_ = os.Chdir(originalDir)
	}()

	cfg := LoadConfig()
	if cfg == nil {
		t.Fatal("LoadConfig() returned nil")
	}

	if cfg.ProviderURL != "http://custom-provider:11434/v1" {
		t.Errorf("LoadConfig() ProviderURL = %q, want %q", cfg.ProviderURL, "http://custom-provider:11434/v1")
	}
}

// TestConfigMergeFromFile tests the mergeFromFile method
func TestConfigMergeFromFile(t *testing.T) {
	tests := []struct {
		name       string
		configYAML string
		wantURL    string
	}{
		{
			name:       "merge valid provider-url",
			configYAML: "provider-url: http://test:11434/v1",
			wantURL:    "http://test:11434/v1",
		},
		{
			name:       "empty config does not change provider-url",
			configYAML: "",
			wantURL:    "http://original:11434/v1",
		},
		{
			name:       "invalid yaml is ignored",
			configYAML: "invalid: yaml: content: [",
			wantURL:    "http://original:11434/v1",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tmpDir := t.TempDir()
			configPath := filepath.Join(tmpDir, "hufu.yaml")
			if err := os.WriteFile(configPath, []byte(tt.configYAML), 0644); err != nil {
				t.Fatalf("Failed to write config file: %v", err)
			}

			cfg := &Config{ProviderURL: "http://original:11434/v1"}
			cfg.mergeFromFile(configPath)

			if cfg.ProviderURL != tt.wantURL {
				t.Errorf("mergeFromFile() ProviderURL = %q, want %q", cfg.ProviderURL, tt.wantURL)
			}
		})
	}
}

// TestResolveProviderURL tests the ResolveProviderURL function
func TestResolveProviderURL(t *testing.T) {
	tests := []struct {
		name               string
		cliFlag            string
		teamCfgProviderURL string
		agentProviderURL   string
		configProviderURL  string
		want               string
	}{
		{
			name:               "CLI flag takes precedence",
			cliFlag:            "http://cli:11434/v1",
			teamCfgProviderURL: "http://team:11434/v1",
			agentProviderURL:   "http://agent:11434/v1",
			configProviderURL:  "http://config:11434/v1",
			want:               "http://cli:11434/v1",
		},
		{
			name:               "team config takes precedence over agent",
			cliFlag:            "",
			teamCfgProviderURL: "http://team:11434/v1",
			agentProviderURL:   "http://agent:11434/v1",
			configProviderURL:  "http://config:11434/v1",
			want:               "http://team:11434/v1",
		},
		{
			name:               "agent provider URL takes precedence over config",
			cliFlag:            "",
			teamCfgProviderURL: "",
			agentProviderURL:   "http://agent:11434/v1",
			configProviderURL:  "http://config:11434/v1",
			want:               "http://agent:11434/v1",
		},
		{
			name:               "config file takes precedence over default",
			cliFlag:            "",
			teamCfgProviderURL: "",
			agentProviderURL:   "",
			configProviderURL:  "http://config:11434/v1",
			want:               "http://config:11434/v1",
		},
		{
			name:               "default provider URL when all empty",
			cliFlag:            "",
			teamCfgProviderURL: "",
			agentProviderURL:   "",
			configProviderURL:  "",
			want:               DefaultProviderURL,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Temporarily rename system config file to simulate no config files
			homeDir, _ := os.UserHomeDir()
			systemConfigPath := filepath.Join(homeDir, ".config", "hufu", "hufu.yaml")
			backupPath := systemConfigPath + ".backup"

			// Move system config file if it exists
			if _, err := os.Stat(systemConfigPath); err == nil {
				if err := os.Rename(systemConfigPath, backupPath); err != nil {
					t.Fatalf("failed to backup system config: %v", err)
				}
				defer func() { _ = os.Rename(backupPath, systemConfigPath) }()
			}

			// Create a temporary directory for testing
			tmpDir := t.TempDir()

			// Create a config file if needed
			if tt.configProviderURL != "" {
				configContent := "provider-url: " + tt.configProviderURL + "\n"
				configPath := filepath.Join(tmpDir, "hufu.yaml")
				if err := os.WriteFile(configPath, []byte(configContent), 0644); err != nil {
					t.Fatalf("Failed to write config file: %v", err)
				}

				// Change to the temp directory
				originalDir, _ := os.Getwd()
				if err := os.Chdir(tmpDir); err != nil {
					t.Fatalf("failed to chdir: %v", err)
				}
				defer func() {
					_ = os.Chdir(originalDir)
				}()
			} else {
				// When no config file is provided, override HOME to prevent
				// the real home config (~/.config/hufu/hufu.yaml) from being read.
				t.Setenv("HOME", t.TempDir())
			}

			got := ResolveProviderURL(tt.cliFlag, tt.teamCfgProviderURL, tt.agentProviderURL)
			if got != tt.want {
				t.Errorf("ResolveProviderURL(%q, %q, %q) = %q, want %q",
					tt.cliFlag, tt.teamCfgProviderURL, tt.agentProviderURL, got, tt.want)
			}
		})
	}
}

// TestDefaultProviderURL tests that DefaultProviderURL is correctly defined
func TestDefaultProviderURL(t *testing.T) {
	expected := "http://localhost:11434/v1"
	if DefaultProviderURL != expected {
		t.Errorf("DefaultProviderURL = %q, want %q", DefaultProviderURL, expected)
	}
}

// TestConfigFields tests that Config has all expected fields
func TestConfigFields(t *testing.T) {
	cfg := &Config{
		ProviderURL: "http://test:11434/v1",
	}

	if cfg.ProviderURL != "http://test:11434/v1" {
		t.Errorf("ProviderURL = %q, want %q", cfg.ProviderURL, "http://test:11434/v1")
	}
}

// TestLoadConfigNonExistentFile tests LoadConfig with non-existent file
func TestLoadConfigNonExistentFile(t *testing.T) {
	// Temporarily rename system config file to simulate no config files
	homeDir, _ := os.UserHomeDir()
	systemConfigPath := filepath.Join(homeDir, ".config", "hufu", "hufu.yaml")
	backupPath := systemConfigPath + ".backup"

	// Move system config file if it exists
	if _, err := os.Stat(systemConfigPath); err == nil {
		if err := os.Rename(systemConfigPath, backupPath); err != nil {
			t.Fatalf("failed to backup system config: %v", err)
		}
		defer func() { _ = os.Rename(backupPath, systemConfigPath) }()
	}

	// Change to a directory where no config file exists
	tmpDir := t.TempDir()
	originalDir, _ := os.Getwd()
	if err := os.Chdir(tmpDir); err != nil {
		t.Fatalf("failed to chdir: %v", err)
	}
	defer func() {
		_ = os.Chdir(originalDir)
	}()

	// Also override HOME to prevent the real home config from being read.
	t.Setenv("HOME", t.TempDir())

	cfg := LoadConfig()
	if cfg == nil {
		t.Fatal("LoadConfig() returned nil")
	}

	// Should return default config with empty ProviderURL
	if cfg.ProviderURL != "" {
		t.Errorf("LoadConfig() ProviderURL = %q, want empty string", cfg.ProviderURL)
	}
}

// TestResolveProviderURLWithEmptyStrings tests ResolveProviderURL with all empty strings
func TestResolveProviderURLWithEmptyStrings(t *testing.T) {
	// Temporarily rename system config file to simulate no config files
	homeDir, _ := os.UserHomeDir()
	systemConfigPath := filepath.Join(homeDir, ".config", "hufu", "hufu.yaml")
	backupPath := systemConfigPath + ".backup"

	// Move system config file if it exists
	if _, err := os.Stat(systemConfigPath); err == nil {
		if err := os.Rename(systemConfigPath, backupPath); err != nil {
			t.Fatalf("failed to backup system config: %v", err)
		}
		defer func() { _ = os.Rename(backupPath, systemConfigPath) }()
	}

	// Create a temporary directory for testing
	tmpDir := t.TempDir()

	// Change to the temp directory
	originalDir, _ := os.Getwd()
	if err := os.Chdir(tmpDir); err != nil {
		t.Fatalf("failed to chdir: %v", err)
	}
	defer func() {
		_ = os.Chdir(originalDir)
	}()

	// Override HOME to prevent the real home config from being read.
	t.Setenv("HOME", t.TempDir())

	got := ResolveProviderURL("", "", "")
	if got != DefaultProviderURL {
		t.Errorf("ResolveProviderURL(\"\", \"\", \"\") = %q, want %q", got, DefaultProviderURL)
	}
}

// TestResolveProviderURLWithOnlyCLIFlag tests ResolveProviderURL with only CLI flag
func TestResolveProviderURLWithOnlyCLIFlag(t *testing.T) {
	got := ResolveProviderURL("http://cli:11434/v1", "", "")
	if got != "http://cli:11434/v1" {
		t.Errorf("ResolveProviderURL(\"http://cli:11434/v1\", \"\", \"\") = %q, want %q", got, "http://cli:11434/v1")
	}
}

// TestResolveProviderURLWithOnlyTeamConfig tests ResolveProviderURL with only team config
func TestResolveProviderURLWithOnlyTeamConfig(t *testing.T) {
	got := ResolveProviderURL("", "http://team:11434/v1", "")
	if got != "http://team:11434/v1" {
		t.Errorf("ResolveProviderURL(\"\", \"http://team:11434/v1\", \"\") = %q, want %q", got, "http://team:11434/v1")
	}
}

// TestResolveProviderURLWithOnlyAgentConfig tests ResolveProviderURL with only agent config
func TestResolveProviderURLWithOnlyAgentConfig(t *testing.T) {
	got := ResolveProviderURL("", "", "http://agent:11434/v1")
	if got != "http://agent:11434/v1" {
		t.Errorf("ResolveProviderURL(\"\", \"\", \"http://agent:11434/v1\") = %q, want %q", got, "http://agent:11434/v1")
	}
}

// TestConfigMergeFromFileNonExistent tests mergeFromFile with non-existent file
func TestConfigMergeFromFileNonExistent(t *testing.T) {
	cfg := &Config{ProviderURL: "http://original:11434/v1"}
	cfg.mergeFromFile("/nonexistent/path/hufu.yaml")

	// Should not change the ProviderURL
	if cfg.ProviderURL != "http://original:11434/v1" {
		t.Errorf("mergeFromFile() changed ProviderURL to %q, want %q", cfg.ProviderURL, "http://original:11434/v1")
	}
}

// TestConfigMergeFromFileInvalidYAML tests mergeFromFile with invalid YAML
func TestConfigMergeFromFileInvalidYAML(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "hufu.yaml")
	if err := os.WriteFile(configPath, []byte("invalid: yaml: content: ["), 0644); err != nil {
		t.Fatalf("Failed to write config file: %v", err)
	}

	cfg := &Config{ProviderURL: "http://original:11434/v1"}
	cfg.mergeFromFile(configPath)

	// Should not change the ProviderURL
	if cfg.ProviderURL != "http://original:11434/v1" {
		t.Errorf("mergeFromFile() changed ProviderURL to %q, want %q", cfg.ProviderURL, "http://original:11434/v1")
	}
}

// TestConfigMergeFromFileEmptyYAML tests mergeFromFile with empty YAML
func TestConfigMergeFromFileEmptyYAML(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "hufu.yaml")
	if err := os.WriteFile(configPath, []byte(""), 0644); err != nil {
		t.Fatalf("Failed to write config file: %v", err)
	}

	cfg := &Config{ProviderURL: "http://original:11434/v1"}
	cfg.mergeFromFile(configPath)

	// Should not change the ProviderURL
	if cfg.ProviderURL != "http://original:11434/v1" {
		t.Errorf("mergeFromFile() changed ProviderURL to %q, want %q", cfg.ProviderURL, "http://original:11434/v1")
	}
}

// TestConfigMergeFromFileWhitespaceYAML tests mergeFromFile with whitespace-only YAML
func TestConfigMergeFromFileWhitespaceYAML(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "hufu.yaml")
	if err := os.WriteFile(configPath, []byte("   \n  \n  "), 0644); err != nil {
		t.Fatalf("Failed to write config file: %v", err)
	}

	cfg := &Config{ProviderURL: "http://original:11434/v1"}
	cfg.mergeFromFile(configPath)

	// Should not change the ProviderURL
	if cfg.ProviderURL != "http://original:11434/v1" {
		t.Errorf("mergeFromFile() changed ProviderURL to %q, want %q", cfg.ProviderURL, "http://original:11434/v1")
	}
}

func TestResolveModelList(t *testing.T) {
	tests := []struct {
		name      string
		cfgList   []ModelEntry
		teamList  []ModelEntry
		wantLen   int
		wantFirst string
	}{
		{
			name:      "team list takes precedence",
			cfgList:   []ModelEntry{{ID: "cfg-model", Details: "config"}},
			teamList:  []ModelEntry{{ID: "team-model", Details: "team"}},
			wantLen:   1,
			wantFirst: "team-model",
		},
		{
			name:      "cfg list used when team list empty",
			cfgList:   []ModelEntry{{ID: "cfg-model", Details: "config"}},
			teamList:  nil,
			wantLen:   1,
			wantFirst: "cfg-model",
		},
		{
			name:      "both empty returns nil",
			cfgList:   nil,
			teamList:  nil,
			wantLen:   0,
			wantFirst: "",
		},
		{
			name:      "empty team list falls back to cfg",
			cfgList:   []ModelEntry{{ID: "a", Details: "A"}, {ID: "b", Details: "B"}},
			teamList:  []ModelEntry{},
			wantLen:   2,
			wantFirst: "a",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &Config{ModelList: tt.cfgList}
			got := cfg.ResolveModelList(tt.teamList)
			if len(got) != tt.wantLen {
				t.Errorf("ResolveModelList() returned %d entries, want %d", len(got), tt.wantLen)
			}
			if tt.wantLen > 0 && got[0].ID != tt.wantFirst {
				t.Errorf("ResolveModelList() first entry ID = %q, want %q", got[0].ID, tt.wantFirst)
			}
		})
	}
}

func TestModelListMergeFromFile(t *testing.T) {
	tmpDir := t.TempDir()
	configContent := `provider-url: http://test:11434/v1
model-list:
  - id: ollama/model-a
    details: Model A details
  - id: ollama/model-b
    details: Model B details
`
	configPath := filepath.Join(tmpDir, "hufu.yaml")
	if err := os.WriteFile(configPath, []byte(configContent), 0644); err != nil {
		t.Fatalf("Failed to write config file: %v", err)
	}

	cfg := &Config{}
	cfg.mergeFromFile(configPath)

	if cfg.ProviderURL != "http://test:11434/v1" {
		t.Errorf("ProviderURL = %q, want %q", cfg.ProviderURL, "http://test:11434/v1")
	}
	if len(cfg.ModelList) != 2 {
		t.Fatalf("ModelList has %d entries, want 2", len(cfg.ModelList))
	}
	if cfg.ModelList[0].ID != "ollama/model-a" {
		t.Errorf("ModelList[0].ID = %q, want %q", cfg.ModelList[0].ID, "ollama/model-a")
	}
	if cfg.ModelList[1].ID != "ollama/model-b" {
		t.Errorf("ModelList[1].ID = %q, want %q", cfg.ModelList[1].ID, "ollama/model-b")
	}
}

func TestResolveModel(t *testing.T) {
	cases := []struct {
		name      string
		cfgModel  string
		teamModel string
		want      string
	}{
		{name: "team model takes precedence", cfgModel: "ollama/global", teamModel: "ollama/team", want: "ollama/team"},
		{name: "falls back to cfg model when team empty", cfgModel: "ollama/global", teamModel: "", want: "ollama/global"},
		{name: "both empty returns empty", cfgModel: "", teamModel: "", want: ""},
		{name: "only team model set", cfgModel: "", teamModel: "ollama/team", want: "ollama/team"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &Config{Model: tc.cfgModel}
			got := cfg.ResolveModel(tc.teamModel)
			if got != tc.want {
				t.Errorf("ResolveModel(%q) with cfg.Model=%q = %q, want %q", tc.teamModel, tc.cfgModel, got, tc.want)
			}
		})
	}
}

func TestModelMergeFromFile(t *testing.T) {
	tmpDir := t.TempDir()
	content := "model: ollama/qwen3:8b\n"
	configPath := filepath.Join(tmpDir, "hufu.yaml")
	if err := os.WriteFile(configPath, []byte(content), 0644); err != nil {
		t.Fatalf("failed to write config: %v", err)
	}
	cfg := &Config{}
	cfg.mergeFromFile(configPath)
	if cfg.Model != "ollama/qwen3:8b" {
		t.Errorf("Model = %q, want %q", cfg.Model, "ollama/qwen3:8b")
	}
}

func TestGetVarsEmpty(t *testing.T) {
	cfg := &Config{}
	vars := cfg.GetVars()
	if vars != nil {
		t.Errorf("GetVars() = %v, want nil for empty config", vars)
	}
}

func TestGetVarsFlat(t *testing.T) {
	tmpDir := t.TempDir()
	content := `provider-url: http://test:11434/v1
vars:
  model: qwen3:8b
  temperature: "0.7"
  debug: true
`
	configPath := filepath.Join(tmpDir, "hufu.yaml")
	if err := os.WriteFile(configPath, []byte(content), 0644); err != nil {
		t.Fatalf("failed to write config: %v", err)
	}
	cfg := &Config{}
	cfg.mergeFromFile(configPath)
	vars := cfg.GetVars()
	if vars == nil {
		t.Fatal("GetVars() returned nil")
	}
	if vars["model"] != "qwen3:8b" {
		t.Errorf("vars[model] = %q, want %q", vars["model"], "qwen3:8b")
	}
	if vars["temperature"] != "0.7" {
		t.Errorf("vars[temperature] = %q, want %q", vars["temperature"], "0.7")
	}
	if vars["debug"] != "true" {
		t.Errorf("vars[debug] = %q, want %q", vars["debug"], "true")
	}
}

func TestGetVarsNested(t *testing.T) {
	tmpDir := t.TempDir()
	content := `vars:
  project:
    name: myapp
    env: staging
`
	configPath := filepath.Join(tmpDir, "hufu.yaml")
	if err := os.WriteFile(configPath, []byte(content), 0644); err != nil {
		t.Fatalf("failed to write config: %v", err)
	}
	cfg := &Config{}
	cfg.mergeFromFile(configPath)
	vars := cfg.GetVars()
	if vars == nil {
		t.Fatal("GetVars() returned nil")
	}
	if vars["project.name"] != "myapp" {
		t.Errorf("vars[project.name] = %q, want %q", vars["project.name"], "myapp")
	}
	if vars["project.env"] != "staging" {
		t.Errorf("vars[project.env] = %q, want %q", vars["project.env"], "staging")
	}
}

func TestMergeFromFileVarsMerge(t *testing.T) {
	tmpDir := t.TempDir()
	baseContent := `vars:
  model: qwen3:8b
  region: us-east
`
	overrideContent := `vars:
  model: deepseek:14b
  timeout: "300"
`
	basePath := filepath.Join(tmpDir, "base.yaml")
	overridePath := filepath.Join(tmpDir, "override.yaml")
	if err := os.WriteFile(basePath, []byte(baseContent), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(overridePath, []byte(overrideContent), 0644); err != nil {
		t.Fatal(err)
	}

	cfg := &Config{}
	cfg.mergeFromFile(basePath)
	cfg.mergeFromFile(overridePath)
	vars := cfg.GetVars()
	if vars["model"] != "deepseek:14b" {
		t.Errorf("vars[model] = %q, want %q (overridden)", vars["model"], "deepseek:14b")
	}
	if vars["region"] != "us-east" {
		t.Errorf("vars[region] = %q, want %q (preserved)", vars["region"], "us-east")
	}
	if vars["timeout"] != "300" {
		t.Errorf("vars[timeout] = %q, want %q", vars["timeout"], "300")
	}
}
