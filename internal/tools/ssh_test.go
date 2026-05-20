//go:build linux || darwin
// +build linux darwin

package tools

import (
	"context"
	"strings"
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

func TestDiagnoseSSHErrors_AuthFailed(t *testing.T) {
	stderr := "Permission denied (publickey,password)."
	result := diagnoseSSHErrors(255, stderr)

	if !strings.Contains(result, "SSH authentication failed") {
		t.Errorf("Expected authentication failure message, got %q", result)
	}
	if !strings.Contains(result, "Identity file permissions") {
		t.Error("Expected identity file troubleshooting")
	}
}

func TestDiagnoseSSHErrors_ConnectionRefused(t *testing.T) {
	stderr := "ssh: connect to host example.com port 22: Connection refused"
	result := diagnoseSSHErrors(255, stderr)

	if !strings.Contains(result, "SSH connection refused") {
		t.Errorf("Expected connection refused message, got %q", result)
	}
	if !strings.Contains(result, "SSH daemon running") {
		t.Error("Expected SSH daemon troubleshooting")
	}
}

func TestDiagnoseSSHErrors_HostUnreachable(t *testing.T) {
	stderr := "ssh: connect to host 192.168.1.100 port 22: No route to host"
	result := diagnoseSSHErrors(255, stderr)

	if !strings.Contains(result, "Host unreachable") {
		t.Errorf("Expected host unreachable message, got %q", result)
	}
	if !strings.Contains(result, "Network connectivity") {
		t.Error("Expected network connectivity troubleshooting")
	}
}

func TestDiagnoseSSHErrors_Timeout(t *testing.T) {
	result := diagnoseSSHErrors(124, "ssh connection timed out")

	if !strings.Contains(result, "timed out") {
		t.Errorf("Expected timeout message, got %q", result)
	}
}

func TestGetSSHErrorTitle(t *testing.T) {
	tests := []struct {
		name   string
		exitCode int
		stderr string
		want   string
	}{
		{"auth failed", 255, "Permission denied", "Authentication Failed"},
		{"connection refused", 255, "Connection refused", "Connection Refused"},
		{"host unreachable", 255, "No route to host", "Host Unreachable"},
		{"timeout", 124, "", "Timeout"},
		{"generic error", 255, "unknown error", "SSH Error"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := getSSHErrorTitle(tt.exitCode, tt.stderr)
			if got != tt.want {
				t.Errorf("getSSHErrorTitle() = %q, want %q", got, tt.want)
			}
		})
	}
}
