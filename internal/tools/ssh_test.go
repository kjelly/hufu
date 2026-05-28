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

func TestSSH_ConnectionReuse(t *testing.T) {
	tool := NewSshTool()
	ctx := SetToolsAllowed(context.Background(), []string{"ssh"})

	input := `{
		"host": "user@example.com",
		"command": "uptime",
		"connection_reuse": true
	}`

	_, err := tool.Run(ctx, fantasy.ToolCall{Input: input})
	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}
	t.Log("Connection reuse parameter accepted")
}

func TestSSH_ControlPath(t *testing.T) {
	tool := NewSshTool()
	ctx := SetToolsAllowed(context.Background(), []string{"ssh"})

	input := `{
		"host": "user@example.com",
		"command": "uptime",
		"connection_reuse": true,
		"control_path": "/tmp/custom-ssh-socket"
	}`

	_, err := tool.Run(ctx, fantasy.ToolCall{Input: input})
	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}
	t.Log("Custom control path parameter accepted")
}

func TestLooksLikeIP(t *testing.T) {
	tests := []struct {
		input string
		want  bool
	}{
		// IPv4 addresses
		{"10.1.24.229", true},
		{"192.168.1.1", true},
		{"1.1.1.1", true},
		{"255.255.255.255", true},
		// IPv6 addresses
		{"::1", true},
		{"2001:db8::1", true},
		{"fe80::1", true},
		// Hostnames
		{"offline-test-gpu", false},
		{"example.com", false},
		{"localhost", false},
		{"server1.example.com", false},
		// user@host
		{"user@10.1.24.229", false}, // has @ symbol
		{"user@example.com", false},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := looksLikeIP(tt.input)
			if got != tt.want {
				t.Errorf("looksLikeIP(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}

func TestExecuteSSH_IPAddress_Warning(t *testing.T) {
	tool := NewSshTool()
	ctx := SetToolsAllowed(context.Background(), []string{"ssh"})

	// Test with IP address - should return error with warning
	input := `{"host": "10.1.24.229", "command": "uptime"}`
	result, err := tool.Run(ctx, fantasy.ToolCall{Input: input})

	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}
	if !result.IsError {
		t.Error("Expected error when host is IP address")
	}
	if !strings.Contains(result.Content, "IP address") {
		t.Errorf("Expected warning about IP address, got: %s", result.Content)
	}
	if !strings.Contains(result.Content, "SSH config") {
		t.Errorf("Expected mention of SSH config, got: %s", result.Content)
	}
	if !strings.Contains(result.Content, "hostname") {
		t.Errorf("Expected mention of hostname, got: %s", result.Content)
	}
}

func TestExecuteSSH_Hostname_Allowed(t *testing.T) {
	tool := NewSshTool()
	ctx := SetToolsAllowed(context.Background(), []string{"ssh"})

	// Test with hostname - should not return IP warning
	input := `{"host": "offline-test-gpu", "command": "uptime"}`
	result, err := tool.Run(ctx, fantasy.ToolCall{Input: input})

	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}
	// Should not be an error about IP address
	if result.IsError && strings.Contains(result.Content, "IP address") {
		t.Errorf("Unexpected IP warning for hostname: %s", result.Content)
	}
}

func TestSSH_UserParameter(t *testing.T) {
	tool := NewSshTool()
	ctx := SetToolsAllowed(context.Background(), []string{"ssh"})

	// Test with explicit user parameter
	input := `{"host": "server.example.com", "user": "admin", "command": "uptime"}`
	result, err := tool.Run(ctx, fantasy.ToolCall{Input: input})

	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}
	// Should not be an error about IP address (server.example.com is not an IP)
	if result.IsError && strings.Contains(result.Content, "IP address") {
		t.Errorf("Unexpected IP warning: %s", result.Content)
	}
}

func TestSSH_UserAtHostFormat(t *testing.T) {
	tool := NewSshTool()
	ctx := SetToolsAllowed(context.Background(), []string{"ssh"})

	// Test with user@host format
	input := `{"host": "admin@server.example.com", "command": "uptime"}`
	result, err := tool.Run(ctx, fantasy.ToolCall{Input: input})

	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}
	// Should not be an error about IP address
	if result.IsError && strings.Contains(result.Content, "IP address") {
		t.Errorf("Unexpected IP warning: %s", result.Content)
	}
}

func TestSSH_UserParameterOverridesUserAtHost(t *testing.T) {
	tool := NewSshTool()
	ctx := SetToolsAllowed(context.Background(), []string{"ssh"})

	// Test that explicit user parameter overrides user@host format
	input := `{"host": "admin@server.example.com", "user": "root", "command": "uptime"}`
	result, err := tool.Run(ctx, fantasy.ToolCall{Input: input})

	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}
	// Should not be an error about IP address
	if result.IsError && strings.Contains(result.Content, "IP address") {
		t.Errorf("Unexpected IP warning: %s", result.Content)
	}
}

func TestSSH_InvalidPort(t *testing.T) {
	tool := NewSshTool()
	ctx := SetToolsAllowed(context.Background(), []string{"ssh"})

	// Test with invalid port (negative)
	input := `{"host": "server.example.com", "port": -1}`
	result, err := tool.Run(ctx, fantasy.ToolCall{Input: input})

	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}
	if !result.IsError {
		t.Error("Expected error for negative port")
	}
	if !strings.Contains(result.Content, "port must be 0-65535") {
		t.Errorf("Expected port validation error, got: %s", result.Content)
	}
}

func TestSSH_PortOutOfRange(t *testing.T) {
	tool := NewSshTool()
	ctx := SetToolsAllowed(context.Background(), []string{"ssh"})

	// Test with port > 65535
	input := `{"host": "server.example.com", "port": 70000}`
	result, err := tool.Run(ctx, fantasy.ToolCall{Input: input})

	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}
	if !result.IsError {
		t.Error("Expected error for port out of range")
	}
	if !strings.Contains(result.Content, "port must be 0-65535") {
		t.Errorf("Expected port validation error, got: %s", result.Content)
	}
}

func TestSSH_IdentityFileNotFound(t *testing.T) {
	tool := NewSshTool()
	ctx := SetToolsAllowed(context.Background(), []string{"ssh"})

	// Test with non-existent identity file
	input := `{"host": "server.example.com", "identity_file": "/nonexistent/path/key"}`
	result, err := tool.Run(ctx, fantasy.ToolCall{Input: input})

	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}
	if !result.IsError {
		t.Error("Expected error for missing identity file")
	}
	if !strings.Contains(result.Content, "identity file not found") {
		t.Errorf("Expected identity file error, got: %s", result.Content)
	}
}
