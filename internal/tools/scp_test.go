//go:build linux || darwin
// +build linux darwin

package tools

import (
	"context"
	"strings"
	"testing"

	"charm.land/fantasy"
)

func TestScpTool_Info(t *testing.T) {
	tool := NewScpTool()
	info := tool.Info()

	if info.Name != "scp" {
		t.Errorf("Name = %q, want scp", info.Name)
	}
}

func TestSCP_ForceMCP(t *testing.T) {
	tool := NewScpTool()
	ctx := context.WithValue(context.Background(), AgentForceMCPKey, true)

	input := `{"source": "/tmp/test.txt", "destination": "/remote/", "host": "user@example.com"}`
	result, err := tool.Run(ctx, fantasy.ToolCall{Input: input})

	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}
	if !result.IsError {
		t.Error("Expected error when --force-mcp is enabled")
	}
	if !strings.Contains(result.Content, "blocked by --force-mcp") {
		t.Errorf("Expected force-mcp error message, got %q", result.Content)
	}
}

func TestSCP_InvalidPort(t *testing.T) {
	tool := NewScpTool()
	ctx := SetToolsAllowed(context.Background(), []string{"scp"})

	input := `{"source": "/tmp/test.txt", "destination": "/remote/", "host": "user@example.com", "port": 70000}`
	result, err := tool.Run(ctx, fantasy.ToolCall{Input: input})

	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}
	if !result.IsError {
		t.Error("Expected error for invalid port")
	}
	if !strings.Contains(result.Content, "port must be 0-65535") {
		t.Errorf("Expected port validation error, got %q", result.Content)
	}
}

func TestSCP_MissingHost(t *testing.T) {
	tool := NewScpTool()
	ctx := SetToolsAllowed(context.Background(), []string{"scp"})

	input := `{"source": "/tmp/test.txt", "destination": "/remote/"}`
	result, err := tool.Run(ctx, fantasy.ToolCall{Input: input})

	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}
	if !result.IsError {
		t.Error("Expected error for missing host")
	}
}

func TestSCP_IdentityFileNotFound(t *testing.T) {
	tool := NewScpTool()
	ctx := SetToolsAllowed(context.Background(), []string{"scp"})

	input := `{
		"source": "/tmp/test.txt",
		"destination": "/remote/",
		"host": "user@example.com",
		"identity_file": "/nonexistent/key"
	}`
	result, err := tool.Run(ctx, fantasy.ToolCall{Input: input})

	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}
	if !result.IsError {
		t.Error("Expected error for missing identity file")
	}
	if !strings.Contains(result.Content, "identity file not found") {
		t.Errorf("Expected identity file error, got %q", result.Content)
	}
}
