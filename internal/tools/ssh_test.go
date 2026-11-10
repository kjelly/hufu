//go:build linux || darwin
// +build linux darwin

package tools

import (
	"context"
	"os/exec"
	"slices"
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
		name     string
		exitCode int
		stderr   string
		want     string
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

// Each tool owns its command factory; no process-wide override can leak to
// another test or invocation. The harmless child still exercises pipe handling.
func newSSHTestTool(t *testing.T) (fantasy.AgentTool, *[][]string) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	var calls [][]string
	tool := newSshTool(func(ctx context.Context, name string, args ...string) *exec.Cmd {
		calls = append(calls, append([]string{name}, args...))
		return exec.CommandContext(ctx, "true")
	})
	return tool, &calls
}

func TestSSHCommandArguments(t *testing.T) {
	cases := []struct {
		name        string
		input       string
		wantHost    string
		wantOptions []string
	}{
		{name: "hostname", input: `{"host":"offline-test-gpu","command":"uptime"}`, wantHost: "offline-test-gpu"},
		{name: "explicit user", input: `{"host":"server.example.com","user":"admin","command":"uptime"}`, wantHost: "admin@server.example.com"},
		{name: "user at host", input: `{"host":"admin@server.example.com","command":"uptime"}`, wantHost: "admin@server.example.com"},
		{name: "explicit user wins", input: `{"host":"admin@server.example.com","user":"root","command":"uptime"}`, wantHost: "root@server.example.com"},
		{name: "reuse", input: `{"host":"user@example.com","command":"uptime","connection_reuse":true}`, wantHost: "user@example.com", wantOptions: []string{"ControlMaster=auto", "ControlPersist=600"}},
		{name: "custom control path", input: `{"host":"user@example.com","command":"uptime","connection_reuse":true,"control_path":"custom-socket"}`, wantHost: "user@example.com", wantOptions: []string{"ControlPath=custom-socket"}},
		{name: "explicit port", input: `{"host":"example.com","command":"uptime","port":2222}`, wantHost: "example.com", wantOptions: []string{"-p", "2222"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tool, calls := newSSHTestTool(t)
			ctx := SetToolsAllowed(t.Context(), []string{"ssh"})
			result, err := tool.Run(ctx, fantasy.ToolCall{Input: tc.input})
			if err != nil || result.IsError {
				t.Fatalf("SSH failed: %v %s", err, result.Content)
			}
			if len(*calls) != 1 {
				t.Fatalf("command calls = %v, want one", *calls)
			}
			args := (*calls)[0]
			if args[0] != "ssh" || !slices.Equal(args[len(args)-2:], []string{tc.wantHost, "uptime"}) {
				t.Fatalf("command = %v, want ssh ending with [%s uptime]", args, tc.wantHost)
			}
			for _, option := range append([]string{"BatchMode=yes"}, tc.wantOptions...) {
				if !slices.Contains(args, option) {
					t.Errorf("command %v missing argument %q", args, option)
				}
			}
		})
	}
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
