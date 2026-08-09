package team

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/kjelly/hufu/internal/utils"
)

// FailureEventPayload is the self-contained diagnostic contract for a task
// failure event (§9). It deliberately contains bounded evidence rather than a
// copy of the task prompt/description.
type FailureEventPayload struct {
	TaskID           string           `json:"task_id" yaml:"task_id"`
	Phase            string           `json:"phase" yaml:"phase"`
	FailureClass     TaskFailureClass `json:"failure_class" yaml:"failure_class"`
	RetryDisposition RetryDisposition `json:"retry_disposition" yaml:"retry_disposition"`
	Command          string           `json:"command" yaml:"command"`
	WorkDir          string           `json:"work_dir" yaml:"work_dir"`
	Shell            string           `json:"shell" yaml:"shell"`
	ExitCode         *int             `json:"exit_code" yaml:"exit_code"`
	Stdout           string           `json:"stdout" yaml:"stdout"`
	Stderr           string           `json:"stderr" yaml:"stderr"`
	Fingerprint      string           `json:"fingerprint" yaml:"fingerprint"`
	Hint             string           `json:"hint" yaml:"hint"`
	Summary          string           `json:"summary" yaml:"summary"`
	FailedStepID     string           `json:"failed_step_id,omitempty" yaml:"failed_step_id,omitempty"`
	ReceiptID        string           `json:"receipt_id,omitempty" yaml:"receipt_id,omitempty"`
	FailureType      string           `json:"failure_type,omitempty" yaml:"failure_type,omitempty"`
}

// MarshalJSON makes direct serialization safe as well as the explicit
// renderer/export paths. This protects future event/report consumers that
// marshal a payload without first calling RedactedFailureEvent.
func (event FailureEventPayload) MarshalJSON() ([]byte, error) {
	type wire FailureEventPayload
	return json.Marshal(wire(*RedactedFailureEvent(&event)))
}

// RedactedFailureEvent returns a detached, bounded, and secret-masked copy
// suitable for every durable or user-facing surface. The canonical failure
// event must never depend on callers remembering to redact individual fields.
func RedactedFailureEvent(event *FailureEventPayload) *FailureEventPayload {
	if event == nil {
		return nil
	}
	copyEvent := cloneFailureEventPayload(event)
	copyEvent.TaskID = utils.TruncateString(utils.RedactSecrets(copyEvent.TaskID), 200)
	copyEvent.Phase = utils.TruncateString(utils.RedactSecrets(copyEvent.Phase), 100)
	copyEvent.Command = utils.TruncateString(utils.RedactSecrets(copyEvent.Command), 500)
	copyEvent.WorkDir = utils.TruncateString(utils.RedactSecrets(copyEvent.WorkDir), 500)
	copyEvent.Shell = utils.TruncateString(utils.RedactSecrets(copyEvent.Shell), 100)
	copyEvent.Stdout = utils.TruncateString(utils.RedactSecrets(copyEvent.Stdout), 2000)
	copyEvent.Stderr = utils.TruncateString(utils.RedactSecrets(copyEvent.Stderr), 2000)
	copyEvent.Fingerprint = utils.TruncateString(utils.RedactSecrets(copyEvent.Fingerprint), 200)
	copyEvent.Hint = utils.TruncateString(utils.RedactSecrets(copyEvent.Hint), 500)
	copyEvent.Summary = utils.TruncateString(utils.RedactSecrets(copyEvent.Summary), 500)
	copyEvent.FailedStepID = utils.TruncateString(utils.RedactSecrets(copyEvent.FailedStepID), 200)
	copyEvent.ReceiptID = utils.TruncateString(utils.RedactSecrets(copyEvent.ReceiptID), 200)
	copyEvent.FailureType = utils.TruncateString(utils.RedactSecrets(copyEvent.FailureType), 100)
	return copyEvent
}

// RenderFailureText is the canonical bounded human-readable representation
// used by terminal output, TUI logs, workspace status, and task records.
// Keep this renderer independent of any UI package so every surface exposes
// the same evidence and never falls back to the unbounded task description.
func RenderFailureText(event *FailureEventPayload) string {
	event = RedactedFailureEvent(event)
	if event == nil {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "failure task_id=%s phase=%s class=%s disposition=%s\n",
		event.TaskID, event.Phase, event.FailureClass, event.RetryDisposition)
	// The summary carries the actual cause, so it goes directly under the
	// header. Every surface that collapses this block into one line truncates
	// it, and with the summary last the cause was the first thing cut: a real
	// run displayed "command: wait_for | work_dir: /home/u…" while the reason —
	// an exhausted attempt budget — appeared nowhere in the CLI output at all.
	// The fields below are supporting detail and can afford to be cut.
	fmt.Fprintf(&b, "summary: %s\n", event.Summary)
	if event.FailedStepID != "" {
		fmt.Fprintf(&b, "failed_step_id: %s\n", event.FailedStepID)
	}
	if event.ReceiptID != "" {
		fmt.Fprintf(&b, "receipt_id: %s\n", event.ReceiptID)
	}
	if event.FailureType != "" {
		fmt.Fprintf(&b, "failure_type: %s\n", event.FailureType)
	}
	fmt.Fprintf(&b, "command: %s\n", event.Command)
	fmt.Fprintf(&b, "work_dir: %s\n", event.WorkDir)
	fmt.Fprintf(&b, "shell: %s\n", event.Shell)
	if event.ExitCode == nil {
		b.WriteString("exit_code: null\n")
	} else {
		fmt.Fprintf(&b, "exit_code: %d\n", *event.ExitCode)
	}
	fmt.Fprintf(&b, "stdout: %s\n", event.Stdout)
	fmt.Fprintf(&b, "stderr: %s\n", event.Stderr)
	fmt.Fprintf(&b, "fingerprint: %s\n", event.Fingerprint)
	fmt.Fprintf(&b, "hint: %s", event.Hint)
	return b.String()
}

// RenderFailureMarkdown embeds the canonical text renderer in a fenced block
// for reports and task markdown files. The payload, not markdown formatting,
// remains the source of truth.
func RenderFailureMarkdown(event *FailureEventPayload) string {
	text := RenderFailureText(event)
	if text == "" {
		return ""
	}
	return "```text\n" + text + "\n```"
}

// FailureDisplayText is the canonical failure projection for a failed
// TodoItem. Normal task details are not failures and must not be rendered as
// synthetic failure events merely because they are present in Detail.
func FailureDisplayText(item *TodoItem) string {
	if item == nil || !isFailureTaskStatus(item.Status) {
		return ""
	}
	if item.FailureEvent != nil {
		return RenderFailureText(item.FailureEvent)
	}
	detail := strings.TrimSpace(item.Detail)
	if detail == "" && item.TypedResult != nil {
		detail = strings.TrimSpace(item.TypedResult.Summary)
	}
	if detail == "" {
		return ""
	}
	return RenderFailureText(&FailureEventPayload{
		TaskID:           item.ID,
		Phase:            "legacy",
		RetryDisposition: RetryNone,
		Summary:          utils.TruncateString(detail, 500),
	})
}

func isFailureTaskStatus(status TaskStatus) bool {
	return status == TaskError || status == TaskBlocked || status == TaskProtocolIncomplete
}

// TaskDetailDisplayText is the bounded, redacted projection for ordinary task
// detail. It deliberately has no failure label or synthetic class metadata.
func TaskDetailDisplayText(item *TodoItem) string {
	if item == nil {
		return ""
	}
	detail := strings.TrimSpace(item.Detail)
	if detail == "" && item.TypedResult != nil {
		detail = strings.TrimSpace(item.TypedResult.Summary)
	}
	return utils.TruncateString(utils.RedactSecrets(detail), 500)
}

// FailureEventsFromTodos returns detached failure payloads in todo order.
func FailureEventsFromTodos(items []*TodoItem) []FailureEventPayload {
	var events []FailureEventPayload
	for _, item := range items {
		if item == nil || item.FailureEvent == nil {
			continue
		}
		events = append(events, *RedactedFailureEvent(item.FailureEvent))
	}
	return events
}

func cloneFailureEventPayload(event *FailureEventPayload) *FailureEventPayload {
	if event == nil {
		return nil
	}
	copyEvent := *event
	if event.ExitCode != nil {
		code := *event.ExitCode
		copyEvent.ExitCode = &code
	}
	return &copyEvent
}

func failurePhase(class TaskFailureClass, item *TodoItem) string {
	if class == FailureVerify || (item != nil && item.VerifyResult != nil) {
		return "verification"
	}
	switch class {
	case FailureContract, FailureEnvironment:
		return "preflight"
	case FailureProtocol:
		return "protocol"
	case FailurePolicy, FailureCancelled:
		return "coordination"
	default:
		return "execution"
	}
}

func (c *Coordinator) failureEventForItem(item *TodoItem, class TaskFailureClass, disposition RetryDisposition, detail string, fingerprint FailureFingerprint, taskID string) *FailureEventPayload {
	if item == nil {
		item = &TodoItem{ID: taskID}
	}
	event := &FailureEventPayload{
		TaskID:           taskID,
		Phase:            failurePhase(class, item),
		FailureClass:     class,
		RetryDisposition: disposition,
		Command:          item.LastOperation,
		WorkDir:          c.verificationWorkDir(),
		Shell:            "sh",
		Fingerprint:      fingerprint.Digest,
		Hint:             utils.TruncateString(localFailureHint(detail), 500),
		Summary:          utils.TruncateString(detail, 500),
	}
	if c != nil && c.session != nil && c.session.Config.Shell != "" {
		event.Shell = c.session.Config.Shell
	}
	if event.Command == "" && c != nil {
		event.Command = c.GetCurrentTool()
	}
	if item.VerifyResult != nil {
		result := item.VerifyResult
		event.Command = result.Command
		event.WorkDir = result.WorkDir
		code := result.ExitCode
		event.ExitCode = &code
		event.Stdout = utils.TruncateString(result.Stdout, 2000)
		event.Stderr = utils.TruncateString(result.Stderr, 2000)
		if result.WeakReason != "" {
			event.Hint = utils.TruncateString(result.WeakReason, 500)
		}
	} else if c != nil {
		attempt := c.currentTaskAttempt(taskID)
		if receipt, ok := c.executionStepReceiptRegistry().FirstFailure(taskID, attempt); ok {
			event.FailedStepID = receipt.StepID
			event.ReceiptID = receipt.ID
			failureType := receipt.FailureClass
			if failureType == "" && receipt.ValidatorVerdict == "fail" {
				failureType = "validation"
			} else if failureType == "" && receipt.PolicyVerdict != "" && receipt.PolicyVerdict != "allowed" {
				failureType = "policy"
			}
			event.FailureType = normalizedExecutionFailureClass(failureType)
			event.Phase = event.FailureType
			event.Command = receipt.Tool
			code := receipt.ExitCode
			event.ExitCode = &code
			event.Stdout = utils.TruncateString(receipt.Stdout, 2000)
			event.Stderr = utils.TruncateString(receipt.Stderr, 2000)
		}
	}
	return RedactedFailureEvent(event)
}

func failureEventPayloadMap(event *FailureEventPayload) map[string]interface{} {
	event = RedactedFailureEvent(event)
	if event == nil {
		return nil
	}
	return map[string]interface{}{
		"task_id":           event.TaskID,
		"phase":             event.Phase,
		"failure_class":     event.FailureClass,
		"retry_disposition": event.RetryDisposition,
		"command":           event.Command,
		"work_dir":          event.WorkDir,
		"shell":             event.Shell,
		"exit_code":         event.ExitCode,
		"stdout":            event.Stdout,
		"stderr":            event.Stderr,
		"fingerprint":       event.Fingerprint,
		"hint":              event.Hint,
		"summary":           event.Summary,
		"failed_step_id":    event.FailedStepID,
		"receipt_id":        event.ReceiptID,
		"failure_type":      event.FailureType,
		"failure_event":     event,
	}
}

var failureEventFieldNames = []string{
	"task_id", "phase", "failure_class", "retry_disposition", "command", "work_dir", "shell",
	"exit_code", "stdout", "stderr", "fingerprint", "hint", "summary", "failed_step_id", "receipt_id", "failure_type",
}

// mergeFailureEventJSON applies only fields present in the event payload.
// This is important for schema evolution: an omitted field is not an explicit
// empty value and must not erase evidence restored from an earlier event.
func mergeFailureEventJSON(existing *FailureEventPayload, payload json.RawMessage) (*FailureEventPayload, bool) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(payload, &top); err != nil {
		return existing, false
	}
	fields := top
	if nested, ok := top["failure_event"]; ok {
		var decoded map[string]json.RawMessage
		if err := json.Unmarshal(nested, &decoded); err != nil {
			return existing, false
		}
		fields = decoded
	}
	found := false
	for _, name := range failureEventFieldNames {
		if _, ok := fields[name]; ok {
			found = true
			break
		}
	}
	if !found {
		return existing, false
	}
	result := cloneFailureEventPayload(existing)
	if result == nil {
		result = &FailureEventPayload{}
	}
	apply := func(name string, target interface{}) {
		if raw, ok := fields[name]; ok {
			_ = json.Unmarshal(raw, target)
		}
	}
	apply("task_id", &result.TaskID)
	apply("phase", &result.Phase)
	apply("failure_class", &result.FailureClass)
	apply("retry_disposition", &result.RetryDisposition)
	apply("command", &result.Command)
	apply("work_dir", &result.WorkDir)
	apply("shell", &result.Shell)
	apply("exit_code", &result.ExitCode)
	apply("stdout", &result.Stdout)
	apply("stderr", &result.Stderr)
	apply("fingerprint", &result.Fingerprint)
	apply("hint", &result.Hint)
	apply("summary", &result.Summary)
	apply("failed_step_id", &result.FailedStepID)
	apply("receipt_id", &result.ReceiptID)
	apply("failure_type", &result.FailureType)
	return RedactedFailureEvent(result), true
}

func failureSummary(item *TodoItem) string {
	if item == nil {
		return ""
	}
	if item.FailureEvent != nil && strings.TrimSpace(item.FailureEvent.Summary) != "" {
		return item.FailureEvent.Summary
	}
	return utils.TruncateString(item.Detail, 500)
}
