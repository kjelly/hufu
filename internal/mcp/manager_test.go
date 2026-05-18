package mcp

import (
	"testing"
)

// TestNewMCPToolManager tests the NewMCPToolManager function
func TestNewMCPToolManager(t *testing.T) {
	manager := NewMCPToolManager("bash", "bash")

	if manager == nil {
		t.Fatal("NewMCPToolManager() returned nil")
	}

	if manager.clients == nil {
		t.Error("NewMCPToolManager() returned manager with nil clients map")
	}

	if manager.toolMap == nil {
		t.Error("NewMCPToolManager() returned manager with nil toolMap")
	}

	if manager.tools != nil && len(manager.tools) != 0 {
		t.Errorf("NewMCPToolManager() returned manager with %d tools, want 0", len(manager.tools))
	}
}

// TestMCPToolManagerGetTools tests the GetTools method
func TestMCPToolManagerGetTools(t *testing.T) {
	manager := NewMCPToolManager("bash", "bash")

	// Initially should return empty slice
	tools := manager.GetTools()
	if tools == nil {
		t.Error("GetTools() returned nil")
	}
	if len(tools) != 0 {
		t.Errorf("GetTools() returned %d tools, want 0", len(tools))
	}
}

// TestMCPToolManagerAsAgentTools tests the AsAgentTools method
func TestMCPToolManagerAsAgentTools(t *testing.T) {
	manager := NewMCPToolManager("bash", "bash")

	// Initially should return empty slice
	tools := manager.AsAgentTools()
	if tools == nil {
		t.Error("AsAgentTools() returned nil")
	}
	if len(tools) != 0 {
		t.Errorf("AsAgentTools() returned %d tools, want 0", len(tools))
	}
}

// TestMCPToolFields tests that MCPTool has all expected fields
func TestMCPToolFields(t *testing.T) {
	tool := MCPTool{
		Name:        "test-server__test-tool",
		Description: "Test tool description",
		Parameters: map[string]any{
			"param1": "value1",
		},
		Required:   []string{"param1"},
		ServerName: "test-server",
		OrigName:   "test-tool",
	}

	if tool.Name != "test-server__test-tool" {
		t.Errorf("Name = %q, want %q", tool.Name, "test-server__test-tool")
	}
	if tool.Description != "Test tool description" {
		t.Errorf("Description = %q, want %q", tool.Description, "Test tool description")
	}
	if len(tool.Parameters) != 1 {
		t.Errorf("Parameters length = %d, want 1", len(tool.Parameters))
	}
	if len(tool.Required) != 1 {
		t.Errorf("Required length = %d, want 1", len(tool.Required))
	}
	if tool.ServerName != "test-server" {
		t.Errorf("ServerName = %q, want %q", tool.ServerName, "test-server")
	}
	if tool.OrigName != "test-tool" {
		t.Errorf("OrigName = %q, want %q", tool.OrigName, "test-tool")
	}
}

// TestIsToolAllowed tests the isToolAllowed function
func TestIsToolAllowed(t *testing.T) {
	tests := []struct {
		name       string
		toolName   string
		allowed    []string
		excluded   []string
		wantResult bool
	}{
		{
			name:       "no restrictions allows all",
			toolName:   "test-tool",
			allowed:    nil,
			excluded:   nil,
			wantResult: true,
		},
		{
			name:       "excluded tool is not allowed",
			toolName:   "test-tool",
			allowed:    nil,
			excluded:   []string{"test-tool"},
			wantResult: false,
		},
		{
			name:       "included tool is allowed",
			toolName:   "test-tool",
			allowed:    []string{"test-tool"},
			excluded:   nil,
			wantResult: true,
		},
		{
			name:       "included tool not in list is not allowed",
			toolName:   "other-tool",
			allowed:    []string{"test-tool"},
			excluded:   nil,
			wantResult: false,
		},
		{
			name:       "excluded takes precedence over included",
			toolName:   "test-tool",
			allowed:    []string{"test-tool"},
			excluded:   []string{"test-tool"},
			wantResult: false,
		},
		{
			name:       "case-sensitive matching",
			toolName:   "Test-Tool",
			allowed:    []string{"test-tool"},
			excluded:   nil,
			wantResult: false,
		},
		{
			name:       "excluded with case-sensitive matching",
			toolName:   "Test-Tool",
			allowed:    nil,
			excluded:   []string{"test-tool"},
			wantResult: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isToolAllowed(tt.toolName, tt.allowed, tt.excluded)
			if got != tt.wantResult {
				t.Errorf("isToolAllowed(%q, %v, %v) = %v, want %v",
					tt.toolName, tt.allowed, tt.excluded, got, tt.wantResult)
			}
		})
	}
}

// TestMCPToolManagerGetToolsWithTools tests GetTools after adding tools
func TestMCPToolManagerGetToolsWithTools(t *testing.T) {
	manager := NewMCPToolManager("bash", "bash")

	// Manually add a tool for testing
	manager.mu.Lock()
	manager.tools = append(manager.tools, MCPTool{
		Name:        "test-tool",
		Description: "Test tool",
		Parameters:  map[string]any{},
		Required:    []string{},
		ServerName:  "test",
		OrigName:    "test",
	})
	manager.toolMap["test-tool"] = manager.tools[0]
	manager.mu.Unlock()

	tools := manager.GetTools()
	if len(tools) != 1 {
		t.Errorf("GetTools() returned %d tools, want 1", len(tools))
	}
	if tools[0].Name != "test-tool" {
		t.Errorf("GetTools()[0].Name = %q, want %q", tools[0].Name, "test-tool")
	}
}

// TestMCPToolManagerAsAgentToolsWithTools tests AsAgentTools after adding tools
func TestMCPToolManagerAsAgentToolsWithTools(t *testing.T) {
	manager := NewMCPToolManager("bash", "bash")

	// Manually add a tool for testing
	manager.mu.Lock()
	manager.tools = append(manager.tools, MCPTool{
		Name:        "test-tool",
		Description: "Test tool",
		Parameters:  map[string]any{},
		Required:    []string{},
		ServerName:  "test",
		OrigName:    "test",
	})
	manager.toolMap["test-tool"] = manager.tools[0]
	manager.mu.Unlock()

	tools := manager.AsAgentTools()
	if len(tools) != 1 {
		t.Errorf("AsAgentTools() returned %d tools, want 1", len(tools))
	}
}

// TestMCPToolManagerConcurrentAccess tests concurrent access to MCPToolManager
func TestMCPToolManagerConcurrentAccess(t *testing.T) {
	manager := NewMCPToolManager("bash", "bash")

	done := make(chan bool)

	// Start multiple goroutines reading tools
	for i := 0; i < 10; i++ {
		go func() {
			defer func() {
				done <- true
			}()
			_ = manager.GetTools()
		}()
	}

	// Start multiple goroutines adding tools
	for i := 0; i < 10; i++ {
		go func() {
			defer func() {
				done <- true
			}()
			manager.mu.Lock()
			manager.tools = append(manager.tools, MCPTool{
				Name:        "test-tool",
				Description: "Test tool",
				Parameters:  map[string]any{},
				Required:    []string{},
				ServerName:  "test",
				OrigName:    "test",
			})
			manager.mu.Unlock()
		}()
	}

	// Wait for all goroutines to complete
	for i := 0; i < 20; i++ {
		<-done
	}
}

// TestMCPToolManagerClose tests the Close method
func TestMCPToolManagerClose(t *testing.T) {
	manager := NewMCPToolManager("bash", "bash")

	err := manager.Close()
	if err != nil {
		t.Errorf("Close() error = %v", err)
	}

	// After close, tools should be cleared
	tools := manager.GetTools()
	if len(tools) != 0 {
		t.Errorf("After Close(), GetTools() returned %d tools, want 0", len(tools))
	}
}

// TestMCPToolManagerCloseMultipleTimes tests calling Close multiple times
func TestMCPToolManagerCloseMultipleTimes(t *testing.T) {
	manager := NewMCPToolManager("bash", "bash")

	err1 := manager.Close()
	err2 := manager.Close()

	// Both closes should not panic
	if err1 != nil {
		t.Logf("First close error: %v", err1)
	}
	if err2 != nil {
		t.Logf("Second close error: %v", err2)
	}
}

// TestMCPToolInfo tests the Info method of mcpAgentTool
func TestMCPToolInfo(t *testing.T) {
	manager := NewMCPToolManager("bash", "bash")

	// Add a tool
	manager.mu.Lock()
	manager.tools = append(manager.tools, MCPTool{
		Name:        "test-tool",
		Description: "Test tool description",
		Parameters: map[string]any{
			"param1": "value1",
		},
		Required:   []string{"param1"},
		ServerName: "test",
		OrigName:   "test",
	})
	manager.toolMap["test-tool"] = manager.tools[0]
	manager.mu.Unlock()

	tools := manager.AsAgentTools()
	if len(tools) != 1 {
		t.Fatalf("AsAgentTools() returned %d tools, want 1", len(tools))
	}

	info := tools[0].Info()
	if info.Name != "test-tool" {
		t.Errorf("Info().Name = %q, want %q", info.Name, "test-tool")
	}
	if info.Description != "Test tool description" {
		t.Errorf("Info().Description = %q, want %q", info.Description, "Test tool description")
	}
	if len(info.Parameters) != 1 {
		t.Errorf("Info().Parameters length = %d, want 1", len(info.Parameters))
	}
	if len(info.Required) != 1 {
		t.Errorf("Info().Required length = %d, want 1", len(info.Required))
	}
}

// TestMCPToolProviderOptions tests the ProviderOptions methods of mcpAgentTool
func TestMCPToolProviderOptions(t *testing.T) {
	manager := NewMCPToolManager("bash", "bash")

	// Add a tool
	manager.mu.Lock()
	manager.tools = append(manager.tools, MCPTool{
		Name:        "test-tool",
		Description: "Test tool",
		Parameters:  map[string]any{},
		Required:    []string{},
		ServerName:  "test",
		OrigName:    "test",
	})
	manager.toolMap["test-tool"] = manager.tools[0]
	manager.mu.Unlock()

	tools := manager.AsAgentTools()
	if len(tools) != 1 {
		t.Fatalf("AsAgentTools() returned %d tools, want 1", len(tools))
	}

	tool := tools[0]

	// Test initial provider options - may be nil or empty, that's ok
	_ = tool.ProviderOptions()
}

// TestMCPServerConfigFields tests that MCPServerConfig has all expected fields
func TestMCPServerConfigFields(t *testing.T) {
	config := MCPServerConfig{
		Type:          "local",
		Command:       []string{"mcp-server"},
		Environment:   map[string]string{"KEY": "value"},
		URL:           "http://example.com",
		AllowedTools:  []string{"tool1", "tool2"},
		ExcludedTools: []string{"tool3"},
		NoOAuth:       true,
	}

	if config.Type != "local" {
		t.Errorf("Type = %q, want %q", config.Type, "local")
	}
	if len(config.Command) != 1 || config.Command[0] != "mcp-server" {
		t.Errorf("Command = %v, want [%q]", config.Command, "mcp-server")
	}
	if len(config.Environment) != 1 || config.Environment["KEY"] != "value" {
		t.Errorf("Environment = %v, want map[KEY:value]", config.Environment)
	}
	if config.URL != "http://example.com" {
		t.Errorf("URL = %q, want %q", config.URL, "http://example.com")
	}
	if len(config.AllowedTools) != 2 {
		t.Errorf("AllowedTools length = %d, want 2", len(config.AllowedTools))
	}
	if len(config.ExcludedTools) != 1 {
		t.Errorf("ExcludedTools length = %d, want 1", len(config.ExcludedTools))
	}
	if !config.NoOAuth {
		t.Error("NoOAuth = false, want true")
	}
}

// TestBlockedEnvVars tests that blocked environment variables are correctly defined
func TestBlockedEnvVars(t *testing.T) {
	blockedVars := []string{
		"LD_PRELOAD",
		"LD_LIBRARY_PATH",
		"DYLD_INSERT_LIBRARIES",
		"DYLD_LIBRARY_PATH",
		"__AFL_PRELOAD",
	}

	for _, varName := range blockedVars {
		if !blockedEnvVars[varName] {
			t.Errorf("blockedEnvVars does not contain expected blocked variable: %s", varName)
		}
	}
}

// TestMCPToolManagerGetToolsThreadSafety tests thread safety of GetTools
func TestMCPToolManagerGetToolsThreadSafety(t *testing.T) {
	manager := NewMCPToolManager("bash", "bash")

	done := make(chan bool)

	// Start multiple goroutines reading tools
	for i := 0; i < 100; i++ {
		go func() {
			defer func() {
				done <- true
			}()
			_ = manager.GetTools()
		}()
	}

	// Wait for all goroutines to complete
	for i := 0; i < 100; i++ {
		<-done
	}
}

// TestMCPToolManagerAsAgentToolsThreadSafety tests thread safety of AsAgentTools
func TestMCPToolManagerAsAgentToolsThreadSafety(t *testing.T) {
	manager := NewMCPToolManager("bash", "bash")

	done := make(chan bool)

	// Start multiple goroutines getting agent tools
	for i := 0; i < 100; i++ {
		go func() {
			defer func() {
				done <- true
			}()
			_ = manager.AsAgentTools()
		}()
	}

	// Wait for all goroutines to complete
	for i := 0; i < 100; i++ {
		<-done
	}
}

// TestMCPToolManagerEmptyTools tests GetTools with empty tools
func TestMCPToolManagerEmptyTools(t *testing.T) {
	manager := NewMCPToolManager("bash", "bash")

	tools := manager.GetTools()

	// Should return empty slice, not nil
	if tools == nil {
		t.Error("GetTools() returned nil, want empty slice")
	}
	if len(tools) != 0 {
		t.Errorf("GetTools() returned %d tools, want 0", len(tools))
	}
}

// TestMCPToolManagerEmptyToolMap tests GetTools with empty toolMap
func TestMCPToolManagerEmptyToolMap(t *testing.T) {
	manager := NewMCPToolManager("bash", "bash")

	// Manually set toolMap to empty
	manager.mu.Lock()
	manager.toolMap = make(map[string]MCPTool)
	manager.mu.Unlock()

	tools := manager.GetTools()

	// Should return empty slice
	if tools == nil {
		t.Error("GetTools() returned nil, want empty slice")
	}
}
