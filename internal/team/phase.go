package team

import (
	"encoding/json"
	"regexp"
	"strings"
)

// Phase identifies the current state of a workflow execution.
type Phase string

const (
	PhaseInit    Phase = "INIT"
	PhasePrepare Phase = "PREPARE"
	PhaseAudit   Phase = "AUDIT"
	PhaseExecute Phase = "EXECUTE"
	PhaseVerify  Phase = "VERIFY"
	PhaseDone    Phase = "DONE"
	PhaseFailed  Phase = "FAILED"
)

// PhaseStatus indicates the result of a single phase.
type PhaseStatus string

const (
	PhaseStatusSuccess PhaseStatus = "SUCCESS"
	PhaseStatusFailure PhaseStatus = "FAILURE"
	PhaseStatusBlocked PhaseStatus = "BLOCKED"
)

// ExecutionError represents a structured failure model.
type ExecutionError struct {
	Phase     Phase    `json:"phase"`
	Component string   `json:"component"`
	Category  string   `json:"category"`
	Source    string   `json:"source"`
	Message   string   `json:"message"`
	Cause     string   `json:"cause,omitempty"`
	Evidence  []string `json:"evidence,omitempty"`
	Retryable bool     `json:"retryable"`
}

// PhaseResult represents the structured outcome of a workflow phase.
type PhaseResult struct {
	Status   PhaseStatus      `json:"status"`
	Summary  string           `json:"summary"`
	Evidence []ArtifactRef    `json:"evidence,omitempty"`
	Errors   []ExecutionError `json:"errors,omitempty"`
}

// allowedTransitions defines the valid workflow state machine transitions.
var allowedTransitions = map[Phase][]Phase{
	PhaseInit:    {PhasePrepare},
	PhasePrepare: {PhaseAudit, PhaseFailed},
	PhaseAudit:   {PhaseExecute, PhaseFailed},
	PhaseExecute: {PhaseVerify, PhaseFailed},
	PhaseVerify:  {PhaseDone, PhaseFailed},
}

// IsValidTransition returns true if the transition from `from` to `to` is allowed.
func IsValidTransition(from, to Phase) bool {
	valid, ok := allowedTransitions[from]
	if !ok {
		return false
	}
	for _, p := range valid {
		if p == to {
			return true
		}
	}
	return false
}

const (
	CategoryInvalidInput     = "INVALID_INPUT"
	CategoryPolicyViolation  = "POLICY_VIOLATION"
	CategoryProviderFailure  = "PROVIDER_FAILURE"
	CategoryValidationFailed = "VALIDATION_FAILURE"
	CategoryToolFailure      = "TOOL_FAILURE"
	CategoryEnvironment      = "ENVIRONMENT_FAILURE"
	CategoryTimeout          = "TIMEOUT"
	CategoryInternalError    = "INTERNAL_ERROR"
)

// FailureSignature provides a stable identity for deduplication and retry limits.
type FailureSignature struct {
	Phase     Phase  `json:"phase"`
	Component string `json:"component"`
	Category  string `json:"category"`
	Source    string `json:"source"`
	Cause     string `json:"cause"`
}

func (f FailureSignature) String() string {
	b, _ := json.Marshal(f)
	return string(b)
}

// normalizeCause canonicalizes whitespace, casing, and transient IDs.
func normalizeCause(cause string) string {
	s := strings.ToLower(strings.TrimSpace(cause))
	s = regexp.MustCompile(`\s+`).ReplaceAllString(s, " ")
	s = regexp.MustCompile(`[0-9a-f]{8,}`).ReplaceAllString(s, "<id>")
	return s
}

// Signature generates the stable signature from an ExecutionError.
func (e ExecutionError) Signature() FailureSignature {
	source := e.Source
	if source == "" {
		source = e.Component
	}
	return FailureSignature{
		Phase:     e.Phase,
		Component: e.Component,
		Category:  e.Category,
		Source:    source,
		Cause:     normalizeCause(e.Cause),
	}
}

// RetryState tracks retry attempts by signature (JSON safe).
type RetryState struct {
	Attempts map[string]int `json:"attempts,omitempty"`
}

// NewRetryState creates a new retry state tracker.
func NewRetryState() *RetryState {
	return &RetryState{
		Attempts: make(map[string]int),
	}
}

// RecordFailure records a failure and returns whether it can be retried.
func (s *RetryState) RecordFailure(e ExecutionError, maxAttempts int) bool {
	if !e.Retryable || maxAttempts <= 0 {
		return false
	}
	if s.Attempts == nil {
		s.Attempts = make(map[string]int)
	}
	sigKey := e.Signature().String()
	s.Attempts[sigKey]++
	return s.Attempts[sigKey] <= maxAttempts
}
