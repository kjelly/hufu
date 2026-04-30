package config

import (
	"os"
	"path/filepath"
	"testing"
)

// TestLoadConfig tests the LoadConfig function
func TestLoadConfig(t *testing.T) {
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
	os.Chdir(tmpDir)
	defer func() {
		os.Chdir(originalDir)
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
				os.Chdir(tmpDir)
				defer func() {
					os.Chdir(originalDir)
				}()
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
	// Change to a directory where no config file exists
	tmpDir := t.TempDir()
	originalDir, _ := os.Getwd()
	os.Chdir(tmpDir)
	defer func() {
		os.Chdir(originalDir)
	}()

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
	// Create a temporary directory for testing
	tmpDir := t.TempDir()

	// Change to the temp directory
	originalDir, _ := os.Getwd()
	os.Chdir(tmpDir)
	defer func() {
		os.Chdir(originalDir)
	}()

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
