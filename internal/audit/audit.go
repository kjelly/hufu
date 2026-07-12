package audit

import (
	"encoding/json"
	"fmt"
	"os"

	"path/filepath"
	"sync"
	"time"

	"github.com/anomalyco/hufu/internal/utils"
)

type AuditLogger struct {
	mu       sync.Mutex
	file     *os.File
	teamName string
}

// Event type values for ToolAction.Event.
const (
	EventToolCall   = "tool_call"
	EventToolResult = "tool_result"
	EventToolError  = "tool_error"
	EventSSH        = "ssh_connection"
)

type ToolAction struct {
	Timestamp string `json:"timestamp"`
	Team      string `json:"team"`
	Agent     string `json:"agent"`
	Tool      string `json:"tool"`
	Action    string `json:"action"`
	Event     string `json:"event,omitempty"`
	CallID    string `json:"call_id,omitempty"`
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

// LogToolCall records a tool invocation. callID is the model-provided tool
// call ID used to correlate this entry with its matching result line; it may
// be empty when the caller has no ID available.
func (l *AuditLogger) LogToolCall(agent, tool, input, callID string) {
	l.log(ToolAction{
		Timestamp: time.Now().Format(time.RFC3339Nano),
		Team:      l.teamName,
		Agent:     agent,
		Tool:      tool,
		Action:    "call",
		Event:     EventToolCall,
		CallID:    callID,
		Input:     utils.TruncateString(input, 10000),
	})
}

// LogToolResult records a tool result. For error results the content is
// written to both the result and error fields so that readers that only look
// at .result still see the text; the event field distinguishes tool_error
// from tool_result.
func (l *AuditLogger) LogToolResult(agent, tool, result string, isError bool, callID string) {
	entry := ToolAction{
		Timestamp: time.Now().Format(time.RFC3339Nano),
		Team:      l.teamName,
		Agent:     agent,
		Tool:      tool,
		Action:    "result",
		Event:     EventToolResult,
		CallID:    callID,
		Result:    utils.TruncateString(result, 5000),
	}
	if isError {
		entry.Event = EventToolError
		entry.Error = utils.TruncateString(result, 5000)
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
		_, _ = l.file.Write(append(data, '\n'))
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

func SetDefault(logger *AuditLogger) {
	defaultLoggerMu.Lock()
	defer defaultLoggerMu.Unlock()
	if defaultLogger != nil {
		_ = defaultLogger.Close()
	}
	defaultLogger = logger
}

func GetDefault() *AuditLogger {
	defaultLoggerMu.Lock()
	defer defaultLoggerMu.Unlock()
	return defaultLogger
}

func LogToolCall(agent, tool, input, callID string) {
	if l := GetDefault(); l != nil {
		l.LogToolCall(agent, tool, input, callID)
	}
}

func LogToolResult(agent, tool, result string, isError bool, callID string) {
	if l := GetDefault(); l != nil {
		l.LogToolResult(agent, tool, result, isError, callID)
	}
}

func (l *AuditLogger) LogSSHConnection(agent, host, command string, exitCode int, durationMs int64) {
	l.log(ToolAction{
		Timestamp: time.Now().Format(time.RFC3339Nano),
		Team:      l.teamName,
		Agent:     agent,
		Tool:      "ssh",
		Action:    "ssh_connection",
		Event:     EventSSH,
		Input:     fmt.Sprintf("host=%s, command=%s", host, utils.TruncateString(command, 500)),
		Result:    fmt.Sprintf("exit_code=%d, duration_ms=%d", exitCode, durationMs),
	})
}
