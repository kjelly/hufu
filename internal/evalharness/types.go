// Package evalharness implements the offline deterministic workflow
// regression harness described in docs/tmp/now/06-workflow-regression-eval-harness.md.
// It runs a real team.Coordinator end-to-end against a scripted
// OpenAI-compatible provider fake and asserts on the resulting RunResult,
// never against model output quality.
package evalharness

import "time"

// SuiteFixture is one YAML file under evals/<suite>/ declaring a named group
// of deterministic cases that share a team.
type SuiteFixture struct {
	Version int           `yaml:"version"`
	Name    string        `yaml:"name"`
	Team    string        `yaml:"team"` // path to a bundled team directory, relative to this file
	Mode    string        `yaml:"mode"` // must be "deterministic" in v1
	Cases   []CaseFixture `yaml:"cases"`

	// Path is the absolute path this fixture was loaded from. It is not part
	// of the YAML; Loader sets it so error messages and relative resolution
	// (Team, ProviderFixture) can point back at the file on disk.
	Path string `yaml:"-"`
}

// CaseFixture is one deterministic scenario within a SuiteFixture.
type CaseFixture struct {
	ID              string     `yaml:"id"`
	Prompt          string     `yaml:"prompt"`
	ProviderFixture string     `yaml:"provider-fixture"` // path to a ProviderFixture JSON file, relative to the SuiteFixture file
	Expect          ExpectSpec `yaml:"expect"`
	// DecisionProfileOverride, when set, is applied via
	// Coordinator.SetDecisionProfile before Run -- the run-scoped top layer
	// of the decision-profile precedence chain (normally set from
	// --decision-profile). Empty means "do not override": the team's own
	// team.yaml decision.default-profile (or the built-in "off" default)
	// applies unmodified.
	DecisionProfileOverride string `yaml:"decision-profile-override,omitempty"`
}

// ExpectSpec is the subset of RunResult dimensions a case asserts on. A zero
// value field (empty string / nil) means "do not check this dimension" --
// see assert.go.
type ExpectSpec struct {
	RunOutcome string       `yaml:"run-outcome"`
	Acceptance string       `yaml:"acceptance"`
	TaskCount  *int         `yaml:"task-count"`
	Tasks      []TaskExpect `yaml:"tasks"`
	Events     EventsExpect `yaml:"events"`
	// ExecutionTargetFrozen, when true, asserts that every StatusEvent
	// carrying a non-empty ExecutionTarget across the whole run reports the
	// SAME value -- the resolved backend/model must be admitted once per
	// task occurrence and never re-resolved on retry (see
	// internal/team/services.go's frozenTaskOccurrenceModel).
	ExecutionTargetFrozen *bool `yaml:"execution-target-frozen,omitempty"`
}

// TaskExpect asserts on one task by position: Tasks[i] in the fixture is
// matched against the i-th item of TaskTracker().TodoList().Items() in
// creation order. Task IDs are never compared -- a durable TodoItem.ID is a
// deterministic sequence counter ("1", "2", ...), not semantically
// meaningful on its own, so position is the stable, opaque-ID-free join key.
// A zero value field means "do not check this dimension".
type TaskExpect struct {
	Agent        string `yaml:"agent"`
	Status       string `yaml:"status"`
	FailureClass string `yaml:"failure-class"`
}

// EventsExpect names the StatusEvent.Type values a case checks for.
type EventsExpect struct {
	// Required lists types that must have been reported at least once,
	// in no particular order.
	Required []string `yaml:"required"`
	// Order lists types that must appear, in this relative order, as a
	// subsequence of the full event stream (other event types may appear
	// interleaved between them; cardinality beyond "at least once" and
	// non-listed fields are never compared -- see §5 "Event").
	Order []string `yaml:"order"`
}

// ProviderFixture is the scripted response program for one case's model
// driver (see docs/tmp/now/06-workflow-regression-eval-harness.md §4.1).
type ProviderFixture struct {
	Steps []ProviderStep `json:"steps"`
}

// ProviderStep is one scripted chat-completion reply. A step with no Match
// is consumed strictly in arrival order; a step with Match is consulted
// first, against every unconsumed request, matched by substring against the
// raw request body. Exactly one of Content or ToolCall must be set: Content
// answers with plain assistant text (finish_reason: stop); ToolCall answers
// with a single tool call (finish_reason: tool_calls), e.g. the coordinator's
// `agent` delegation call, a worker's `submit_result`, or the coordinator's
// closing `finish`.
type ProviderStep struct {
	Match    *StepMatch    `json:"match,omitempty"`
	Content  string        `json:"content,omitempty"`
	ToolCall *ToolCallStep `json:"tool_call,omitempty"`
}

// ToolCallStep is one scripted tool call. Arguments is the exact JSON object
// text the model would have emitted as the call's raw argument string (it is
// embedded verbatim as the tool call's `function.arguments` string, not
// re-encoded), so it must already match the target tool's schema -- e.g.
// `{"tasks":[{"agent":"worker","goal":"..."}]}` for the coordinator's `agent`
// tool, `{"status":"success","summary":"..."}` for a worker's
// `submit_result`, or `{"response":"..."}` for the coordinator's `finish`.
type ToolCallStep struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// StepMatch selects which incoming chat-completion request a ProviderStep
// answers. Exactly one of its fields is set.
type StepMatch struct {
	Contains string `json:"contains,omitempty"`
}

// EvalFinding is one failed assertion dimension for a case.
type EvalFinding struct {
	Dimension string
	Expected  string
	Actual    string
}

// EvalMetrics is non-assertion telemetry about how a case ran.
type EvalMetrics struct {
	Duration time.Duration
	// RunID is the completed run's RunID with its opaque timestamp+random
	// suffix redacted (see normalizeOpaqueID), so two runs of the same
	// deterministic case produce byte-identical JSON reports.
	RunID string
}

// EvalCaseResult is the canonical per-case result (§6 of the plan).
type EvalCaseResult struct {
	CaseID     string
	Passed     bool
	RunOutcome string
	Findings   []EvalFinding
	Metrics    EvalMetrics
}

// EvalSuiteResult aggregates every case result loaded from one SuiteFixture.
type EvalSuiteResult struct {
	SuiteName string
	Cases     []EvalCaseResult
}

// Passed reports whether every case in the suite passed.
func (s EvalSuiteResult) Passed() bool {
	for _, c := range s.Cases {
		if !c.Passed {
			return false
		}
	}
	return true
}
