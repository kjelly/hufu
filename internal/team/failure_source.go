package team

import "strings"

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
)

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

func isPermissionBlockedFailureDetail(detail string) bool {
	if detail == "" {
		return false
	}
	s := strings.ToLower(detail)
	return strings.Contains(s, "tool '") && strings.Contains(s, "is not permitted") ||
		strings.Contains(s, "user denied permission for tool") ||
		strings.Contains(s, "permission denied") ||
		strings.Contains(s, "guard rule")
}
