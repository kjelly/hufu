package audit

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type AuditLogger struct {
	mu       sync.Mutex
	file     *os.File
	teamName string
}

type ToolAction struct {
	Timestamp string `json:"timestamp"`
	Team      string `json:"team"`
	Agent     string `json:"agent"`
	Tool      string `json:"tool"`
	Action    string `json:"action"`
	Input     string `json:"input,omitempty"`
	Result    string `json:"result,omitempty"`
	Error     string `json:"error,omitempty"`
}

var defaultLogger *AuditLogger
var defaultLoggerMu sync.Mutex

func NewAuditLogger(workspace, teamName string) (*AuditLogger, error) {
	auditDir := filepath.Join(workspace, "logs", "audit")
	if err := os.MkdirAll(auditDir, 0o755); err != nil {
		return nil, fmt.Errorf("failed to create audit directory: %w", err)
	}

	auditFile := filepath.Join(auditDir, fmt.Sprintf("audit-%s.jsonl", time.Now().Format("2006-01-02")))
	f, err := os.OpenFile(auditFile, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, fmt.Errorf("failed to open audit log: %w", err)
	}

	return &AuditLogger{
		file:     f,
		teamName: teamName,
	}, nil
}

func (l *AuditLogger) LogToolCall(agent, tool, input string) {
	l.log(ToolAction{
		Timestamp: time.Now().Format(time.RFC3339Nano),
		Team:      l.teamName,
		Agent:     agent,
		Tool:      tool,
		Action:    "call",
		Input:     truncate(input, 10000),
	})
}

func (l *AuditLogger) LogToolResult(agent, tool, result string, isError bool) {
	entry := ToolAction{
		Timestamp: time.Now().Format(time.RFC3339Nano),
		Team:      l.teamName,
		Agent:     agent,
		Tool:      tool,
		Action:    "result",
		Result:    truncate(result, 5000),
	}
	if isError {
		entry.Error = truncate(result, 5000)
		entry.Result = ""
	}
	l.log(entry)
}

func (l *AuditLogger) log(entry ToolAction) {
	data, err := json.Marshal(entry)
	if err != nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file != nil {
		l.file.Write(append(data, '\n'))
	}
}

func (l *AuditLogger) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file != nil {
		err := l.file.Close()
		l.file = nil
		return err
	}
	return nil
}

func truncate(s string, maxLen int) string {
	if maxLen <= 0 {
		return "...[truncated]"
	}
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "...[truncated]"
}

func SetDefault(logger *AuditLogger) {
	defaultLoggerMu.Lock()
	defer defaultLoggerMu.Unlock()
	if defaultLogger != nil {
		defaultLogger.Close()
	}
	defaultLogger = logger
}

func GetDefault() *AuditLogger {
	defaultLoggerMu.Lock()
	defer defaultLoggerMu.Unlock()
	return defaultLogger
}

func LogToolCall(agent, tool, input string) {
	if l := GetDefault(); l != nil {
		l.LogToolCall(agent, tool, input)
	}
}

func LogToolResult(agent, tool, result string, isError bool) {
	if l := GetDefault(); l != nil {
		l.LogToolResult(agent, tool, result, isError)
	}
}

func (l *AuditLogger) LogSSHConnection(agent, host, command string, exitCode int, durationMs int64) {
	l.log(ToolAction{
		Timestamp: time.Now().Format(time.RFC3339Nano),
		Team:      l.teamName,
		Agent:     agent,
		Tool:      "ssh",
		Action:    "ssh_connection",
		Input:     fmt.Sprintf("host=%s, command=%s", host, truncate(command, 500)),
		Result:    fmt.Sprintf("exit_code=%d, duration_ms=%d", exitCode, durationMs),
	})
}
