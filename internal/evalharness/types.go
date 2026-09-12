// Package evalharness implements the offline deterministic workflow
// regression harness described in docs/archive/implementation-plans/workflow-regression-eval-harness.md.
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
	// SeedExecutionPolicySnapshot materializes the coordinator's current,
	// dynamic execution policy into a seeded session and its event journal
	// before restore. This models a checkpoint produced by a prior run against
	// the same provider endpoint; httptest chooses a new endpoint per case, so
	// a valid snapshot (whose identity hash covers that endpoint) cannot be
	// stored as static fixture text.
	SeedExecutionPolicySnapshot bool `yaml:"seed-execution-policy-snapshot,omitempty"`
	// PriorRunDecisionAdmissionDigests supplies the historical off-profile
	// admission digest for each restored Todo ID. The digest intentionally
	// remains fixture evidence: recomputing it from live code would hide a
	// compatibility regression in the very contract this case exercises.
	PriorRunDecisionAdmissionDigests map[string]string `yaml:"prior-run-decision-admission-digests,omitempty"`
	// SeedMemoryPolicy records the team's configured learning policy as the
	// active canonical policy before Coordinator construction. NewCoordinator
	// deliberately loads only an adopted policy from context.sqlite, so team
	// YAML alone is not sufficient runtime arrangement for memory evals.
	SeedMemoryPolicy bool `yaml:"seed-memory-policy,omitempty"`
	// WorkspaceFiles seeds files into the case's ephemeral workspace before
	// the run starts, keyed by path relative to the workspace root, e.g. a
	// fan_out source manifest a task's tool_call references by a
	// workspace-relative path. Written verbatim (already-formatted content,
	// no templating).
	WorkspaceFiles map[string]string `yaml:"workspace-files,omitempty"`
	// ContextItems seeds confirmed shared-persistent context into the case's
	// canonical SQLite store before coordinator construction. The harness owns
	// project/team scoping so fixtures cannot accidentally seed an item that the
	// real runtime would never be allowed to retrieve.
	ContextItems []ContextItemFixture `yaml:"context-items,omitempty"`
}

// ContextItemFixture is the minimal stable surface needed to arrange a
// deterministic canonical-memory retrieval. Runtime-owned defaults supply
// kind, authority, trust, priority, confidence, lifecycle, and scope.
type ContextItemFixture struct {
	ID       string `yaml:"id"`
	Content  string `yaml:"content"`
	MustKeep bool   `yaml:"must-keep,omitempty"`
}

// ExpectSpec is the subset of RunResult dimensions a case asserts on. A zero
// value field (empty string / nil) means "do not check this dimension" --
// see assert.go.
type ExpectSpec struct {
	RunOutcome        string       `yaml:"run-outcome"`
	StopReason        string       `yaml:"stop-reason"`
	Acceptance        string       `yaml:"acceptance"`
	AuditVerdict      string       `yaml:"audit-verdict,omitempty"`
	TerminalLifecycle *bool        `yaml:"terminal-lifecycle-confirmed,omitempty"`
	TaskCount         *int         `yaml:"task-count"`
	Tasks             []TaskExpect `yaml:"tasks"`
	Events            EventsExpect `yaml:"events"`
	// DurableEvents asserts against the coordinator's append-only RunEvent
	// journal. Status Events above remain the live reporter surface; recovery
	// and compatibility boundaries such as execution_target_migrated are
	// durable-only and must be checked at their canonical source.
	DurableEvents EventsExpect `yaml:"durable-events"`
	// MemoryAggregates asserts durable post-run learning projections by
	// reopening context.sqlite after Coordinator.Run returns. Pointer fields
	// distinguish an explicit expected zero from an omitted assertion.
	MemoryAggregates []MemoryAggregateExpect `yaml:"memory-aggregates,omitempty"`
	// ExecutionTargetFrozen, when true, asserts that every StatusEvent
	// carrying a non-empty ExecutionTarget across the whole run reports the
	// SAME value -- the resolved backend/model must be admitted once per
	// task occurrence and never re-resolved on retry (see
	// internal/team/services.go's frozenTaskOccurrenceModel).
	ExecutionTargetFrozen *bool `yaml:"execution-target-frozen,omitempty"`
	// NoUnauthorizedFallback requires every recorded attempt backend and the
	// task's mutable BackendBinding to agree with its immutable admitted
	// ExecutionTarget. Missing provenance also fails closed.
	NoUnauthorizedFallback *bool `yaml:"no-unauthorized-fallback,omitempty"`
	// Evidence checks the sealed manifest and its artifact/evidence membership
	// against the case workspace's canonical artifact store.
	Evidence *EvidenceExpect `yaml:"evidence,omitempty"`
}

type EvidenceExpect struct {
	ManifestExists       *bool                  `yaml:"manifest-exists,omitempty"`
	HashValid            *bool                  `yaml:"hash-valid,omitempty"`
	Status               string                 `yaml:"status,omitempty"`
	MinArtifactRefs      *int                   `yaml:"min-artifact-refs,omitempty"`
	RequiredResults      []EvidenceResultExpect `yaml:"required-results,omitempty"`
	RequiredArtifactRefs []ArtifactRefExpect    `yaml:"required-artifact-refs,omitempty"`
}

// EvidenceResultExpect selects either an explicit requirement ID or a task
// by stable creation-order index. Exactly one selector must be set.
type EvidenceResultExpect struct {
	RequirementID   string `yaml:"requirement-id,omitempty"`
	TaskIndex       *int   `yaml:"task-index,omitempty"`
	Status          string `yaml:"status,omitempty"`
	MinArtifactRefs *int   `yaml:"min-artifact-refs,omitempty"`
}

// ArtifactRefExpect asserts minimum manifest membership for stable metadata.
// TaskIndex is optional; when omitted, refs from any task may match.
type ArtifactRefExpect struct {
	Kind      string `yaml:"kind,omitempty"`
	TaskIndex *int   `yaml:"task-index,omitempty"`
	MinCount  int    `yaml:"min-count"`
}

// MemoryAggregateExpect describes the durable learning counters and credit
// for one seeded/retrieved context item under one policy revision.
type MemoryAggregateExpect struct {
	ContextItemID    string   `yaml:"context-item-id"`
	PolicyVersion    string   `yaml:"policy-version"`
	MinExposureCount *int     `yaml:"min-exposure-count,omitempty"`
	ConsultedCount   *int     `yaml:"consulted-count,omitempty"`
	AppliedCount     *int     `yaml:"applied-count,omitempty"`
	RejectedCount    *int     `yaml:"rejected-count,omitempty"`
	PositiveWeight   *float64 `yaml:"positive-weight,omitempty"`
	NegativeWeight   *float64 `yaml:"negative-weight,omitempty"`
}

// TaskExpect asserts on one task by position: Tasks[i] in the fixture is
// matched against the i-th item of TaskTracker().TodoList().Items() in
// creation order. Task IDs are never compared -- a durable TodoItem.ID is a
// deterministic sequence counter ("1", "2", ...), not semantically
// meaningful on its own, so position is the stable, opaque-ID-free join key.
// A zero value field means "do not check this dimension".
type TaskExpect struct {
	Agent            string                `yaml:"agent"`
	Status           string                `yaml:"status"`
	Verification     string                `yaml:"verification"` // not-run, passed, or failed
	FailureClass     string                `yaml:"failure-class"`
	RetryDisposition string                `yaml:"retry-disposition"`
	SideEffect       string                `yaml:"side-effect"`
	Attempts         *int                  `yaml:"attempts"`
	ExecutionTarget  string                `yaml:"execution-target"`
	BackendBinding   *BackendBindingExpect `yaml:"backend-binding,omitempty"`
}

// BackendBindingExpect asserts the stable, secret-free execution backend
// projection. Empty fields are omitted assertions; ResumeSupported is a
// pointer so an explicit false remains distinguishable from omission.
type BackendBindingExpect struct {
	Backend          string `yaml:"backend"`
	SessionID        string `yaml:"session-id,omitempty"`
	TurnID           string `yaml:"turn-id,omitempty"`
	BackendVersion   string `yaml:"backend-version,omitempty"`
	EffectiveTarget  string `yaml:"effective-target,omitempty"`
	ExecutionWorldID string `yaml:"execution-world-id,omitempty"`
	CWD              string `yaml:"cwd,omitempty"`
	SandboxMode      string `yaml:"sandbox-mode,omitempty"`
	ResumeSupported  *bool  `yaml:"resume-supported,omitempty"`
}

// EventsExpect names the StatusEvent.Type values a case checks for.
type EventsExpect struct {
	// Required lists types that must have been reported at least once,
	// in no particular order.
	Required []string `yaml:"required"`
	// Order lists types that must appear, in this relative order, as a
	// subsequence of the full event stream. Other event types may appear
	// interleaved between them.
	Order []string `yaml:"order"`
	// Counts asserts exact cardinality for selected event types.
	Counts []EventCountExpect `yaml:"counts,omitempty"`
	// Matches selects the Nth event of a type (1-based) and compares only the
	// listed stable fields. Nested maps use dotted paths such as data.reason or
	// payload.status.
	Matches []EventMatchExpect `yaml:"matches,omitempty"`
}

type EventCountExpect struct {
	Type  string `yaml:"type"`
	Count int    `yaml:"count"`
}

type EventMatchExpect struct {
	Type       string         `yaml:"type"`
	Occurrence int            `yaml:"occurrence,omitempty"`
	Fields     map[string]any `yaml:"fields"`
}

// ProviderFixture is the scripted response program for one case's model
// driver (see docs/archive/implementation-plans/workflow-regression-eval-harness.md §4.1).
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
