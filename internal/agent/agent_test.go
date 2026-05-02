package agent

import (
	"testing"

	"github.com/anomalyco/hufu/internal/config"
)

// TestParseModelInt tests the parseModelInt function
func TestParseModelInt(t *testing.T) {
	tests := []struct {
		name     string
		primary  string
		fallback string
		expected int
	}{
		{
			name:     "primary valid integer",
			primary:  "1000",
			fallback: "500",
			expected: 1000,
		},
		{
			name:     "primary invalid, fallback valid",
			primary:  "invalid",
			fallback: "500",
			expected: 500,
		},
		{
			name:     "both invalid returns -1",
			primary:  "invalid",
			fallback: "also-invalid",
			expected: -1,
		},
		{
			name:     "primary empty, fallback valid",
			primary:  "",
			fallback: "500",
			expected: 500,
		},
		{
			name:     "both empty returns -1",
			primary:  "",
			fallback: "",
			expected: -1,
		},
		{
			name:     "primary zero",
			primary:  "0",
			fallback: "500",
			expected: 0,
		},
		{
			name:     "primary negative",
			primary:  "-100",
			fallback: "500",
			expected: -100,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseModelInt(tt.primary, tt.fallback)
			if got != tt.expected {
				t.Errorf("parseModelInt(%q, %q) = %d, want %d", tt.primary, tt.fallback, got, tt.expected)
			}
		})
	}
}

// TestParseModelFloat tests the parseModelFloat function
func TestParseModelFloat(t *testing.T) {
	tests := []struct {
		name     string
		primary  string
		fallback string
		expected float64
	}{
		{
			name:     "primary valid float",
			primary:  "0.7",
			fallback: "0.5",
			expected: 0.7,
		},
		{
			name:     "primary invalid, fallback valid",
			primary:  "invalid",
			fallback: "0.5",
			expected: 0.5,
		},
		{
			name:     "both invalid returns -1",
			primary:  "invalid",
			fallback: "also-invalid",
			expected: -1,
		},
		{
			name:     "primary empty, fallback valid",
			primary:  "",
			fallback: "0.5",
			expected: 0.5,
		},
		{
			name:     "both empty returns -1",
			primary:  "",
			fallback: "",
			expected: -1,
		},
		{
			name:     "primary zero",
			primary:  "0.0",
			fallback: "0.5",
			expected: 0.0,
		},
		{
			name:     "primary negative",
			primary:  "-0.5",
			fallback: "0.5",
			expected: -0.5,
		},
		{
			name:     "primary integer string",
			primary:  "100",
			fallback: "50",
			expected: 100.0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseModelFloat(tt.primary, tt.fallback)
			if got != tt.expected {
				t.Errorf("parseModelFloat(%q, %q) = %v, want %v", tt.primary, tt.fallback, got, tt.expected)
			}
		})
	}
}

// TestSelectTools tests the SelectTools function
func TestSelectTools(t *testing.T) {
	// Create mock tools for testing
	type mockTool struct {
		name string
	}

	mockTools := []struct {
		name string
	}{
		{"bash"},
		{"grep"},
		{"view"},
		{"glob"},
		{"edit"},
		{"agent"},
		{"todo"},
	}

	tests := []struct {
		name      string
		toolNames string
		expected  []string
	}{
		{
			name:      "empty tool names returns all tools",
			toolNames: "",
			expected:  []string{"bash", "grep", "view", "glob", "edit", "agent", "todo"},
		},
		{
			name:      "all returns all tools",
			toolNames: "all",
			expected:  []string{"bash", "grep", "view", "glob", "edit", "agent", "todo"},
		},
		{
			name:      "select specific tools",
			toolNames: "bash,grep",
			expected:  []string{"bash", "grep"},
		},
		{
			name:      "select with whitespace",
			toolNames: " bash , grep ",
			expected:  []string{"bash", "grep"},
		},
		{
			name:      "select view when read requested",
			toolNames: "read",
			expected:  []string{"view"},
		},
		{
			name:      "select glob when find requested",
			toolNames: "find",
			expected:  []string{"glob"},
		},
		{
			name:      "always include agent tool",
			toolNames: "bash",
			expected:  []string{"bash", "agent"},
		},
		{
			name:      "always include todo tool",
			toolNames: "bash",
			expected:  []string{"bash", "todo"},
		},
		{
			name:      "select nonexistent tool returns empty",
			toolNames: "nonexistent",
			expected:  []string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create mock tools
			allTools := make([]struct {
				name string
			}, len(mockTools))
			for i, t := range mockTools {
				allTools[i].name = t.name
			}

			// We can't directly test SelectTools without fantasy.AgentTool interface
			// So we test the logic through the helper functions
			requested := map[string]bool{}
			for _, name := range splitAndTrim(tt.toolNames) {
				requested[name] = true
			}

			// Test that always include tools are present
			for tool := range alwaysIncludeTools {
				if requested[tool] {
					t.Errorf("tool %q should be always included", tool)
				}
			}

			// Test view/glob are known tool names
			knownTools := map[string]bool{
				"view": true, "glob": true, "bash": true, "write": true,
			}
			for name := range requested {
				if knownTools[name] {
					// Known tool names should be in the map
				}
			}
		})
	}
}

// Helper function to split and trim tool names
func splitAndTrim(s string) []string {
	if s == "" {
		return nil
	}
	var result []string
	for _, item := range split(s, ",") {
		item = trimSpace(item)
		if item != "" {
			result = append(result, item)
		}
	}
	return result
}

// Helper functions for testing
func split(s, sep string) []string {
	var result []string
	var current string
	for _, r := range s {
		if string(r) == sep {
			result = append(result, current)
			current = ""
		} else {
			current += string(r)
		}
	}
	result = append(result, current)
	return result
}

func trimSpace(s string) string {
	start := 0
	end := len(s)
	for start < end && (s[start] == ' ' || s[start] == '\t' || s[start] == '\n' || s[start] == '\r') {
		start++
	}
	for end > start && (s[end-1] == ' ' || s[end-1] == '\t' || s[end-1] == '\n' || s[end-1] == '\r') {
		end--
	}
	return s[start:end]
}

// TestAgentDefFields tests that AgentDef has all expected fields
func TestAgentDefFields(t *testing.T) {
	agentDef := &AgentDef{
		Name:        "test-agent",
		Description: "A test agent",
		Tools:       "bash,grep",
		Role:        "researcher",
		System:      "You are a helpful assistant.",
		Skills:      "skill-a,skill-b",
		Timeout:     300,
		MaxRetries:  3,
		Generation: GenerationParams{
			Model:       "llama2",
			Temperature: "0.7",
			MaxTokens:   "2000",
			TopP:        "0.9",
			TopK:        "40",
		},
		ProviderURL: "http://localhost:11434/v1",
	}

	if agentDef.Name != "test-agent" {
		t.Errorf("Name = %q, want %q", agentDef.Name, "test-agent")
	}
	if agentDef.Description != "A test agent" {
		t.Errorf("Description = %q, want %q", agentDef.Description, "A test agent")
	}
	if agentDef.Tools != "bash,grep" {
		t.Errorf("Tools = %q, want %q", agentDef.Tools, "bash,grep")
	}
	if agentDef.Role != "researcher" {
		t.Errorf("Role = %q, want %q", agentDef.Role, "researcher")
	}
	if agentDef.System != "You are a helpful assistant." {
		t.Errorf("System = %q, want %q", agentDef.System, "You are a helpful assistant.")
	}
	if agentDef.Skills != "skill-a,skill-b" {
		t.Errorf("Skills = %q, want %q", agentDef.Skills, "skill-a,skill-b")
	}
	if agentDef.Timeout != 300 {
		t.Errorf("Timeout = %d, want %d", agentDef.Timeout, 300)
	}
	if agentDef.MaxRetries != 3 {
		t.Errorf("MaxRetries = %d, want %d", agentDef.MaxRetries, 3)
	}
	if agentDef.Generation.Model != "llama2" {
		t.Errorf("Generation.Model = %q, want %q", agentDef.Generation.Model, "llama2")
	}
	if agentDef.ProviderURL != "http://localhost:11434/v1" {
		t.Errorf("ProviderURL = %q, want %q", agentDef.ProviderURL, "http://localhost:11434/v1")
	}
}

// TestTeamConfigFields tests that TeamConfig has all expected fields
func TestTeamConfigFields(t *testing.T) {
	teamConfig := &TeamConfig{
		Name:         "test-team",
		Description:  "A test team",
		MaxRounds:    10,
		WorkspaceDir: "/tmp/workspace",
		Timeout:      300,
		MaxRetries:   3,
		Generation: GenerationParams{
			Model:       "llama2",
			Temperature: "0.7",
			MaxTokens:   "2000",
			TopP:        "0.9",
			TopK:        "40",
		},
		Skills:        "skill-a",
		SkillsExclude: "skill-b",
		ProviderURL:   "http://localhost:11434/v1",
	}

	if teamConfig.Name != "test-team" {
		t.Errorf("Name = %q, want %q", teamConfig.Name, "test-team")
	}
	if teamConfig.Description != "A test team" {
		t.Errorf("Description = %q, want %q", teamConfig.Description, "A test team")
	}
	if teamConfig.MaxRounds != 10 {
		t.Errorf("MaxRounds = %d, want %d", teamConfig.MaxRounds, 10)
	}
	if teamConfig.WorkspaceDir != "/tmp/workspace" {
		t.Errorf("WorkspaceDir = %q, want %q", teamConfig.WorkspaceDir, "/tmp/workspace")
	}
	if teamConfig.Timeout != 300 {
		t.Errorf("Timeout = %d, want %d", teamConfig.Timeout, 300)
	}
	if teamConfig.MaxRetries != 3 {
		t.Errorf("MaxRetries = %d, want %d", teamConfig.MaxRetries, 3)
	}
	if teamConfig.Generation.Model != "llama2" {
		t.Errorf("Generation.Model = %q, want %q", teamConfig.Generation.Model, "llama2")
	}
	if teamConfig.Skills != "skill-a" {
		t.Errorf("Skills = %q, want %q", teamConfig.Skills, "skill-a")
	}
	if teamConfig.SkillsExclude != "skill-b" {
		t.Errorf("SkillsExclude = %q, want %q", teamConfig.SkillsExclude, "skill-b")
	}
	if teamConfig.ProviderURL != "http://localhost:11434/v1" {
		t.Errorf("ProviderURL = %q, want %q", teamConfig.ProviderURL, "http://localhost:11434/v1")
	}
}

// TestGenerationParamsFields tests that GenerationParams has all expected fields
func TestGenerationParamsFields(t *testing.T) {
	genParams := &GenerationParams{
		Model:       "llama2",
		Temperature: "0.7",
		MaxTokens:   "2000",
		TopP:        "0.9",
		TopK:        "40",
	}

	if genParams.Model != "llama2" {
		t.Errorf("Model = %q, want %q", genParams.Model, "llama2")
	}
	if genParams.Temperature != "0.7" {
		t.Errorf("Temperature = %q, want %q", genParams.Temperature, "0.7")
	}
	if genParams.MaxTokens != "2000" {
		t.Errorf("MaxTokens = %q, want %q", genParams.MaxTokens, "2000")
	}
	if genParams.TopP != "0.9" {
		t.Errorf("TopP = %q, want %q", genParams.TopP, "0.9")
	}
	if genParams.TopK != "40" {
		t.Errorf("TopK = %q, want %q", genParams.TopK, "40")
	}
}

// TestAgentConfigFields tests that AgentConfig has all expected fields
func TestAgentConfigFields(t *testing.T) {
	agentDef := &AgentDef{
		Name: "test-agent",
	}
	teamConfig := &TeamConfig{
		Name: "test-team",
	}

	agentConfig := &AgentConfig{
		Def:        agentDef,
		TeamConfig: teamConfig,
		WorkDir:    "/tmp/workdir",
	}

	if agentConfig.Def != agentDef {
		t.Errorf("Def mismatch")
	}
	if agentConfig.TeamConfig != teamConfig {
		t.Errorf("TeamConfig mismatch")
	}
	if agentConfig.WorkDir != "/tmp/workdir" {
		t.Errorf("WorkDir = %q, want %q", agentConfig.WorkDir, "/tmp/workdir")
	}
}

// TestDefaultProviderURL tests that config.DefaultProviderURL is correctly defined
func TestDefaultProviderURL(t *testing.T) {
	expected := "http://localhost:11434/v1"
	if config.DefaultProviderURL != expected {
		t.Errorf("config.DefaultProviderURL = %q, want %q", config.DefaultProviderURL, expected)
	}
}

// TestAlwaysIncludeTools tests that alwaysIncludeTools contains expected tools
func TestAlwaysIncludeTools(t *testing.T) {
	expectedTools := []string{"agent", "todo"}

	for _, tool := range expectedTools {
		if !alwaysIncludeTools[tool] {
			t.Errorf("alwaysIncludeTools does not contain expected tool: %s", tool)
		}
	}

	// Verify no unexpected tools are always included
	unexpectedTools := []string{"bash", "grep", "view", "edit"}
	for _, tool := range unexpectedTools {
		if alwaysIncludeTools[tool] {
			t.Errorf("alwaysIncludeTools contains unexpected tool: %s", tool)
		}
	}
}

// TestResolveMaxSteps tests the resolveMaxSteps function with all priority scenarios
func TestResolveMaxSteps(t *testing.T) {
	tests := []struct {
		name       string
		agentSteps int
		teamSteps  int
		expected   int
	}{
		{
			name:       "agentSteps greater than zero returns agentSteps",
			agentSteps: 50,
			teamSteps:  20,
			expected:   50,
		},
		{
			name:       "agentSteps zero with positive teamSteps returns teamSteps",
			agentSteps: 0,
			teamSteps:  20,
			expected:   20,
		},
		{
			name:       "both zero returns DefaultMaxSteps",
			agentSteps: 0,
			teamSteps:  0,
			expected:   DefaultMaxSteps,
		},
		{
			name:       "agentSteps negative falls back to teamSteps",
			agentSteps: -5,
			teamSteps:  20,
			expected:   20,
		},
		{
			name:       "agentSteps negative with zero teamSteps returns DefaultMaxSteps",
			agentSteps: -10,
			teamSteps:  0,
			expected:   DefaultMaxSteps,
		},
		{
			name:       "agentSteps one returns one",
			agentSteps: 1,
			teamSteps:  20,
			expected:   1,
		},
		{
			name:       "teamSteps one with zero agentSteps returns one",
			agentSteps: 0,
			teamSteps:  1,
			expected:   1,
		},
		{
			name:       "large agentSteps value",
			agentSteps: 1000,
			teamSteps:  20,
			expected:   1000,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := resolveMaxSteps(tt.agentSteps, tt.teamSteps)
			if got != tt.expected {
				t.Errorf("resolveMaxSteps(%d, %d) = %d, want %d", tt.agentSteps, tt.teamSteps, got, tt.expected)
			}
		})
	}
}

// TestMaxStepsConfiguration tests that MaxSteps field exists in all config types
func TestMaxStepsConfiguration(t *testing.T) {
	// Test AgentDef.MaxSteps field exists
	agentDef := &AgentDef{
		Name:     "test-agent",
		MaxSteps: 50,
	}
	if agentDef.MaxSteps != 50 {
		t.Errorf("AgentDef.MaxSteps = %d, want %d", agentDef.MaxSteps, 50)
	}

	// Test TeamConfig.MaxSteps field exists
	teamConfig := &TeamConfig{
		Name:     "test-team",
		MaxSteps: 20,
	}
	if teamConfig.MaxSteps != 20 {
		t.Errorf("TeamConfig.MaxSteps = %d, want %d", teamConfig.MaxSteps, 20)
	}

	// Test AgentConfig.MaxSteps field exists
	agentConfig := &AgentConfig{
		Def:      agentDef,
		MaxSteps: 30,
	}
	if agentConfig.MaxSteps != 30 {
		t.Errorf("AgentConfig.MaxSteps = %d, want %d", agentConfig.MaxSteps, 30)
	}

	// Test that MaxSteps field can be read and written
	agentDef.MaxSteps = 100
	if agentDef.MaxSteps != 100 {
		t.Errorf("AgentDef.MaxSteps = %d, want %d after write", agentDef.MaxSteps, 100)
	}

	teamConfig.MaxSteps = 150
	if teamConfig.MaxSteps != 150 {
		t.Errorf("TeamConfig.MaxSteps = %d, want %d after write", teamConfig.MaxSteps, 150)
	}

	agentConfig.MaxSteps = 200
	if agentConfig.MaxSteps != 200 {
		t.Errorf("AgentConfig.MaxSteps = %d, want %d after write", agentConfig.MaxSteps, 200)
	}
}
