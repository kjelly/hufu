// Package operator defines the immutable, presentation-safe view of Hufu's
// persisted runtime state. Its path resolver reads only filesystem metadata
// needed for canonicalization; the package never creates workspace state or
// owns database, provider, or runtime mutation behavior.
package operator

const SchemaVersion = 1

const (
	ActionSelectScope         = "select-scope"
	ActionInspectIntegrity    = "inspect-integrity"
	ActionInspectTaskRecovery = "inspect-task-recovery"
	ActionProvideInput        = "provide-input"
	ActionReviewApproval      = "review-approval"
	ActionWaitRuntime         = "wait-runtime"
	ActionResumeSession       = "resume-session"
	ActionReconcileTask       = "reconcile-task"
	ActionRetryTask           = "retry-task"
	ActionReviewResult        = "review-result"
	ActionNoneRequired        = "none-required"
)

const (
	ActivityUnconfigured    = "unconfigured"
	ActivityReady           = "ready"
	ActivityPreflight       = "preflight"
	ActivityPlanning        = "planning"
	ActivityExecuting       = "executing"
	ActivityVerifying       = "verifying"
	ActivityWaitingInput    = "waiting_input"
	ActivityWaitingApproval = "waiting_approval"
	ActivityBlocked         = "blocked"
	ActivityWrappingUp      = "wrapping_up"
	ActivityInterrupted     = "interrupted"
	ActivityFinished        = "finished"
	ActivityUnknown         = "unknown"
)

const (
	AttentionNone            = "none"
	AttentionInformational   = "informational"
	AttentionActionAvailable = "action_available"
	AttentionReviewRequired  = "review_required"
	AttentionHumanRequired   = "human_required"
	AttentionUnknown         = "unknown"
)

type OperatorSnapshot struct {
	SchemaVersion    int                `json:"schema_version"`
	SnapshotID       string             `json:"snapshot_id"`
	Scope            ResolvedScope      `json:"scope"`
	Activity         ActivityView       `json:"activity"`
	Outcome          OutcomeView        `json:"outcome"`
	Integrity        IntegrityView      `json:"integrity"`
	Freshness        FreshnessView      `json:"freshness"`
	Attention        string             `json:"attention"`
	Blockers         []DiagnosticView   `json:"blockers"`
	LatestChanges    []ChangeView       `json:"latest_changes"`
	RoleTargets      []RoleTargetView   `json:"role_targets"`
	Learning         LearningView       `json:"learning"`
	PrimaryAction    *ActionSuggestion  `json:"primary_action"`
	SecondaryActions []ActionSuggestion `json:"secondary_actions"`
}

type ResolvedScope struct {
	RequestedPath      string `json:"requested_path"`
	RequestedSemantics string `json:"requested_semantics"`
	WorkspaceExact     string `json:"workspace_exact"`
	WorkspaceRoot      string `json:"workspace_root"`
	ProjectDir         string `json:"project_dir"`
	ProjectID          string `json:"project_id"`
	TeamName           string `json:"team_name"`
	TeamDir            string `json:"team_dir"`
	SessionID          string `json:"session_id"`
	InvocationID       string `json:"invocation_id,omitempty"`
	RunID              string `json:"run_id"`
	BranchID           string `json:"branch_id"`
	SelectionSource    string `json:"selection_source"`
	BindingStatus      string `json:"binding_status"`
}

type ActivityView struct {
	State          string   `json:"state"`
	RawTaskStates  []string `json:"raw_task_states"`
	RawReasonCodes []string `json:"raw_reason_codes"`
}

type OutcomeView struct {
	RunOutcome      string `json:"run_outcome"`
	GoalSatisfied   *bool  `json:"goal_satisfied"`
	StopReason      string `json:"stop_reason"`
	AcceptanceState string `json:"acceptance_state"`
	CompletionState string `json:"completion_state"`
}

type IntegrityView struct {
	Status      string   `json:"status"`
	EventChain  string   `json:"event_chain"`
	Projection  string   `json:"projection"`
	ReasonCodes []string `json:"reason_codes"`
}

type FreshnessView struct {
	QueriedAt       string `json:"queried_at"`
	EventID         string `json:"event_id"`
	EventHash       string `json:"event_hash"`
	EventOrdinal    int64  `json:"event_ordinal"`
	ContextRevision string `json:"context_revision"`
	LiveState       string `json:"live_state"`
	StaleReason     string `json:"stale_reason"`
}

type DiagnosticView struct {
	Code     string `json:"code"`
	Severity string `json:"severity"`
	Message  string `json:"message"`
	Ref      string `json:"ref"`
}

type ChangeView struct {
	EventID      string   `json:"event_id"`
	EventOrdinal int64    `json:"event_ordinal"`
	Kind         string   `json:"kind"`
	Status       string   `json:"status"`
	ReasonCode   string   `json:"reason_code"`
	Refs         []string `json:"refs"`
}

type RoleTargetView struct {
	Role         string `json:"role"`
	Requested    string `json:"requested"`
	Effective    string `json:"effective"`
	BackendKind  string `json:"backend_kind"`
	Source       string `json:"source"`
	Availability string `json:"availability"`
	ReasonCode   string `json:"reason_code"`
}

type LearningView struct {
	Status             string `json:"status"`
	RequestedMode      string `json:"requested_mode"`
	EffectiveMode      string `json:"effective_mode"`
	PolicyVersion      string `json:"policy_version"`
	Exposures          *int64 `json:"exposures"`
	Consulted          *int64 `json:"consulted"`
	Applied            *int64 `json:"applied"`
	Rejected           *int64 `json:"rejected"`
	VerifiedSupport    *int64 `json:"verified_support"`
	CausalFailures     *int64 `json:"causal_failures"`
	EligiblePromotions *int64 `json:"eligible_promotions"`
	ProposedPromotions *int64 `json:"proposed_promotions"`
	ApprovedPromotions *int64 `json:"approved_not_applied"`
	AppliedPromotions  *int64 `json:"applied_promotions"`
	EmptyState         string `json:"empty_state"`
	UnavailableReason  string `json:"unavailable_reason"`
}

type ActionSuggestion struct {
	ID              string              `json:"id"`
	Kind            string              `json:"kind"`
	Actor           string              `json:"actor"`
	ReasonCode      string              `json:"reason_code"`
	Availability    string              `json:"availability"`
	Risk            string              `json:"risk"`
	Target          ActionTarget        `json:"target"`
	Preconditions   ActionPreconditions `json:"preconditions"`
	Argv            []string            `json:"argv"`
	CWD             string              `json:"cwd"`
	Confirmation    string              `json:"confirmation"`
	SourceRefs      []string            `json:"source_refs"`
	RevalidationKey string              `json:"revalidation_key"`
}

type ActionTarget struct {
	Workspace  string `json:"workspace"`
	ProjectID  string `json:"project_id"`
	TeamID     string `json:"team_id"`
	SessionID  string `json:"session_id"`
	RunID      string `json:"run_id"`
	BranchID   string `json:"branch_id"`
	TaskID     string `json:"task_id"`
	Attempt    int    `json:"attempt"`
	ProposalID string `json:"proposal_id"`
}

type ActionPreconditions struct {
	BindingStatus       string `json:"binding_status"`
	ExpectedActivity    string `json:"expected_activity"`
	ExpectedTaskStatus  string `json:"expected_task_status"`
	ExpectedEventID     string `json:"expected_event_id"`
	ExpectedEventHash   string `json:"expected_event_hash"`
	ExpectedPolicy      string `json:"expected_policy"`
	ExpectedRevision    string `json:"expected_revision"`
	ExternalEffectState string `json:"external_effect_state"`
}

// RecoveryEligibility is a read-only projection of the existing runtime
// recovery and policy decision. It does not grant permission to mutate.
type RecoveryEligibility struct {
	TaskID              string
	Attempt             int
	TaskStatus          string
	ExpectedEventID     string
	ExpectedEventHash   string
	ExternalEffectState string
	EffectivePolicy     string
	PolicyRevision      string
	ExpectedRevision    string
	PolicyDenied        bool
	ResumeEligible      bool
	ReconcileEligible   bool
	RetryEligible       bool
	CapabilityKnown     bool
	ReasonCode          string
	SourceRefs          []string
}

type ActionSelectionFacts struct {
	Snapshot                OperatorSnapshot
	Recovery                *RecoveryEligibility
	SessionExpectedRevision string
	SessionSourceRefs       []string
	MutationFacadeAvailable bool
}

type OperatorSummary struct {
	State string
	What  string
	Next  string
	Data  string
}

type WorkspaceRequest struct {
	RequestedPath string
	Mode          string
	TeamName      string
	ProjectDir    string
}

type WorkspaceResolution struct {
	RequestedPath      string
	RequestedSemantics string
	WorkspaceExact     string
	WorkspaceRoot      string
	ProjectDir         string
	TeamName           string
}

type BindingRequest struct {
	Workspace WorkspaceResolution
	ProjectID string
	TeamID    string
	SessionID string
	RunID     string
	BranchID  string
}
