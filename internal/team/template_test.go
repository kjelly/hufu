package team

import (
	"os"
	"path/filepath"
	"testing"
)

func TestApplyTemplateNoVars(t *testing.T) {
	content := "---\nname: agent1\nrole: worker\n---\nHello world"
	result, err := applyTemplate(content, "test.md", nil)
	if err != nil {
		t.Fatalf("applyTemplate() error: %v", err)
	}
	if result != content {
		t.Errorf("applyTemplate() with nil vars should return content unchanged")
	}
}

func TestApplyTemplateEmptyVars(t *testing.T) {
	content := "---\nname: agent1\nrole: worker\n---\nHello world"
	result, err := applyTemplate(content, "test.md", map[string]string{})
	if err != nil {
		t.Fatalf("applyTemplate() error: %v", err)
	}
	if result != content {
		t.Errorf("applyTemplate() with empty vars should return content unchanged")
	}
}

func TestApplyTemplateNoTemplateDelimiters(t *testing.T) {
	vars := map[string]string{"model": "qwen3:8b"}
	content := "---\nname: agent1\nrole: worker\n---\nHello world"
	result, err := applyTemplate(content, "test.md", vars)
	if err != nil {
		t.Fatalf("applyTemplate() error: %v", err)
	}
	if result != content {
		t.Errorf("applyTemplate() with no {{ delimiters should return content unchanged")
	}
}

func TestApplyTemplateAgentFile(t *testing.T) {
	vars := map[string]string{
		"model":   "qwen3:8b",
		"project": "myapp",
	}
	content := "---\nname: developer\nmodel: {{.model}}\n---\nYou are working on project {{.project}}."
	expected := "---\nname: developer\nmodel: qwen3:8b\n---\nYou are working on project myapp."
	result, err := applyTemplate(content, "developer.md", vars)
	if err != nil {
		t.Fatalf("applyTemplate() error: %v", err)
	}
	if result != expected {
		t.Errorf("applyTemplate() = %q, want %q", result, expected)
	}
}

func TestApplyTemplateTeamConfig(t *testing.T) {
	vars := map[string]string{
		"model": "deepseek:14b",
		"url":   "http://remote:11434/v1",
	}
	content := "name: my-team\nmodel: {{.model}}\nprovider-url: {{.url}}\n"
	expected := "name: my-team\nmodel: deepseek:14b\nprovider-url: http://remote:11434/v1\n"
	result, err := applyTemplate(content, "team.yml", vars)
	if err != nil {
		t.Fatalf("applyTemplate() error: %v", err)
	}
	if result != expected {
		t.Errorf("applyTemplate() = %q, want %q", result, expected)
	}
}

func TestApplyTemplateMissingVar(t *testing.T) {
	vars := map[string]string{"model": "qwen3:8b"}
	content := "---\nname: agent1\nmodel: {{.model}}\ntools: {{.missing_var}}\n---\nBody"
	_, err := applyTemplate(content, "test.md", vars)
	if err == nil {
		t.Error("applyTemplate() should return error for missing variable")
	}
}

func TestApplyTemplateUnusedVars(t *testing.T) {
	vars := map[string]string{
		"model":   "qwen3:8b",
		"unused1": "value1",
		"unused2": "value2",
	}
	content := "model: {{.model}}\n"
	expected := "model: qwen3:8b\n"
	result, err := applyTemplate(content, "team.yml", vars)
	if err != nil {
		t.Fatalf("applyTemplate() error: %v", err)
	}
	if result != expected {
		t.Errorf("applyTemplate() = %q, want %q", result, expected)
	}
}

func TestApplyTemplateMultipleOccurrences(t *testing.T) {
	vars := map[string]string{"name": "agent-x"}
	content := "---\nname: {{.name}}\n---\nHello {{.name}}, welcome {{.name}}!"
	expected := "---\nname: agent-x\n---\nHello agent-x, welcome agent-x!"
	result, err := applyTemplate(content, "test.md", vars)
	if err != nil {
		t.Fatalf("applyTemplate() error: %v", err)
	}
	if result != expected {
		t.Errorf("applyTemplate() = %q, want %q", result, expected)
	}
}

func TestParseAgentFileWithTemplate(t *testing.T) {
	tmpDir := t.TempDir()
	content := "---\nname: {{.agent_name}}\nmodel: {{.model}}\n---\nYou are {{.agent_name}} working on {{.project}}."
	agentPath := filepath.Join(tmpDir, "dev.md")
	if err := os.WriteFile(agentPath, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	vars := map[string]string{
		"agent_name": "developer",
		"model":      "qwen3:8b",
		"project":    "myapp",
	}

	def, err := parseAgentFile(agentPath, vars)
	if err != nil {
		t.Fatalf("parseAgentFile() error: %v", err)
	}
	if def == nil {
		t.Fatal("parseAgentFile() returned nil")
	}
	if def.Name != "developer" {
		t.Errorf("Name = %q, want %q", def.Name, "developer")
	}
	if def.Generation.Model != "qwen3:8b" {
		t.Errorf("Model = %q, want %q", def.Generation.Model, "qwen3:8b")
	}
	expectedBody := "You are developer working on myapp."
	if def.System != expectedBody {
		t.Errorf("System = %q, want %q", def.System, expectedBody)
	}
}

func TestParseAgentFileWithTemplateMissingVar(t *testing.T) {
	tmpDir := t.TempDir()
	content := "---\nname: {{.agent_name}}\ntools: {{.missing}}\n---\nBody"
	agentPath := filepath.Join(tmpDir, "dev.md")
	if err := os.WriteFile(agentPath, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	vars := map[string]string{"agent_name": "developer"}
	_, err := parseAgentFile(agentPath, vars)
	if err == nil {
		t.Error("parseAgentFile() should return error for missing template variable")
	}
}

func TestParseAgentFileNoTemplateWithoutVars(t *testing.T) {
	tmpDir := t.TempDir()
	content := "---\nname: developer\nmodel: qwen3:8b\n---\nYou are a developer."
	agentPath := filepath.Join(tmpDir, "dev.md")
	if err := os.WriteFile(agentPath, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	def, err := parseAgentFile(agentPath, nil)
	if err != nil {
		t.Fatalf("parseAgentFile() error: %v", err)
	}
	if def == nil {
		t.Fatal("parseAgentFile() returned nil")
	}
	if def.Name != "developer" {
		t.Errorf("Name = %q, want %q", def.Name, "developer")
	}
}

func TestParseTeamYMLWithTemplate(t *testing.T) {
	tmpDir := t.TempDir()
	yamlContent := "name: {{.team_name}}\nmodel: {{.model}}\nmax-rounds: 5\n"
	teamPath := filepath.Join(tmpDir, "team.yml")
	if err := os.WriteFile(teamPath, []byte(yamlContent), 0644); err != nil {
		t.Fatal(err)
	}

	vars := map[string]string{
		"team_name": "my-team",
		"model":     "deepseek:14b",
	}

	cfg, err := parseTeamYML(tmpDir, vars)
	if err != nil {
		t.Fatalf("parseTeamYML() error: %v", err)
	}
	if cfg.Name != "my-team" {
		t.Errorf("Name = %q, want %q", cfg.Name, "my-team")
	}
	if cfg.Generation.Model != "deepseek:14b" {
		t.Errorf("Model = %q, want %q", cfg.Generation.Model, "deepseek:14b")
	}
	if cfg.MaxRounds != 5 {
		t.Errorf("MaxRounds = %d, want %d", cfg.MaxRounds, 5)
	}
}

func TestParseTeamYMLTemplateError(t *testing.T) {
	tmpDir := t.TempDir()
	yamlContent := "name: {{.team_name}}\nmodel: {{.undefined_var}}\n"
	teamPath := filepath.Join(tmpDir, "team.yml")
	if err := os.WriteFile(teamPath, []byte(yamlContent), 0644); err != nil {
		t.Fatal(err)
	}

	vars := map[string]string{"team_name": "my-team"}
	_, err := parseTeamYML(tmpDir, vars)
	if err == nil {
		t.Error("parseTeamYML() should return error for missing template variable")
	}
}
