//go:build linux || darwin
// +build linux darwin

package tools

import (
	"context"
	"testing"

	"charm.land/fantasy"
)

func TestExecuteSSH_ForceMCP(t *testing.T) {
	tool := NewSshTool()
	ctx := context.WithValue(context.Background(), AgentForceMCPKey, true)

	input := `{"host": "user@example.com", "command": "uptime"}`
	result, err := tool.Run(ctx, fantasy.ToolCall{Input: input})

	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}
	if !result.IsError {
		t.Error("Expected error when --force-mcp is enabled")
	}
	if result.Content == "" {
		t.Error("Expected error message")
	}
	if !contains(result.Content, "blocked by --force-mcp") {
		t.Errorf("Expected error message to mention 'blocked by --force-mcp', got: %s", result.Content)
	}
	if !contains(result.Content, "MCP server") {
		t.Errorf("Expected error message to mention MCP server, got: %s", result.Content)
	}
}

func TestExecuteSSH_ForceMCP_Direct(t *testing.T) {
	ctx := context.WithValue(context.Background(), AgentForceMCPKey, true)

	input := `{"host": "user@example.com", "command": "uptime"}`
	result, err := executeSSH(ctx, fantasy.ToolCall{Input: input})

	if err != nil {
		t.Fatalf("executeSSH() error: %v", err)
	}
	if !result.IsError {
		t.Error("Expected error when --force-mcp is enabled")
	}
	if result.Content == "" {
		t.Error("Expected error message")
	}
	if !contains(result.Content, "blocked by --force-mcp") {
		t.Errorf("Expected error message to mention 'blocked by --force-mcp', got: %s", result.Content)
	}
	if !contains(result.Content, "MCP server") {
		t.Errorf("Expected error message to mention MCP server, got: %s", result.Content)
	}
}

func contains(s, substr string) bool {
	return len(s) > 0 && len(substr) > 0 && (s == substr || len(s) > len(substr) && (s[:len(substr)] == substr || s[len(s)-len(substr):] == substr || containsHelper(s, substr)))
}

func containsHelper(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
