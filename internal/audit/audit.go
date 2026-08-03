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
	redactor Redactor
}

// Redactor is the persistence-boundary secret redaction contract. It is kept
// local to audit to avoid coupling the audit package to a particular secret
// registry implementation.
type Redactor interface {
	RedactText(string) string
	RedactJSON([]byte) ([]byte, error)
}

// SetRedactor installs the process-local redactor used before audit entries
// are serialized. A nil value restores the legacy pattern-based redaction.
func (l *AuditLogger) SetRedactor(redactor Redactor) {
	if l == nil {
		return
	}
	l.mu.Lock()
	l.redactor = redactor
	l.mu.Unlock()
}

func (l *AuditLogger) redactText(value string) string {
	value = utils.RedactSecrets(value)
	l.mu.Lock()
	redactor := l.redactor
	l.mu.Unlock()
	if redactor != nil {
		value = redactor.RedactText(value)
	}
	return value
}

// Event type values for ToolAction.Event.
const (
	EventToolCall   = "tool_call"
	EventToolResult = "tool_result"
	EventToolError  = "tool_error"
	EventSSH        = "ssh_connection"
	EventWaitPoll   = "wait_poll"
	// Consent instrumentation: an interactive path-consent prompt can block a
	// tool for minutes (stdin lock + a human who has not noticed the prompt).
	// Paired start/resolved events make that wait attributable — without them
	// a long call→result gap in the audit log looks like a tool hang.
	EventConsentWaitStart   = "consent_wait_start"
	EventConsentResolved    = "consent_resolved"
	EventAcceptanceModified = "acceptance_contract_modified"
	EventAcceptanceRejected = "acceptance_contract_rejected"
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
		Input:     l.redactText(utils.TruncateString(input, 10000)),
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
		Result:    l.redactText(utils.TruncateString(result, 5000)),
	}
	if isError {
		entry.Event = EventToolError
		entry.Error = l.redactText(utils.TruncateString(result, 5000))
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

// LogConsentWaitStart records that a tool is now blocked on an interactive
// path-consent prompt.
func (l *AuditLogger) LogConsentWaitStart(agent, tool, path string) {
	l.log(ToolAction{
		Timestamp: time.Now().Format(time.RFC3339Nano),
		Team:      l.teamName,
		Agent:     agent,
		Tool:      tool,
		Action:    "consent",
		Event:     EventConsentWaitStart,
		Input:     utils.TruncateString(path, 500),
	})
}

// LogConsentResolved records the outcome of a consent prompt and how long the
// tool was blocked waiting for it.
func (l *AuditLogger) LogConsentResolved(agent, tool, path, outcome string, wait time.Duration) {
	l.log(ToolAction{
		Timestamp: time.Now().Format(time.RFC3339Nano),
		Team:      l.teamName,
		Agent:     agent,
		Tool:      tool,
		Action:    "consent",
		Event:     EventConsentResolved,
		Input:     utils.TruncateString(path, 500),
		Result:    fmt.Sprintf("outcome=%s, wait_ms=%d", outcome, wait.Milliseconds()),
	})
}

func LogConsentWaitStart(agent, tool, path string) {
	if l := GetDefault(); l != nil {
		l.LogConsentWaitStart(agent, tool, path)
	}
}

func LogConsentResolved(agent, tool, path, outcome string, wait time.Duration) {
	if l := GetDefault(); l != nil {
		l.LogConsentResolved(agent, tool, path, outcome, wait)
	}
}

// LogWaitPoll records one polling attempt of the wait_for tool so long waits
// remain attributable in the audit trail (a silent multi-minute gap between a
// call and its result is indistinguishable from a hang otherwise).
func (l *AuditLogger) LogWaitPoll(agent, command string, attempt, exitCode int) {
	l.log(ToolAction{
		Timestamp: time.Now().Format(time.RFC3339Nano),
		Team:      l.teamName,
		Agent:     agent,
		Tool:      "wait_for",
		Action:    "poll",
		Event:     EventWaitPoll,
		Input:     l.redactText(utils.TruncateString(command, 500)),
		Result:    fmt.Sprintf("attempt=%d, exit_code=%d", attempt, exitCode),
	})
}

func LogWaitPoll(agent, command string, attempt, exitCode int) {
	if l := GetDefault(); l != nil {
		l.LogWaitPoll(agent, command, attempt, exitCode)
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
		Input:     fmt.Sprintf("host=%s, command=%s", host, l.redactText(utils.TruncateString(command, 500))),
		Result:    fmt.Sprintf("exit_code=%d, duration_ms=%d", exitCode, durationMs),
	})
}

func (l *AuditLogger) LogAcceptanceChange(event, status, agent, oldState, oldSpecJSON, newState, newSpecJSON, reason string) {
	l.log(ToolAction{
		Timestamp: time.Now().Format(time.RFC3339Nano),
		Team:      l.teamName,
		Agent:     agent,
		Tool:      "coordinator",
		Action:    event,
		Event:     event,
		Input:     fmt.Sprintf("old_state=%s, old_spec=%s", oldState, oldSpecJSON),
		Result:    fmt.Sprintf("new_state=%s, new_spec=%s, status=%s, reason=%s", newState, newSpecJSON, status, reason),
	})
}

func (l *AuditLogger) LogAcceptanceModified(agent, oldSpecJSON, newSpecJSON, reason string) {
	l.LogAcceptanceChange(EventAcceptanceModified, "accepted", agent, "unknown", oldSpecJSON, "unknown", newSpecJSON, reason)
}

func (l *AuditLogger) LogAcceptanceRejected(agent, oldState, oldSpecJSON, newState, newSpecJSON, reason string) {
	l.LogAcceptanceChange(EventAcceptanceRejected, "rejected", agent, oldState, oldSpecJSON, newState, newSpecJSON, reason)
}

func LogAcceptanceChange(event, status, agent, oldState, oldSpecJSON, newState, newSpecJSON, reason string) {
	if l := GetDefault(); l != nil {
		l.LogAcceptanceChange(event, status, agent, oldState, oldSpecJSON, newState, newSpecJSON, reason)
	}
}

func LogAcceptanceModified(agent, oldSpecJSON, newSpecJSON, reason string) {
	if l := GetDefault(); l != nil {
		l.LogAcceptanceModified(agent, oldSpecJSON, newSpecJSON, reason)
	}
}

func LogAcceptanceRejected(agent, oldState, oldSpecJSON, newState, newSpecJSON, reason string) {
	if l := GetDefault(); l != nil {
		l.LogAcceptanceRejected(agent, oldState, oldSpecJSON, newState, newSpecJSON, reason)
	}
}
