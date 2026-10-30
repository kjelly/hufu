package team

import (
	"errors"
	"strings"

	"charm.land/fantasy"
)

const (
	FailureSourceError                       = "error"
	FailureSourceTaskTimeout                 = "task_timeout"
	FailureSourceSigint                      = "sigint"
	FailureSourceContextCanceled             = "context_canceled"
	FailureSourceBudgetExceeded              = "budget_exceeded"
	FailureSourceMaxRoundsExceeded           = "max_rounds_exceeded"
	FailureSourceUserDeclined                = "user_declined"
	FailureSourceChatSessionFailed           = "chat_session_failed"
	FailureSourceTeamFailed                  = "team_failed"
	FailureSourceTeamContinuationFailed      = "team_continuation_failed"
	FailureSourceDirectAgentFailed           = "direct_agent_failed"
	FailureSourceSynthesisFailed             = "synthesis_failed"
	FailureSourceSynthesisContinuationFailed = "synthesis_continuation_failed"
	FailureSourceSegmentFailed               = "segment_failed"
	FailureSourceProviderModel               = "provider/model"
)

// providerModelFailure preserves the provider's original error while adding
// typed ownership metadata. In particular, it prevents a stale task snapshot
// (for example, the last successful grep call) from being mistaken for the
// source of a later provider-stream failure.
type providerModelFailure struct {
	err        error
	model      string
	provider   string
	statusCode int
	response   string
}

func (e *providerModelFailure) Error() string {
	if e == nil || e.err == nil {
		return "provider/model failure"
	}
	return e.err.Error()
}

func (e *providerModelFailure) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.err
}

func providerModelFailureFrom(err error) *providerModelFailure {
	if err == nil {
		return nil
	}
	var typed *providerModelFailure
	if errors.As(err, &typed) {
		return typed
	}
	return nil
}

// annotateProviderModelFailure wraps only Fantasy's provider-stream error
// type. Local tool errors remain ordinary execution failures and retain their
// normal tool attribution.
func annotateProviderModelFailure(err error, modelID string) error {
	if err == nil || providerModelFailureFrom(err) != nil {
		return err
	}
	var providerErr *fantasy.ProviderError
	if !errors.As(err, &providerErr) || providerErr == nil {
		return err
	}
	provider := ""
	if modelID != "" {
		provider, _, _ = strings.Cut(modelID, "/")
	}
	response := strings.TrimSpace(string(providerErr.ResponseBody))
	if response == strings.TrimSpace(providerErr.Message) {
		response = ""
	}
	return &providerModelFailure{
		err:        err,
		model:      modelID,
		provider:   provider,
		statusCode: providerErr.StatusCode,
		response:   response,
	}
}

// FailureSource* values are the stable labels written into structured failure
// detail, workspace artifacts, and logs. SegmentFailureSource maps the
// human-facing CLI error template for a segment into one of these stable
// labels so every failure path uses the same vocabulary.
var segmentFailureSourcePatterns = map[string]string{
	"chat session failed":                   FailureSourceChatSessionFailed,
	"team %q continuation failed":           FailureSourceTeamContinuationFailed,
	"team %q failed":                        FailureSourceTeamFailed,
	"direct agent @%s failed":               FailureSourceDirectAgentFailed,
	"synthesis continuation for @%s failed": FailureSourceSynthesisContinuationFailed,
	"synthesis for @%s failed":              FailureSourceSynthesisFailed,
}

var segmentFailureSourceOrder = []string{
	// Keep the most specific templates first so a broad "team %q failed"
	// pattern does not shadow the more precise continuation/synthesis cases.
	"chat session failed",
	"team %q continuation failed",
	"team %q failed",
	"direct agent @%s failed",
	"synthesis continuation for @%s failed",
	"synthesis for @%s failed",
}

// SegmentFailureSource maps a segment error template to a stable failure
// source string used in structured logs and workspace artifacts.
func SegmentFailureSource(kind string) string {
	for _, pattern := range segmentFailureSourceOrder {
		if strings.Contains(kind, pattern) {
			if source, ok := segmentFailureSourcePatterns[pattern]; ok {
				return source
			}
		}
	}
	return FailureSourceSegmentFailed
}

// attemptDenialFailure carries a native authorization decision separately
// from model/provider prose. The receipt remains the durable source evidence.
type attemptDenialFailure struct {
	err     error
	blocked bool
	safety  bool
}

func (e *attemptDenialFailure) Error() string { return e.err.Error() }
func (e *attemptDenialFailure) Unwrap() error { return e.err }

func withAttemptDenials(err error, dispositions []ToolExecutionDisposition) error {
	if err == nil {
		return nil
	}
	// A submitted blocked result already forbids replay. Preserve its own
	// continuation policy: a denied write did not mutate the environment.
	if workerReportedBlocked(err) {
		return &attemptDenialFailure{err: err}
	}
	denial := &attemptDenialFailure{err: err}
	if existing, ok := errors.AsType[*attemptDenialFailure](err); ok {
		denial.blocked, denial.safety = existing.blocked, existing.safety
	}
	for _, disposition := range dispositions {
		if disposition.ReasonCode == "path_out_of_scope" {
			denial.safety = true
		}
		if disposition.Executed {
			continue
		}
		if disposition.Kind == ToolExecutionGuardDenied {
			denial.blocked = true
		}
		switch disposition.ReasonCode {
		case "tool_permission_denied", "tool_authorization_denied":
			denial.blocked = true
		case "path_out_of_scope", "read_only_tool_denied", "read_only_shell_empty_command", "read_only_shell_redirect_denied", "read_only_shell_syntax_denied", "read_only_shell_malformed_syntax", "read_only_command_denied":
			denial.safety = true
		}
	}
	if denial.blocked {
		denial.err = withFailureClassOverride(err, FailurePolicy)
	}
	return denial
}

func isPermissionBlockedError(err error) bool {
	permission, ok := errors.AsType[*attemptDenialFailure](err)
	return ok && permission.blocked
}

func isPermissionBlockedFailureDetail(detail string) bool {
	for field := range strings.SplitSeq(detail, " | ") {
		field = strings.TrimSpace(field)
		if strings.HasPrefix(field, "error=") {
			break
		}
		switch field {
		case "permission_blocked=true":
			return true
		case "permission_blocked=false":
			return false
		}
	}
	// Compatibility only for historical details without the native marker.

	if detail == "" {
		return false
	}
	s := strings.ToLower(detail)
	return strings.Contains(s, "tool '") && strings.Contains(s, "is not permitted") ||
		strings.Contains(s, "user denied permission for tool") ||
		strings.Contains(s, "permission denied") ||
		strings.Contains(s, "guard rule")
}

// isSafetyDeniedError prevents retries after native path or read-only denials.
func isSafetyDeniedError(err error) bool {
	denial, ok := errors.AsType[*attemptDenialFailure](err)
	return ok && denial.safety
}

// onlyReadOnlyPolicyDenials permits the existing single grammar correction
// only for native read-only denials. Authorization and path consent are never
// bypassed by this protocol fallback.
func onlyReadOnlyPolicyDenials(dispositions []ToolExecutionDisposition) bool {
	if len(dispositions) == 0 {
		return false
	}
	for _, d := range dispositions {
		if d.Executed || d.Kind != ToolExecutionPolicyDenied {
			return false
		}
		switch d.ReasonCode {
		case "read_only_tool_denied", "read_only_shell_empty_command", "read_only_shell_redirect_denied", "read_only_shell_syntax_denied", "read_only_shell_malformed_syntax", "read_only_command_denied":
		default:
			return false
		}
	}
	return true
}
