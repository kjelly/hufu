package audit

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newTestLogger(t *testing.T) (*AuditLogger, string) {
	t.Helper()
	dir := t.TempDir()
	logger, err := NewAuditLogger(dir, "test-team")
	if err != nil {
		t.Fatalf("NewAuditLogger() error = %v", err)
	}
	t.Cleanup(func() { _ = logger.Close() })
	return logger, dir
}

// readLogEntries reads and parses all JSONL entries from the audit log in dir.
func readLogEntries(t *testing.T, dir string) []ToolAction {
	t.Helper()
	content := readLogFile(t, dir)
	var entries []ToolAction
	for _, line := range strings.Split(strings.TrimSpace(content), "\n") {
		if line == "" {
			continue
		}
		var entry ToolAction
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("log line %q is not valid JSON: %v", line, err)
		}
		entries = append(entries, entry)
	}
	return entries
}

func readLogFile(t *testing.T, dir string) string {
	t.Helper()
	auditDir := filepath.Join(dir, "logs", "audit")
	files, err := os.ReadDir(auditDir)
	if err != nil {
		t.Fatalf("read audit directory: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("no audit log files created")
	}
	content, err := os.ReadFile(filepath.Join(auditDir, files[0].Name()))
	if err != nil {
		t.Fatalf("read log file: %v", err)
	}
	return string(content)
}

func TestNewAuditLogger(t *testing.T) {
	_, dir := newTestLogger(t)
	if _, err := os.Stat(filepath.Join(dir, "logs", "audit")); err != nil {
		t.Errorf("audit directory was not created: %v", err)
	}
	if _, err := NewAuditLogger("/root/nonexistent", "test-team"); err == nil {
		t.Error("NewAuditLogger() in an unwritable location returned no error")
	}
}

func TestAuditLoggerCloseIsIdempotent(t *testing.T) {
	logger, _ := newTestLogger(t)
	for i := range 2 {
		if err := logger.Close(); err != nil {
			t.Errorf("Close() call %d error = %v", i+1, err)
		}
	}
}

// TestLogEntryObservability verifies action, event, call_id, input, result and
// error fields across the call, result and error variants.
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
			name:       "tool call carries call_id and tool_call event",
			log:        func(l *AuditLogger) { l.LogToolCall("agent-a", "bash", "echo hi", "toolu_01") },
			wantAction: "call",
			wantEvent:  "tool_call",
			wantCallID: "toolu_01",
			wantInput:  "echo hi",
		},
		{
			name:       "success result carries call_id and tool_result event",
			log:        func(l *AuditLogger) { l.LogToolResult("agent-a", "bash", "hi", false, "toolu_01") },
			wantAction: "result",
			wantEvent:  "tool_result",
			wantCallID: "toolu_01",
			wantResult: "hi",
		},
		{
			// Readers that only look at .result must not see a blank error.
			name:       "error result fills both result and error with tool_error event",
			log:        func(l *AuditLogger) { l.LogToolResult("agent-a", "bash", "command failed: exit 1", true, "toolu_02") },
			wantAction: "result",
			wantEvent:  "tool_error",
			wantCallID: "toolu_02",
			wantResult: "command failed: exit 1",
			wantError:  "command failed: exit 1",
		},
		{
			name:       "empty call_id is allowed",
			log:        func(l *AuditLogger) { l.LogToolCall("agent-a", "bash", "echo hi", "") },
			wantAction: "call",
			wantEvent:  "tool_call",
			wantInput:  "echo hi",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			logger, dir := newTestLogger(t)
			tt.log(logger)
			entries := readLogEntries(t, dir)
			if len(entries) != 1 {
				t.Fatalf("entries = %d, want 1", len(entries))
			}
			got := entries[0]
			if got.Agent != "agent-a" || got.Tool != "bash" || got.Team != "test-team" {
				t.Errorf("identity = %q/%q/%q, want agent-a/bash/test-team", got.Agent, got.Tool, got.Team)
			}
			want := ToolAction{Action: tt.wantAction, Event: tt.wantEvent, CallID: tt.wantCallID, Input: tt.wantInput, Result: tt.wantResult, Error: tt.wantError}
			got.Timestamp, got.Agent, got.Tool, got.Team = "", "", "", ""
			if got != want {
				t.Errorf("entry = %+v, want %+v", got, want)
			}
		})
	}
}

func TestGlobalFunctionsUseTheDefaultLogger(t *testing.T) {
	logger, dir := newTestLogger(t)
	SetDefault(logger)
	if GetDefault() != logger {
		t.Fatal("GetDefault() did not return the logger passed to SetDefault")
	}
	LogToolCall("agent-a", "bash", "echo hello", "call-1")
	LogToolResult("agent-a", "bash", "hello world", false, "call-1")
	entries := readLogEntries(t, dir)
	if len(entries) != 2 || entries[0].Event != "tool_call" || entries[1].Event != "tool_result" {
		t.Fatalf("entries = %+v, want one tool_call followed by one tool_result", entries)
	}
	if entries[0].Agent != "agent-a" || entries[0].Tool != "bash" || entries[0].Input != "echo hello" || entries[0].CallID != "call-1" {
		t.Errorf("tool_call entry = %+v, want the arguments passed to LogToolCall", entries[0])
	}
	if entries[1].Agent != "agent-a" || entries[1].Tool != "bash" || entries[1].Result != "hello world" || entries[1].CallID != "call-1" {
		t.Errorf("tool_result entry = %+v, want the arguments passed to LogToolResult", entries[1])
	}

	replacement, _ := newTestLogger(t)
	SetDefault(replacement)
	if GetDefault() != replacement {
		t.Error("GetDefault() did not return the replacement logger")
	}
}

// TestAuditLoggerRedactsSecretsAcrossAllFields asserts the JSON-aware
// redaction pass scrubs secrets from every field, including those assembled
// via fmt.Sprintf (LogAcceptanceModified's embedded spec JSON) where a
// per-field text redactor would miss an escaped secret.
func TestAuditLoggerRedactsSecretsAcrossAllFields(t *testing.T) {
	logger, dir := newTestLogger(t)
	secret := "audit-secret-7f3c-9a1e"
	specJSON := `{"password":"` + secret + `","other":"ok"}`
	logger.LogAcceptanceModified("test-agent", specJSON, `{"password":"new"}`, "rotated "+secret)

	content := readLogFile(t, dir)
	if strings.Contains(content, secret) {
		t.Fatalf("audit log persisted secret: %s", content)
	}
	// readLogEntries fails if redaction broke the one-record-per-line framing.
	if entries := readLogEntries(t, dir); len(entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(entries))
	}
}

func TestConcurrentLogToolCall(t *testing.T) {
	logger, dir := newTestLogger(t)
	done := make(chan struct{})
	for i := range 10 {
		go func() {
			id := string(rune('0' + i))
			logger.LogToolCall("agent-"+id, "bash", "echo test-"+id, "call-"+id)
			done <- struct{}{}
		}()
	}
	for range 10 {
		<-done
	}
	if entries := readLogEntries(t, dir); len(entries) != 10 {
		t.Errorf("entries = %d, want 10", len(entries))
	}
}

func TestToolActionJSON(t *testing.T) {
	tests := []struct {
		name            string
		action          ToolAction
		wantContains    []string
		wantNotContains []string
	}{
		{
			name:         "call with identity fields",
			action:       ToolAction{Team: "test-team", Agent: "test-agent", Tool: "bash", Action: "call", Input: "echo hello"},
			wantContains: []string{`"team":"test-team"`, `"agent":"test-agent"`, `"tool":"bash"`, `"action":"call"`},
		},
		{
			name:         "error field is serialized",
			action:       ToolAction{Action: "result", Error: "command failed"},
			wantContains: []string{`"error":"command failed"`},
		},
		{
			// Old log consumers never saw call_id; keep it absent when empty.
			name:            "empty call_id is omitted",
			action:          ToolAction{Action: "call"},
			wantNotContains: []string{"call_id"},
		},
		{
			name:         "call_id and event are serialized when set",
			action:       ToolAction{Action: "call", CallID: "toolu_01", Event: "tool_call"},
			wantContains: []string{`"call_id":"toolu_01"`, `"event":"tool_call"`},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data, err := json.Marshal(tt.action)
			if err != nil {
				t.Fatalf("json.Marshal() error = %v", err)
			}
			for _, want := range tt.wantContains {
				if !strings.Contains(string(data), want) {
					t.Errorf("JSON %s missing %s", data, want)
				}
			}
			for _, unwanted := range tt.wantNotContains {
				if strings.Contains(string(data), unwanted) {
					t.Errorf("JSON %s must not contain %s", data, unwanted)
				}
			}
		})
	}
}

// Consent waits can block a tool for minutes; the paired start/resolved
// events must carry enough detail (path, outcome, wait duration) to make a
// long call→result gap attributable after the fact.
func TestConsentWaitEvents(t *testing.T) {
	tests := []struct {
		name       string
		log        func(l *AuditLogger)
		wantEvent  string
		wantResult []string
	}{
		{
			name:      "wait start",
			log:       func(l *AuditLogger) { l.LogConsentWaitStart("deployer", "sudo", "/etc/qemu/bridge.conf") },
			wantEvent: EventConsentWaitStart,
		},
		{
			name: "resolved with outcome and wait",
			log: func(l *AuditLogger) {
				l.LogConsentResolved("deployer", "sudo", "/etc/qemu/bridge.conf", "once", 613*time.Second)
			},
			wantEvent:  EventConsentResolved,
			wantResult: []string{"outcome=once", "wait_ms=613000"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			logger, dir := newTestLogger(t)
			tt.log(logger)
			entries := readLogEntries(t, dir)
			if len(entries) != 1 {
				t.Fatalf("entries = %d, want 1", len(entries))
			}
			e := entries[0]
			if e.Event != tt.wantEvent || e.Action != "consent" || e.Input != "/etc/qemu/bridge.conf" || e.Agent != "deployer" || e.Tool != "sudo" {
				t.Errorf("entry = %+v, want %s consent for deployer/sudo on /etc/qemu/bridge.conf", e, tt.wantEvent)
			}
			for _, want := range tt.wantResult {
				if !strings.Contains(e.Result, want) {
					t.Errorf("Result %q missing %q", e.Result, want)
				}
			}
		})
	}
}
