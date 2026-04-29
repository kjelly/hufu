package audit

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestNewAuditLogger tests the NewAuditLogger function
func TestNewAuditLogger(t *testing.T) {
	// Create a temporary directory for testing
	tmpDir := t.TempDir()
	teamName := "test-team"

	logger, err := NewAuditLogger(tmpDir, teamName)
	if err != nil {
		t.Fatalf("NewAuditLogger() error = %v", err)
	}
	defer logger.Close()

	// Verify the audit directory was created
	auditDir := filepath.Join(tmpDir, "audit")
	if _, err := os.Stat(auditDir); os.IsNotExist(err) {
		t.Errorf("NewAuditLogger() did not create audit directory")
	}

	// Verify the logger has the correct team name
	// We can't directly access teamName, but we can verify the logger was created
	if logger == nil {
		t.Error("NewAuditLogger() returned nil logger")
	}
}

// TestNewAuditLoggerInvalidDir tests NewAuditLogger with invalid directory
func TestNewAuditLoggerInvalidDir(t *testing.T) {
	teamName := "test-team"

	// Try to create audit logger in a non-writable location
	_, err := NewAuditLogger("/root/nonexistent", teamName)
	if err == nil {
		t.Error("NewAuditLogger() expected error for invalid directory, got nil")
	}
}

// TestAuditLoggerLogToolCall tests the LogToolCall method
func TestAuditLoggerLogToolCall(t *testing.T) {
	tmpDir := t.TempDir()
	teamName := "test-team"

	logger, err := NewAuditLogger(tmpDir, teamName)
	if err != nil {
		t.Fatalf("NewAuditLogger() error = %v", err)
	}
	defer logger.Close()

	// Log a tool call
	agent := "test-agent"
	tool := "bash"
	input := "echo hello"

	logger.LogToolCall(agent, tool, input)

	// Verify the log file was created and contains the entry
	auditDir := filepath.Join(tmpDir, "audit")
	files, err := os.ReadDir(auditDir)
	if err != nil {
		t.Fatalf("Failed to read audit directory: %v", err)
	}

	if len(files) == 0 {
		t.Fatal("No audit log files created")
	}

	// Read the log file and verify content
	logFile := filepath.Join(auditDir, files[0].Name())
	content, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatalf("Failed to read log file: %v", err)
	}

	contentStr := string(content)
	if !strings.Contains(contentStr, agent) {
		t.Errorf("LogToolCall() did not log agent name: %s", agent)
	}
	if !strings.Contains(contentStr, tool) {
		t.Errorf("LogToolCall() did not log tool name: %s", tool)
	}
	if !strings.Contains(contentStr, input) {
		t.Errorf("LogToolCall() did not log input: %s", input)
	}
	if !strings.Contains(contentStr, "call") {
		t.Errorf("LogToolCall() did not log action as 'call'")
	}
}

// TestAuditLoggerLogToolResult tests the LogToolResult method
func TestAuditLoggerLogToolResult(t *testing.T) {
	tmpDir := t.TempDir()
	teamName := "test-team"

	logger, err := NewAuditLogger(tmpDir, teamName)
	if err != nil {
		t.Fatalf("NewAuditLogger() error = %v", err)
	}
	defer logger.Close()

	// Log a successful tool result
	agent := "test-agent"
	tool := "bash"
	result := "hello world"

	logger.LogToolResult(agent, tool, result, false)

	// Verify the log file contains the result
	auditDir := filepath.Join(tmpDir, "audit")
	files, err := os.ReadDir(auditDir)
	if err != nil {
		t.Fatalf("Failed to read audit directory: %v", err)
	}

	if len(files) == 0 {
		t.Fatal("No audit log files created")
	}

	logFile := filepath.Join(auditDir, files[0].Name())
	content, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatalf("Failed to read log file: %v", err)
	}

	contentStr := string(content)
	if !strings.Contains(contentStr, result) {
		t.Errorf("LogToolResult() did not log result: %s", result)
	}
	if !strings.Contains(contentStr, "result") {
		t.Errorf("LogToolResult() did not log action as 'result'")
	}
}

// TestAuditLoggerLogToolResultError tests the LogToolResult method with error
func TestAuditLoggerLogToolResultError(t *testing.T) {
	tmpDir := t.TempDir()
	teamName := "test-team"

	logger, err := NewAuditLogger(tmpDir, teamName)
	if err != nil {
		t.Fatalf("NewAuditLogger() error = %v", err)
	}
	defer logger.Close()

	// Log an error result
	agent := "test-agent"
	tool := "bash"
	result := "error: command failed"

	logger.LogToolResult(agent, tool, result, true)

	// Verify the log file contains the error
	auditDir := filepath.Join(tmpDir, "audit")
	files, err := os.ReadDir(auditDir)
	if err != nil {
		t.Fatalf("Failed to read audit directory: %v", err)
	}

	if len(files) == 0 {
		t.Fatal("No audit log files created")
	}

	logFile := filepath.Join(auditDir, files[0].Name())
	content, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatalf("Failed to read log file: %v", err)
	}

	contentStr := string(content)
	if !strings.Contains(contentStr, "error") {
		t.Errorf("LogToolResult() did not log error field")
	}
}

// TestAuditLoggerClose tests the Close method
func TestAuditLoggerClose(t *testing.T) {
	tmpDir := t.TempDir()
	teamName := "test-team"

	logger, err := NewAuditLogger(tmpDir, teamName)
	if err != nil {
		t.Fatalf("NewAuditLogger() error = %v", err)
	}

	// Close the logger
	err = logger.Close()
	if err != nil {
		t.Errorf("Close() error = %v", err)
	}

	// Closing again should not error
	err = logger.Close()
	if err != nil {
		t.Errorf("Close() second call error = %v", err)
	}
}

// TestTruncate tests the truncate function
func TestTruncate(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		maxLen   int
		expected string
	}{
		{
			name:     "string shorter than maxLen",
			input:    "hello",
			maxLen:   10,
			expected: "hello",
		},
		{
			name:     "string equal to maxLen",
			input:    "hello",
			maxLen:   5,
			expected: "hello",
		},
		{
			name:     "string longer than maxLen",
			input:    "hello world this is a long string",
			maxLen:   10,
			expected:   "hello worl...[truncated]",
		},
		{
			name:     "empty string",
			input:    "",
			maxLen:   10,
			expected: "",
		},
		{
			name:     "maxLen zero",
			input:    "hello",
			maxLen:   0,
			expected:   "...[truncated]",
		},
		{
			name:     "maxLen negative",
			input:    "hello",
			maxLen:   -1,
			expected:   "...[truncated]",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := truncate(tt.input, tt.maxLen)
			if got != tt.expected {
				t.Errorf("truncate(%q, %d) = %q, want %q", tt.input, tt.maxLen, got, tt.expected)
			}
		})
	}
}

// TestSetDefaultAndGetDefault tests the SetDefault and GetDefault functions
func TestSetDefaultAndGetDefault(t *testing.T) {
	tmpDir := t.TempDir()
	teamName := "test-team"

	// Create a logger
	logger, err := NewAuditLogger(tmpDir, teamName)
	if err != nil {
		t.Fatalf("NewAuditLogger() error = %v", err)
	}
	defer logger.Close()

	// Set as default
	SetDefault(logger)

	// Get default
	defaultLogger := GetDefault()
	if defaultLogger != logger {
		t.Error("GetDefault() did not return the set logger")
	}

	// Set a new default
	newLogger, err := NewAuditLogger(tmpDir, "new-team")
	if err != nil {
		t.Fatalf("NewAuditLogger() error = %v", err)
	}
	defer newLogger.Close()

	SetDefault(newLogger)

	// Verify the new logger is returned
	defaultLogger = GetDefault()
	if defaultLogger != newLogger {
		t.Error("GetDefault() did not return the new logger")
	}
}

// TestLogToolCallGlobal tests the global LogToolCall function
func TestLogToolCallGlobal(t *testing.T) {
	tmpDir := t.TempDir()
	teamName := "test-team"

	// Create and set a logger
	logger, err := NewAuditLogger(tmpDir, teamName)
	if err != nil {
		t.Fatalf("NewAuditLogger() error = %v", err)
	}
	defer logger.Close()

	SetDefault(logger)

	// Call the global function
	agent := "test-agent"
	tool := "bash"
	input := "echo hello"

	LogToolCall(agent, tool, input)

	// Verify the log file was created
	auditDir := filepath.Join(tmpDir, "audit")
	files, err := os.ReadDir(auditDir)
	if err != nil {
		t.Fatalf("Failed to read audit directory: %v", err)
	}

	if len(files) == 0 {
		t.Fatal("No audit log files created")
	}

	logFile := filepath.Join(auditDir, files[0].Name())
	content, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatalf("Failed to read log file: %v", err)
	}

	contentStr := string(content)
	if !strings.Contains(contentStr, agent) {
		t.Errorf("LogToolCall() did not log agent name: %s", agent)
	}
}

// TestLogToolResultGlobal tests the global LogToolResult function
func TestLogToolResultGlobal(t *testing.T) {
	tmpDir := t.TempDir()
	teamName := "test-team"

	// Create and set a logger
	logger, err := NewAuditLogger(tmpDir, teamName)
	if err != nil {
		t.Fatalf("NewAuditLogger() error = %v", err)
	}
	defer logger.Close()

	SetDefault(logger)

	// Call the global function
	agent := "test-agent"
	tool := "bash"
	result := "hello world"

	LogToolResult(agent, tool, result, false)

	// Verify the log file was created
	auditDir := filepath.Join(tmpDir, "audit")
	files, err := os.ReadDir(auditDir)
	if err != nil {
		t.Fatalf("Failed to read audit directory: %v", err)
	}

	if len(files) == 0 {
		t.Fatal("No audit log files created")
	}

	logFile := filepath.Join(auditDir, files[0].Name())
	content, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatalf("Failed to read log file: %v", err)
	}

	contentStr := string(content)
	if !strings.Contains(contentStr, result) {
		t.Errorf("LogToolResult() did not log result: %s", result)
	}
}

// TestToolActionJSONSerialization tests that ToolAction can be serialized to JSON
func TestToolActionJSONSerialization(t *testing.T) {
	action := ToolAction{
		Timestamp: time.Now().Format(time.RFC3339Nano),
		Team:      "test-team",
		Agent:     "test-agent",
		Tool:      "bash",
		Action:    "call",
		Input:     "echo hello",
		Result:    "",
		Error:     "",
	}

	data, err := json.Marshal(action)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}

	// Verify the JSON contains expected fields
	jsonStr := string(data)
	if !strings.Contains(jsonStr, "test-team") {
		t.Errorf("JSON does not contain team name")
	}
	if !strings.Contains(jsonStr, "test-agent") {
		t.Errorf("JSON does not contain agent name")
	}
	if !strings.Contains(jsonStr, "bash") {
		t.Errorf("JSON does not contain tool name")
	}
	if !strings.Contains(jsonStr, "call") {
		t.Errorf("JSON does not contain action")
	}
}

// TestToolActionJSONSerializationWithError tests serialization with error field
func TestToolActionJSONSerializationWithError(t *testing.T) {
	action := ToolAction{
		Timestamp: time.Now().Format(time.RFC3339Nano),
		Team:      "test-team",
		Agent:     "test-agent",
		Tool:      "bash",
		Action:    "result",
		Input:     "",
		Result:    "",
		Error:     "command failed",
	}

	data, err := json.Marshal(action)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}

	jsonStr := string(data)
	if !strings.Contains(jsonStr, "command failed") {
		t.Errorf("JSON does not contain error message")
	}
}

// TestConcurrentLogToolCall tests concurrent logging
func TestConcurrentLogToolCall(t *testing.T) {
	tmpDir := t.TempDir()
	teamName := "test-team"

	logger, err := NewAuditLogger(tmpDir, teamName)
	if err != nil {
		t.Fatalf("NewAuditLogger() error = %v", err)
	}
	defer logger.Close()

	// Run multiple goroutines
	done := make(chan bool)
	for i := 0; i < 10; i++ {
		go func(id int) {
			agent := "agent-" + string(rune('0'+id))
			tool := "bash"
			input := "echo test-" + string(rune('0'+id))

			logger.LogToolCall(agent, tool, input)
			done <- true
		}(i)
	}

	// Wait for all goroutines to complete
	for i := 0; i < 10; i++ {
		<-done
	}

	// Verify all logs were written
	auditDir := filepath.Join(tmpDir, "audit")
	files, err := os.ReadDir(auditDir)
	if err != nil {
		t.Fatalf("Failed to read audit directory: %v", err)
	}

	if len(files) == 0 {
		t.Fatal("No audit log files created")
	}

	// Count log entries
	logFile := filepath.Join(auditDir, files[0].Name())
	content, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatalf("Failed to read log file: %v", err)
	}

	lines := strings.Split(strings.TrimSpace(string(content)), "\n")
	if len(lines) != 10 {
		t.Errorf("Expected 10 log entries, got %d", len(lines))
	}
}

// TestToolActionFields tests that ToolAction has all expected fields
func TestToolActionFields(t *testing.T) {
	action := ToolAction{
		Timestamp: "2024-01-01T00:00:00Z",
		Team:      "test-team",
		Agent:     "test-agent",
		Tool:      "bash",
		Action:    "call",
		Input:     "echo hello",
		Result:    "hello",
		Error:     "",
	}

	if action.Timestamp != "2024-01-01T00:00:00Z" {
		t.Errorf("Timestamp = %q, want %q", action.Timestamp, "2024-01-01T00:00:00Z")
	}
	if action.Team != "test-team" {
		t.Errorf("Team = %q, want %q", action.Team, "test-team")
	}
	if action.Agent != "test-agent" {
		t.Errorf("Agent = %q, want %q", action.Agent, "test-agent")
	}
	if action.Tool != "bash" {
		t.Errorf("Tool = %q, want %q", action.Tool, "bash")
	}
	if action.Action != "call" {
		t.Errorf("Action = %q, want %q", action.Action, "call")
	}
	if action.Input != "echo hello" {
		t.Errorf("Input = %q, want %q", action.Input, "echo hello")
	}
	if action.Result != "hello" {
		t.Errorf("Result = %q, want %q", action.Result, "hello")
	}
}
