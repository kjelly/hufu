package audit

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"time"

	"github.com/anomalyco/hufu/internal/utils"
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
	defer func() { _ = logger.Close() }()

	// Verify the audit directory was created
	auditDir := filepath.Join(tmpDir, "logs", "audit")
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
	defer func() { _ = logger.Close() }()

	// Log a tool call
	agent := "test-agent"
	tool := "bash"
	input := "echo hello"

	logger.LogToolCall(agent, tool, input, "call-1")

	// Verify the log file was created and contains the entry
	auditDir := filepath.Join(tmpDir, "logs", "audit")
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
	defer func() { _ = logger.Close() }()

	// Log a successful tool result
	agent := "test-agent"
	tool := "bash"
	result := "hello world"

	logger.LogToolResult(agent, tool, result, false, "call-1")

	// Verify the log file contains the result
	auditDir := filepath.Join(tmpDir, "logs", "audit")
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
	defer func() { _ = logger.Close() }()

	// Log an error result
	agent := "test-agent"
	tool := "bash"
	result := "error: command failed"

	logger.LogToolResult(agent, tool, result, true, "call-1")

	// Verify the log file contains the error
	auditDir := filepath.Join(tmpDir, "logs", "audit")
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
			expected: "hello wor…",
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
			expected: "…",
		},
		{
			name:     "maxLen negative",
			input:    "hello",
			maxLen:   -1,
			expected: "…",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := utils.TruncateString(tt.input, tt.maxLen)
			if got != tt.expected {
				t.Errorf("utils.TruncateString(%q, %d) = %q, want %q", tt.input, tt.maxLen, got, tt.expected)
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
	defer func() { _ = logger.Close() }()

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
	defer func() { _ = newLogger.Close() }()

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
	defer func() { _ = logger.Close() }()

	SetDefault(logger)

	// Call the global function
	agent := "test-agent"
	tool := "bash"
	input := "echo hello"

	LogToolCall(agent, tool, input, "call-1")

	// Verify the log file was created
	auditDir := filepath.Join(tmpDir, "logs", "audit")
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
	defer func() { _ = logger.Close() }()

	SetDefault(logger)

	// Call the global function
	agent := "test-agent"
	tool := "bash"
	result := "hello world"

	LogToolResult(agent, tool, result, false, "call-1")

	// Verify the log file was created
	auditDir := filepath.Join(tmpDir, "logs", "audit")
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
	defer func() { _ = logger.Close() }()

	// Run multiple goroutines
	done := make(chan bool)
	for i := 0; i < 10; i++ {
		go func(id int) {
			agent := "agent-" + string(rune('0'+id))
			tool := "bash"
			input := "echo test-" + string(rune('0'+id))

			logger.LogToolCall(agent, tool, input, "call-"+string(rune('0'+id)))
			done <- true
		}(i)
	}

	// Wait for all goroutines to complete
	for i := 0; i < 10; i++ {
		<-done
	}

	// Verify all logs were written
	auditDir := filepath.Join(tmpDir, "logs", "audit")
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

// readLogEntries reads and parses all JSONL entries from the audit log in tmpDir.
func readLogEntries(t *testing.T, tmpDir string) []ToolAction {
	t.Helper()
	auditDir := filepath.Join(tmpDir, "logs", "audit")
	files, err := os.ReadDir(auditDir)
	if err != nil {
		t.Fatalf("Failed to read audit directory: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("No audit log files created")
	}
	content, err := os.ReadFile(filepath.Join(auditDir, files[0].Name()))
	if err != nil {
		t.Fatalf("Failed to read log file: %v", err)
	}
	var entries []ToolAction
	for _, line := range strings.Split(strings.TrimSpace(string(content)), "\n") {
		if line == "" {
			continue
		}
		var entry ToolAction
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("Failed to unmarshal log line %q: %v", line, err)
		}
		entries = append(entries, entry)
	}
	return entries
}

// TestLogEntryObservability verifies call_id, event type, and error-result
// fields across call/result/error variants.
func TestLogEntryObservability(t *testing.T) {
	tests := []struct {
		name       string
		log        func(l *AuditLogger)
		wantAction string
		wantEvent  string
		wantCallID string
		wantInput  string
		wantResult string
		wantError  string
	}{
		{
			name: "tool call carries call_id and tool_call event",
			log: func(l *AuditLogger) {
				l.LogToolCall("agent-a", "bash", "echo hi", "toolu_01")
			},
			wantAction: "call",
			wantEvent:  "tool_call",
			wantCallID: "toolu_01",
			wantInput:  "echo hi",
		},
		{
			name: "success result carries call_id and tool_result event",
			log: func(l *AuditLogger) {
				l.LogToolResult("agent-a", "bash", "hi", false, "toolu_01")
			},
			wantAction: "result",
			wantEvent:  "tool_result",
			wantCallID: "toolu_01",
			wantResult: "hi",
		},
		{
			name: "error result fills both result and error with tool_error event",
			log: func(l *AuditLogger) {
				l.LogToolResult("agent-a", "bash", "command failed: exit 1", true, "toolu_02")
			},
			wantAction: "result",
			wantEvent:  "tool_error",
			wantCallID: "toolu_02",
			wantResult: "command failed: exit 1",
			wantError:  "command failed: exit 1",
		},
		{
			name: "empty call_id is allowed and omitted",
			log: func(l *AuditLogger) {
				l.LogToolCall("agent-a", "bash", "echo hi", "")
			},
			wantAction: "call",
			wantEvent:  "tool_call",
			wantCallID: "",
			wantInput:  "echo hi",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tmpDir := t.TempDir()
			logger, err := NewAuditLogger(tmpDir, "test-team")
			if err != nil {
				t.Fatalf("NewAuditLogger() error = %v", err)
			}
			defer func() { _ = logger.Close() }()

			tt.log(logger)

			entries := readLogEntries(t, tmpDir)
			if len(entries) != 1 {
				t.Fatalf("Expected 1 log entry, got %d", len(entries))
			}
			entry := entries[0]

			if entry.Action != tt.wantAction {
				t.Errorf("Action = %q, want %q", entry.Action, tt.wantAction)
			}
			if entry.Event != tt.wantEvent {
				t.Errorf("Event = %q, want %q", entry.Event, tt.wantEvent)
			}
			if entry.CallID != tt.wantCallID {
				t.Errorf("CallID = %q, want %q", entry.CallID, tt.wantCallID)
			}
			if entry.Input != tt.wantInput {
				t.Errorf("Input = %q, want %q", entry.Input, tt.wantInput)
			}
			if entry.Result != tt.wantResult {
				t.Errorf("Result = %q, want %q", entry.Result, tt.wantResult)
			}
			if entry.Error != tt.wantError {
				t.Errorf("Error = %q, want %q", entry.Error, tt.wantError)
			}
		})
	}
}

// TestErrorResultFieldNotEmpty guards against error results rendering as
// blank for readers that only look at the .result field.
func TestErrorResultFieldNotEmpty(t *testing.T) {
	tmpDir := t.TempDir()
	logger, err := NewAuditLogger(tmpDir, "test-team")
	if err != nil {
		t.Fatalf("NewAuditLogger() error = %v", err)
	}
	defer func() { _ = logger.Close() }()

	logger.LogToolResult("agent-a", "bash", "boom", true, "toolu_err")

	entries := readLogEntries(t, tmpDir)
	if len(entries) != 1 {
		t.Fatalf("Expected 1 log entry, got %d", len(entries))
	}
	if entries[0].Result == "" {
		t.Error("error result wrote empty .result field; readers of .result would see blank")
	}
	if entries[0].Error == "" {
		t.Error("error result wrote empty .error field")
	}
	if entries[0].Result != entries[0].Error {
		t.Errorf("Result %q and Error %q should carry the same content", entries[0].Result, entries[0].Error)
	}
}

// TestCallIDOmittedWhenEmpty verifies the raw JSON omits call_id when empty
// (backward compatibility with old log consumers).
func TestCallIDOmittedWhenEmpty(t *testing.T) {
	data, err := json.Marshal(ToolAction{Action: "call"})
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	if strings.Contains(string(data), "call_id") {
		t.Errorf("empty call_id should be omitted from JSON, got %s", data)
	}

	data, err = json.Marshal(ToolAction{Action: "call", CallID: "toolu_01", Event: "tool_call"})
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	if !strings.Contains(string(data), `"call_id":"toolu_01"`) {
		t.Errorf("JSON missing call_id field, got %s", data)
	}
	if !strings.Contains(string(data), `"event":"tool_call"`) {
		t.Errorf("JSON missing event field, got %s", data)
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
