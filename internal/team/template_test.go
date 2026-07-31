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
		t.Errorf("applyTemplate() with no {@ delimiters should return content unchanged")
	}
}

func TestApplyTemplateBasicSubstitution(t *testing.T) {
	vars := map[string]string{
		"model":   "qwen3:8b",
		"project": "myapp",
	}
	content := "---\nname: developer\nmodel: {@ .model @}\n---\nYou are working on project {@ .project @}."
	expected := "---\nname: developer\nmodel: qwen3:8b\n---\nYou are working on project myapp."
	result, err := applyTemplate(content, "developer.md", vars)
	if err != nil {
		t.Fatalf("applyTemplate() error: %v", err)
	}
	if result != expected {
		t.Errorf("applyTemplate() = %q, want %q", result, expected)
	}
}

func TestApplyTemplateSpacesVariants(t *testing.T) {
	vars := map[string]string{"name": "test"}
	tests := []struct {
		name     string
		content  string
		expected string
	}{
		{"no spaces", "{@.name@}", "test"},
		{"space after open", "{@ .name@}", "test"},
		{"space before close", "{@.name @}", "test"},
		{"spaces both sides", "{@ .name @}", "test"},
		{"multiple spaces", "{@  .name  @}", "test"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := applyTemplate(tt.content, "test.md", vars)
			if err != nil {
				t.Fatalf("applyTemplate() error: %v", err)
			}
			if result != tt.expected {
				t.Errorf("applyTemplate(%q) = %q, want %q", tt.content, result, tt.expected)
			}
		})
	}
}

func TestApplyTemplateDottedKeys(t *testing.T) {
	vars := map[string]string{
		"project.name": "myapp",
		"project.env":  "staging",
	}
	content := "name: {@ .project.name @}\nenvironment: {@ .project.env @}"
	expected := "name: myapp\nenvironment: staging"
	result, err := applyTemplate(content, "config.yml", vars)
	if err != nil {
		t.Fatalf("applyTemplate() error: %v", err)
	}
	if result != expected {
		t.Errorf("applyTemplate() = %q, want %q", result, expected)
	}
}

func TestApplyTemplateMissingVar(t *testing.T) {
	vars := map[string]string{"model": "qwen3:8b"}
	content := "model: {@ .model @}\ntools: {@ .missing_var @}"
	_, err := applyTemplate(content, "test.md", vars)
	if err == nil {
		t.Error("applyTemplate() should return error for missing variable")
	}
}

func TestApplyTemplateGitHubActionsUnchanged(t *testing.T) {
	vars := map[string]string{"model": "qwen3:8b"}
	content := "deploy: ${{ secrets.GITHUB_TOKEN }}\nmodel: {@ .model @}"
	expected := "deploy: ${{ secrets.GITHUB_TOKEN }}\nmodel: qwen3:8b"
	result, err := applyTemplate(content, "ci.yml", vars)
	if err != nil {
		t.Fatalf("applyTemplate() error: %v", err)
	}
	if result != expected {
		t.Errorf("applyTemplate() = %q, want %q", result, expected)
	}
}

func TestApplyTemplateHugoShortcodesUnchanged(t *testing.T) {
	vars := map[string]string{"title": "My Page"}
	content := "---\ntitle: {@ .title @}\n---\n{{< highlight go >}}code{{< /highlight >}}"
	expected := "---\ntitle: My Page\n---\n{{< highlight go >}}code{{< /highlight >}}"
	result, err := applyTemplate(content, "page.md", vars)
	if err != nil {
		t.Fatalf("applyTemplate() error: %v", err)
	}
	if result != expected {
		t.Errorf("applyTemplate() = %q, want %q", result, expected)
	}
}

func TestApplyTemplateGoTemplateLogicUnchanged(t *testing.T) {
	vars := map[string]string{"name": "test"}
	content := "---\nname: {@ .name @}\n---\n{{ if .verbose }}detailed{{ end }}"
	expected := "---\nname: test\n---\n{{ if .verbose }}detailed{{ end }}"
	result, err := applyTemplate(content, "test.md", vars)
	if err != nil {
		t.Fatalf("applyTemplate() error: %v", err)
	}
	if result != expected {
		t.Errorf("applyTemplate() = %q, want %q", result, expected)
	}
}

func TestApplyTemplateConditionals(t *testing.T) {
	vars := map[string]string{
		"env":     "production",
		"verbose": "true",
	}
	content := "env: {@ .env @}\n{@ if eq .env \"production\" @}critical{@ end @}"
	expected := "env: production\ncritical"
	result, err := applyTemplate(content, "config.yml", vars)
	if err != nil {
		t.Fatalf("applyTemplate() error: %v", err)
	}
	if result != expected {
		t.Errorf("applyTemplate() = %q, want %q", result, expected)
	}
}

func TestApplyTemplateMultipleOccurrences(t *testing.T) {
	vars := map[string]string{"name": "agent-x"}
	content := "---\nname: {@ .name @}\n---\nHello {@ .name @}, welcome {@ .name @}!"
	expected := "---\nname: agent-x\n---\nHello agent-x, welcome agent-x!"
	result, err := applyTemplate(content, "test.md", vars)
	if err != nil {
		t.Fatalf("applyTemplate() error: %v", err)
	}
	if result != expected {
		t.Errorf("applyTemplate() = %q, want %q", result, expected)
	}
}

func TestApplyTemplateRealWorldSidecarModel(t *testing.T) {
	vars := map[string]string{
		"big_model":   "ollama/glm-5.1:cloud",
		"small_model": "ollama/gemma4:31b-cloud",
	}
	content := "sidecar-model: {@ .small_model @}\nmodel: {@ .big_model @}"
	expected := "sidecar-model: ollama/gemma4:31b-cloud\nmodel: ollama/glm-5.1:cloud"
	result, err := applyTemplate(content, "team.yml", vars)
	if err != nil {
		t.Fatalf("applyTemplate() error: %v", err)
	}
	if result != expected {
		t.Errorf("applyTemplate() = %q, want %q", result, expected)
	}
}

func TestParseAgentFileWithTemplate(t *testing.T) {
	tmpDir := t.TempDir()
	content := "---\nname: {@ .agent_name @}\nmodel: {@ .model @}\n---\nYou are {@ .agent_name @} working on {@ .project @}."
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
	content := "---\nname: {@ .agent_name @}\ntools: {@ .missing @}\n---\nBody"
	agentPath := filepath.Join(tmpDir, "dev.md")
	if err := os.WriteFile(agentPath, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	vars := map[string]string{"agent_name": "developer"}
	def, err := parseAgentFile(agentPath, vars)
	if err == nil {
		t.Error("parseAgentFile() should return error for missing template variable")
	}
	if def != nil {
		t.Error("parseAgentFile() should return nil def for missing template variable")
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

func TestParseAgentFileGitHubActionsUnchanged(t *testing.T) {
	tmpDir := t.TempDir()
	content := "---\nname: developer\n---\nDeploy: ${{ secrets.GITHUB_TOKEN }}\nModel: {@ .model @}"
	agentPath := filepath.Join(tmpDir, "dev.md")
	if err := os.WriteFile(agentPath, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	vars := map[string]string{"model": "qwen3:8b"}
	def, err := parseAgentFile(agentPath, vars)
	if err != nil {
		t.Fatalf("parseAgentFile() error: %v", err)
	}
	if def == nil {
		t.Fatal("parseAgentFile() returned nil")
	}
	expectedBody := "Deploy: ${{ secrets.GITHUB_TOKEN }}\nModel: qwen3:8b"
	if def.System != expectedBody {
		t.Errorf("System = %q, want %q", def.System, expectedBody)
	}
}

func TestParseTeamYMLWithTemplate(t *testing.T) {
	tmpDir := t.TempDir()
	yamlContent := "name: {@ .team_name @}\nacceptance: 'true'\nmodel: {@ .model @}\nmax-rounds: 5\n"
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

func TestParseTeamYMLTemplateMissingVar(t *testing.T) {
	tmpDir := t.TempDir()
	yamlContent := "name: {@ .team_name @}\nmodel: {@ .undefined_var @}\n"
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

func TestExpandToNestedMapConflictScalarThenNested(t *testing.T) {
	// "project" as scalar and "project.name" as nested should conflict
	vars := map[string]string{
		"project":      "myapp",
		"project.name": "myapp-nested",
	}
	_, err := expandToNestedMap(vars)
	if err == nil {
		t.Error("expandToNestedMap() should return error for conflicting scalar/nested keys")
	}
}

func TestExpandToNestedMapConflictNestedThenScalar(t *testing.T) {
	// "project.name" as nested and "project" as scalar should conflict
	vars := map[string]string{
		"project.name": "myapp-nested",
		"project":      "myapp",
	}
	_, err := expandToNestedMap(vars)
	if err == nil {
		t.Error("expandToNestedMap() should return error for conflicting nested/scalar keys")
	}
}

func TestExpandToNestedMapNoConflict(t *testing.T) {
	vars := map[string]string{
		"project.name": "myapp",
		"project.env":  "staging",
	}
	result, err := expandToNestedMap(vars)
	if err != nil {
		t.Fatalf("expandToNestedMap() unexpected error: %v", err)
	}
	projectMap, ok := result["project"].(map[string]interface{})
	if !ok {
		t.Fatal("result[\"project\"] should be a map")
	}
	if projectMap["name"] != "myapp" {
		t.Errorf("project.name = %q, want %q", projectMap["name"], "myapp")
	}
	if projectMap["env"] != "staging" {
		t.Errorf("project.env = %q, want %q", projectMap["env"], "staging")
	}
}

func TestApplyTemplateConflictingVars(t *testing.T) {
	vars := map[string]string{
		"project":      "myapp",
		"project.name": "myapp-nested",
	}
	content := "name: {@ .project.name @}"
	_, err := applyTemplate(content, "test.md", vars)
	if err == nil {
		t.Error("applyTemplate() should return error for conflicting variable keys")
	}
}
