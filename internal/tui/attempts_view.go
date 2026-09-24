package tui

import (
	"fmt"
	"strings"
	"time"
)

// OperatorAttemptDetail is one worker attempt shown in a task's detail
// view. The host converts it from the canonical attempt projection; it holds
// no output, prompt, or tool content.
type OperatorAttemptDetail struct {
	TaskID       string
	Attempt      int
	Target       string
	Workspace    string
	Activity     string
	Status       string
	Duration     time.Duration
	Tokens       int
	TokensKnown  bool
	Fallbacks    int
	FailureClass string
}

// attemptDisplayLimit bounds the attempts the detail header shows, so a
// long-retried task cannot push the log viewport off screen.
const attemptDisplayLimit = 5

// renderAttemptLines renders a task's most recent attempts as plain text.
// States are spelled out, never shown as a spinner or color alone, so every
// theme (including e-paper) reads them the same way.
func (m Model) renderAttemptLines(taskID string) []string {
	var attempts []OperatorAttemptDetail
	for _, attempt := range m.operatorAttempts {
		if attempt.TaskID == taskID {
			attempts = append(attempts, attempt)
		}
	}
	var lines []string
	if hidden := len(attempts) - attemptDisplayLimit; hidden > 0 {
		lines = append(lines, m.styles.dim.Render(fmt.Sprintf("  (%d earlier attempt(s) not shown)", hidden)))
		attempts = attempts[hidden:]
	}
	for _, attempt := range attempts {
		duration := "unknown"
		if attempt.Duration > 0 {
			duration = attempt.Duration.Round(time.Second).String()
		}
		tokens := "tokens unknown"
		if attempt.TokensKnown {
			tokens = fmt.Sprintf("%d tokens", attempt.Tokens)
		}
		parts := []string{
			fmt.Sprintf("#%d", attempt.Attempt),
			sanitizeTerminalText(textOrUnknown(attempt.Target)),
			sanitizeTerminalText(textOrUnknown(attempt.Workspace)),
			sanitizeTerminalText(textOrUnknown(attempt.Activity)),
			duration, tokens,
			fmt.Sprintf("fallbacks %d", attempt.Fallbacks),
		}
		if attempt.FailureClass != "" {
			parts = append(parts, "last failure "+sanitizeTerminalText(attempt.FailureClass))
		}
		lines = append(lines, m.styles.dim.Render("  "+truncateLineCells(strings.Join(parts, " · "), max(m.width-2, 20))))
	}
	return lines
}

func textOrUnknown(value string) string {
	if strings.TrimSpace(value) == "" {
		return "unknown"
	}
	return value
}
