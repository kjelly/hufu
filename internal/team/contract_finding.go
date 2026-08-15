package team

// ContractFinding is a single diagnostic produced by ValidateTaskContract or
// LintVerifier.  It is a pure data type; no I/O is performed when producing one.
type ContractFinding struct {
	// Severity is one of "error", "warning", "info".
	Severity string
	// Code is a machine-readable identifier, e.g. "verifier_not_asserting".
	Code string
	// Field identifies which task field triggered the finding (e.g. "verify",
	// "verify_spec", "execution", "timeout").
	Field string
	// Message is a human-readable explanation.
	Message string
	// Hint is an optional actionable suggestion.
	Hint string
}

// ContractPreflightResult is the outcome of a pre-dispatch contract preflight check.
type ContractPreflightResult struct {
	Valid       bool
	Findings    []ContractFinding
	Environment ExecutionEnvironment
}

// ExecutionEnvironment captures the environment snapshot taken during preflight.
type ExecutionEnvironment struct {
	WorkDir string
	Shell   string
}

// Severity constants for ContractFinding.
const (
	FindingSeverityError   = "error"
	FindingSeverityWarning = "warning"
	FindingSeverityInfo    = "info"
)

// Code constants for ContractFinding.
const (
	// FindingVerifierNotAsserting indicates a verifier whose structure guarantees
	// it can never fail, regardless of task outcome.
	// Refs: docs/hufu-generic-task-reliability-mechanisms.md §4.3
	FindingVerifierNotAsserting = "verifier_not_asserting"

	// FindingExecutableUnresolved indicates that one or more pipeline stage
	// executables could not be resolved via PATH or project-local lookup.
	// Refs: docs/hufu-generic-task-reliability-mechanisms.md §4.1
	FindingExecutableUnresolved = "executable_unresolved"

	// FindingAcceptanceVacuous indicates that an outcome-mode run carries an
	// empty acceptance contract, making run-level completion permanently
	// unachievable.
	// Refs: docs/hufu-generic-task-reliability-mechanisms.md §4.4
	FindingAcceptanceVacuous = "acceptance_vacuous"
	// FindingCompletionToolDenied reports an outcome team that has an
	// acceptance contract but removes the finish tool needed to evaluate it.
	FindingCompletionToolDenied    = "completion_tool_denied"
	FindingDelegationWorkerUnknown = "delegation_worker_unknown"
	FindingDelegationWorkerRole    = "delegation_worker_role_invalid"
	FindingDelegationWorkerDenied  = "delegation_worker_not_allowed"
	FindingToolPolicyConflict      = "tool_policy_conflict"
	FindingDeprecatedMemoryTool    = "deprecated_memory_tool"
	FindingRequiredToolDenied      = "required_tool_denied"
	FindingRequiredToolUnavailable = "required_tool_unavailable"
	FindingRequiredEnvMissing      = "required_environment_missing"
	FindingRequiredPathDenied      = "required_path_denied"
	FindingInteractiveUnattended   = "interactive_requirement_unattended"
	FindingNetworkDisabled         = "network_requirement_disabled"
	FindingPlanFirstRequired       = "plan_first_required"
	FindingRequirementInvalid      = "requirement_invalid"

	// FindingDeadlineConflict indicates that a child deadline equals or exceeds
	// its parent deadline, leaving no room for result finalisation.
	// Refs: docs/hufu-generic-task-reliability-mechanisms.md §4.1
	FindingDeadlineConflict = "deadline_conflict"

	// FindingObservationMode indicates a verifier is in observation mode and
	// therefore cannot satisfy a requires_verification contract.
	FindingObservationMode = "observation_mode"
)
