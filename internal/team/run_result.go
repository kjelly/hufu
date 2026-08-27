package team

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/kjelly/hufu/internal/agent"
)

type RunOutcome string

// ErrInvocationStalled is the stable cancellation cause used when an
// invocation-owned watchdog observes that its provider has stopped making
// progress. It is deliberately distinct from user cancellation.
var ErrInvocationStalled = errors.New("invocation stalled")

const (
	RunOutcomeCompleted  RunOutcome = "completed"
	RunOutcomeUnverified RunOutcome = "unverified"
	RunOutcomePartial    RunOutcome = "partial"
	RunOutcomeBlocked    RunOutcome = "blocked"
	RunOutcomeFailed     RunOutcome = "failed"
	RunOutcomeCancelled  RunOutcome = "cancelled"
	RunOutcomeStalled    RunOutcome = "stalled"
)

type GoalMode string

const (
	GoalModeOutcome     GoalMode = "outcome"
	GoalModeExploratory GoalMode = "exploratory"
)

func ParseGoalMode(s string) (GoalMode, error) {
	lower := strings.ToLower(strings.TrimSpace(s))
	if lower == "" {
		return GoalModeOutcome, nil
	}
	switch GoalMode(lower) {
	case GoalModeOutcome:
		return GoalModeOutcome, nil
	case GoalModeExploratory:
		return GoalModeExploratory, nil
	default:
		return "", fmt.Errorf("invalid goal mode %q (must be \"outcome\" or \"exploratory\")", s)
	}
}

func IsValidGoalMode(mode GoalMode) bool {
	switch mode {
	case GoalModeOutcome, GoalModeExploratory:
		return true
	default:
		return false
	}
}

type StopReason string

const (
	StopReasonCompleted              StopReason = "completed"
	StopReasonAcceptanceFailed       StopReason = "acceptance_failed"
	StopReasonAcceptanceNotSet       StopReason = "acceptance_not_configured"
	StopReasonUnresolvedTasks        StopReason = "unresolved_tasks"
	StopReasonExternalBlockage       StopReason = "external_blockage"
	StopReasonPolicyViolation        StopReason = "policy_violation"
	StopReasonBudgetExceeded         StopReason = "budget_exceeded"
	StopReasonCoordinatorInterrupted StopReason = "coordinator_interrupted"
	StopReasonCancelled              StopReason = "cancelled"
	StopReasonRunFailed              StopReason = "run_failed"
	StopReasonEvidenceIncomplete     StopReason = "evidence_incomplete"
	StopReasonInvocationStalled      StopReason = "invocation_stalled"
)

func (r RunOutcome) String() string {
	return string(r)
}

func IsRunOutcomeSuccess(outcome RunOutcome) bool {
	return outcome == RunOutcomeCompleted
}

// RunOutcomeError carries the canonical evaluator result across the CLI
// boundary while preserving the underlying execution error for errors.Is and
// errors.As callers.
type RunOutcomeError struct {
	Result *RunResult
	Cause  error
}

func (e *RunOutcomeError) Error() string {
	if e == nil {
		return "run outcome error"
	}
	if e.Cause != nil {
		return e.Cause.Error()
	}
	if e.Result != nil {
		return fmt.Sprintf("run outcome: %s", e.Result.Outcome)
	}
	return "run outcome error"
}

func (e *RunOutcomeError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

// ProcessExitCode returns the exit code chosen by the canonical evaluator.
func (e *RunOutcomeError) ProcessExitCode() int {
	if e != nil && e.Result != nil && e.Result.ExitCode != 0 {
		return e.Result.ExitCode
	}
	return 1
}

// WrapRunOutcomeError attaches canonical outcome data to an existing error.
func WrapRunOutcomeError(cause error, result *RunResult) error {
	if result == nil {
		return cause
	}
	return &RunOutcomeError{Cause: cause, Result: result}
}

type AcceptanceSpec = agent.AcceptanceSpec
type VerificationSpec = agent.VerificationSpec
type VerificationType = agent.VerificationType
type JSONAssertion = agent.JSONAssertion
type ToolCallAssertion = agent.ToolCallAssertion
type TaskResultAssertion = agent.TaskResultAssertion
type AcceptanceCriterion = agent.AcceptanceCriterion
type ReliabilityConfig = agent.ReliabilityConfig

const (
	VerifyCommandExit      = agent.VerifyCommandExit
	VerifyFileExists       = agent.VerifyFileExists
	VerifyFileAbsent       = agent.VerifyFileAbsent
	VerifyJSONAssert       = agent.VerifyJSONAssert
	VerifyToolCallAssert   = agent.VerifyToolCallAssert
	VerifyTaskResultAssert = agent.VerifyTaskResultAssert
	VerifyWorksetComplete  = agent.VerifyWorksetComplete
)

// AcceptanceState describes whether an acceptance gate was configured and,
// when configured, whether it passed. NotConfigured is deliberately distinct
// from Passed: absence of a gate is not evidence that the gate succeeded.
type AcceptanceState string

const (
	AcceptanceNotConfigured AcceptanceState = "not_configured"
	AcceptancePassed        AcceptanceState = "passed"
	AcceptanceFailed        AcceptanceState = "failed"
)

func cloneVerificationSpec(v VerificationSpec) VerificationSpec {
	c := v
	if v.Assertions != nil {
		c.Assertions = make([]JSONAssertion, len(v.Assertions))
		for i, assertion := range v.Assertions {
			c.Assertions[i] = assertion
			c.Assertions[i].Equals = cloneTaskResultValue(assertion.Equals)
		}
	}
	if v.ToolCallAssertions != nil {
		c.ToolCallAssertions = append([]ToolCallAssertion(nil), v.ToolCallAssertions...)
	}
	if v.TaskResultAssertions != nil {
		c.TaskResultAssertions = make([]TaskResultAssertion, len(v.TaskResultAssertions))
		for i, assertion := range v.TaskResultAssertions {
			c.TaskResultAssertions[i] = assertion
			c.TaskResultAssertions[i].Value = cloneTaskResultValue(assertion.Value)
		}
	}
	c.WorksetSourceTask = v.WorksetSourceTask
	c.WorksetRequireTerminal = v.WorksetRequireTerminal
	c.WorksetRequireVerified = v.WorksetRequireVerified
	c.WorksetAcceptedStatuses = append([]string(nil), v.WorksetAcceptedStatuses...)
	return c
}

// cloneVerificationSpecPtr returns nil if src is nil, or a deep clone otherwise.
func cloneVerificationSpecPtr(src *VerificationSpec) *VerificationSpec {
	if src == nil {
		return nil
	}
	clone := cloneVerificationSpec(*src)
	return &clone
}

// cloneAcceptanceSpec detaches every caller-owned slice from the acceptance
// contract. AcceptanceSpec is part of the run's immutable contract after it is
// accepted; copying only the struct would leave Commands and
// RequiredArtifacts vulnerable to out-of-band mutation through aliases.
func cloneAcceptanceSpec(spec AcceptanceSpec) AcceptanceSpec {
	clone := spec
	if spec.Commands != nil {
		clone.Commands = append([]string(nil), spec.Commands...)
	}
	if spec.RequiredArtifacts != nil {
		clone.RequiredArtifacts = append([]string(nil), spec.RequiredArtifacts...)
	}
	if spec.Verifications != nil {
		clone.Verifications = make([]VerificationSpec, len(spec.Verifications))
		for i, v := range spec.Verifications {
			clone.Verifications[i] = cloneVerificationSpec(v)
		}
	}
	if spec.Criteria != nil {
		clone.Criteria = make([]AcceptanceCriterion, len(spec.Criteria))
		for i, criterion := range spec.Criteria {
			clone.Criteria[i] = criterion
			clone.Criteria[i].DependsOn = append([]string(nil), criterion.DependsOn...)
			clone.Criteria[i].Verify = cloneVerificationSpec(criterion.Verify)
		}
	}
	return clone
}

type AcceptanceResult struct {
	State                AcceptanceState       `json:"state"`
	Passed               bool                  `json:"passed"`
	Errors               []string              `json:"errors,omitempty"`
	Commands             []string              `json:"commands,omitempty"`
	RequiredArtifacts    []string              `json:"required_artifacts,omitempty"`
	VerificationEvidence []*VerificationResult `json:"verification_evidence,omitempty"`
	CriterionResults     []CriterionResult     `json:"criterion_results,omitempty"`
}

// MarshalJSON canonicalizes the legacy Passed field from the tri-state value.
// This prevents an inconsistent in-memory value such as
// {state:not_configured, passed:true} from being persisted as acceptance
// evidence.
func (r AcceptanceResult) MarshalJSON() ([]byte, error) {
	type wire AcceptanceResult
	canonical := wire(r)
	canonical.State = r.EffectiveState()
	canonical.Passed = canonical.State == AcceptancePassed
	return json.Marshal(canonical)
}

// EffectiveState provides a compatibility interpretation for run results
// persisted before the tri-state field existed. New results always populate
// State explicitly.
func (r AcceptanceResult) EffectiveState() AcceptanceState {
	if r.State != "" {
		switch r.State {
		case AcceptanceNotConfigured, AcceptancePassed, AcceptanceFailed:
			return r.State
		default:
			return AcceptanceFailed
		}
	}
	if r.Passed {
		return AcceptancePassed
	}
	return AcceptanceFailed
}

// IsPassed derives acceptance success from the canonical state, ignoring a
// stale legacy Passed bit.
func (r AcceptanceResult) IsPassed() bool {
	return r.EffectiveState() == AcceptancePassed
}

// FormatCanonicalStatus formats the canonical human-readable status text from a RunResult
// for CLI, TUI, reports, and notifications.
func FormatCanonicalStatus(res *RunResult) string {
	if res == nil {
		return "All tasks completed"
	}
	if res.GoalSatisfied {
		if res.FixedAndVerified {
			return "Execution fixed and objectively verified"
		}
		return "Execution completed successfully"
	}
	switch res.Outcome {
	case RunOutcomeCompleted:
		if res.CompletedReview {
			if res.FindingsPresent {
				return "Review completed; findings remain"
			}
			return "Review completed; findings unverified"
		}
		return "Execution completed; goal unverified"
	case RunOutcomeUnverified:
		return "Execution completed; goal unverified (no acceptance configured)"
	case RunOutcomeBlocked:
		return "Execution blocked"
	case RunOutcomeCancelled:
		return "Execution cancelled"
	case RunOutcomeStalled:
		return "Execution stalled"
	case RunOutcomeFailed:
		return "Execution failed"
	case RunOutcomePartial:
		switch res.StopReason {
		case StopReasonBudgetExceeded:
			return "Budget exhausted"
		case StopReasonAcceptanceFailed:
			return "Acceptance check failed"
		default:
			return "Execution incomplete"
		}
	default:
		if res.StopReason != "" {
			return fmt.Sprintf("Execution %s (%s)", res.Outcome, res.StopReason)
		}
		return fmt.Sprintf("Execution %s", res.Outcome)
	}
}

// RunEvaluationInput is the complete canonical input to outcome evaluation.
// It contains state already observed by the coordinator; evaluating it has no
// side effects and does not inspect prompts, task descriptions, or output.
type RunEvaluationInput struct {
	UnresolvedTasks        []TaskReference
	Acceptance             AcceptanceState
	Cancelled              bool
	Stalled                bool
	BudgetExceeded         bool
	CoordinatorInterrupted bool
	RunFailed              bool
	ExitCode               int
	Response               string
	Reason                 string
	Stats                  RunStats
	Metrics                RunMetrics
	GoalMode               GoalMode
}

// EvaluateRunOutcome is the sole policy for deriving a run outcome and goal
// satisfaction from canonical run state.
func EvaluateRunOutcome(input RunEvaluationInput) RunResult {
	acceptance := input.Acceptance
	if acceptance == "" {
		acceptance = AcceptanceNotConfigured
	} else if acceptance != AcceptanceNotConfigured && acceptance != AcceptancePassed && acceptance != AcceptanceFailed {
		// An unknown persisted/configured state is not evidence of acceptance.
		// Fail closed rather than allowing malformed state to become completed.
		acceptance = AcceptanceFailed
	}
	goalMode := input.GoalMode
	if goalMode == "" || !IsValidGoalMode(goalMode) {
		goalMode = GoalModeOutcome
	}

	result := RunResult{
		GoalMode: goalMode,
		Response: input.Response,
		Reason:   input.Reason,
		Acceptance: &AcceptanceResult{
			State:  acceptance,
			Passed: acceptance == AcceptancePassed,
		},
		UnresolvedTasks: append([]TaskReference(nil), input.UnresolvedTasks...),
		Stats:           input.Stats,
		Metrics:         input.Metrics,
	}

	if input.Stalled {
		result.Outcome = RunOutcomeStalled
		result.StopReason = StopReasonInvocationStalled
		result.ExitCode = 124
		return result
	}
	if input.Cancelled {
		result.Outcome = RunOutcomeCancelled
		result.StopReason = StopReasonCancelled
		result.ExitCode = input.ExitCode
		if result.ExitCode == 0 {
			result.ExitCode = 130
		}
		return result
	}
	if input.RunFailed {
		result.Outcome = RunOutcomeFailed
		result.StopReason = StopReasonRunFailed
		result.ExitCode = input.ExitCode
		if result.ExitCode == 0 {
			result.ExitCode = 1
		}
		return result
	}
	if input.BudgetExceeded {
		result.Outcome = RunOutcomePartial
		result.StopReason = StopReasonBudgetExceeded
		result.ExitCode = 7
		return result
	}
	if input.CoordinatorInterrupted {
		result.Outcome = RunOutcomePartial
		result.StopReason = StopReasonCoordinatorInterrupted
		result.ExitCode = 7
		return result
	}
	for _, task := range input.UnresolvedTasks {
		if task.Status == string(TaskBlocked) {
			result.Outcome = RunOutcomeBlocked
			if task.FailureClass == FailurePolicy || task.FailureClass == FailureContract || task.FailureClass == FailureProtocol {
				result.StopReason = StopReasonPolicyViolation
			} else {
				result.StopReason = StopReasonExternalBlockage
			}
			result.ExitCode = 7
			return result
		}
		if task.Status == string(TaskError) {
			result.Outcome = RunOutcomePartial
			result.StopReason = StopReasonUnresolvedTasks
			result.ExitCode = 7
			return result
		}
	}
	if len(input.UnresolvedTasks) > 0 {
		result.Outcome = RunOutcomePartial
		result.StopReason = StopReasonUnresolvedTasks
		result.ExitCode = 7
		return result
	}
	if acceptance == AcceptanceFailed {
		result.Outcome = RunOutcomePartial
		result.StopReason = StopReasonAcceptanceFailed
		result.ExitCode = 7
		return result
	}
	if acceptance == AcceptanceNotConfigured {
		// An exploratory run may legitimately stop without an acceptance
		// contract, but it has not established that the user's requested
		// outcome exists. Keep that distinction in the canonical result: a
		// successful-looking worker summary or finish call is not evidence of
		// completion. Callers that only need exploratory output can inspect the
		// response, while automation must see an unverified non-success result.
		result.Outcome = RunOutcomeUnverified
		result.StopReason = StopReasonAcceptanceNotSet
		result.GoalSatisfied = false
		result.ExitCode = 7
		return result
	}

	result.Outcome = RunOutcomeCompleted
	result.StopReason = StopReasonCompleted
	result.GoalSatisfied = true
	result.ExitCode = 0
	return result
}

// AggregateRunResults evaluates a multi-team run from the already computed
// per-team canonical results. Presentation layers must use this helper rather
// than reimplementing outcome or goal-satisfaction rules. The aggregation is
// deliberately pure: it only folds observed results and delegates the final
// decision to EvaluateRunOutcome.
func AggregateRunResults(results []*RunResult, unresolved []TaskReference, stats RunStats) RunResult {
	input := RunEvaluationInput{
		UnresolvedTasks: append([]TaskReference(nil), unresolved...),
		Stats:           stats,
	}
	var partial bool
	var blocked bool
	var foldedStats RunStats
	failedExitCode := 0
	cancelledExitCode := 0
	completedReview := false
	findingsPresent := false
	acceptanceAdvisory := false
	var contextTelemetry ContextWindowTelemetrySummary
	worksets := make(map[string]WorksetGroupState)
	for _, result := range results {
		if result == nil {
			continue
		}
		if result.GoalMode == GoalModeOutcome {
			input.GoalMode = GoalModeOutcome
		} else if input.GoalMode == "" && result.GoalMode != "" {
			input.GoalMode = result.GoalMode
		}
		if input.Response == "" {
			input.Response = result.Response
		}
		if input.Reason == "" {
			input.Reason = result.Reason
		}
		if len(result.UnresolvedTasks) > 0 {
			input.UnresolvedTasks = append(input.UnresolvedTasks, result.UnresolvedTasks...)
		}
		foldedStats.TasksTotal += result.Stats.TasksTotal
		foldedStats.TasksDone += result.Stats.TasksDone
		foldedStats.TasksUnresolved += result.Stats.TasksUnresolved
		foldedStats.AttemptsTotal += result.Stats.AttemptsTotal
		foldedStats.AttemptsFailed += result.Stats.AttemptsFailed

		if result.StopReason == StopReasonBudgetExceeded {
			input.BudgetExceeded = true
		}
		completedReview = completedReview || result.CompletedReview
		findingsPresent = findingsPresent || result.FindingsPresent
		acceptanceAdvisory = acceptanceAdvisory || result.AcceptanceAdvisory
		mergeWorksetStates(worksets, result.Worksets)
		mergeContextWindowTelemetrySummary(&contextTelemetry, result.Metrics.ContextWindowTelemetry)

		if result.Acceptance != nil {
			switch result.Acceptance.EffectiveState() {
			case AcceptanceFailed:
				input.Acceptance = AcceptanceFailed
			case AcceptancePassed:
				if input.Acceptance != AcceptanceFailed {
					input.Acceptance = AcceptancePassed
				}
			case AcceptanceNotConfigured:
				if input.Acceptance == "" {
					input.Acceptance = AcceptanceNotConfigured
				}
			default:
				input.Acceptance = AcceptanceFailed
			}
		}
		switch result.Outcome {
		case RunOutcomeCancelled:
			input.Cancelled = true
			if result.ExitCode > cancelledExitCode {
				cancelledExitCode = result.ExitCode
			}
		case RunOutcomeStalled:
			input.Stalled = true
		case RunOutcomeFailed:
			input.RunFailed = true
			if result.ExitCode > failedExitCode {
				failedExitCode = result.ExitCode
			}
		case RunOutcomePartial:
			partial = true
		case RunOutcomeBlocked:
			blocked = true
		}
	}
	if input.Stats.IsZero() {
		input.Stats = foldedStats
	}
	// Cancellation has evaluator precedence over ordinary failures, so its
	// exit code must also win regardless of result iteration order.
	if input.Cancelled {
		input.ExitCode = cancelledExitCode
	} else if input.RunFailed {
		input.ExitCode = failedExitCode
	}
	// A legacy or partially persisted result may omit its task references. Keep
	// that result fail-closed instead of allowing presentation to turn it into
	// completed solely because the reference list is empty.
	if blocked && len(input.UnresolvedTasks) == 0 {
		input.UnresolvedTasks = []TaskReference{{ID: "<blocked-run>", Status: string(TaskBlocked)}}
	} else if partial && len(input.UnresolvedTasks) == 0 && input.Stats.TasksUnresolved > 0 {
		input.UnresolvedTasks = []TaskReference{{ID: "<unresolved-run>", Status: string(TaskPending)}}
	}
	// A partial team result with no unresolved task or failed acceptance is the
	// canonical signal for a budget/early-stop outcome. If unresolved work is
	// present, let the evaluator classify it as partial or blocked instead.
	if partial && len(input.UnresolvedTasks) == 0 && input.Acceptance != AcceptanceFailed {
		input.BudgetExceeded = true
	}
	input.Metrics.ContextWindowTelemetry = contextTelemetry
	aggregated := EvaluateRunOutcome(input)
	aggregated.CompletedReview = completedReview
	aggregated.FindingsPresent = findingsPresent
	aggregated.AcceptanceAdvisory = acceptanceAdvisory
	if len(worksets) > 0 {
		aggregated.Worksets = make([]WorksetGroupState, 0, len(worksets))
		for _, workset := range worksets {
			aggregated.Worksets = append(aggregated.Worksets, workset)
		}
		sort.Slice(aggregated.Worksets, func(i, j int) bool { return aggregated.Worksets[i].WorksetID < aggregated.Worksets[j].WorksetID })
	}
	// A multi-team aggregate can only claim fixed_and_verified when its
	// canonical outcome is successful and no participating review left findings.
	aggregated.FixedAndVerified = aggregated.GoalSatisfied && !findingsPresent
	return aggregated
}

func mergeWorksetStates(dst map[string]WorksetGroupState, states []WorksetGroupState) {
	for _, state := range states {
		dst[state.WorksetID] = state
	}
}

func mergeContextWindowTelemetrySummary(dst *ContextWindowTelemetrySummary, src ContextWindowTelemetrySummary) {
	if dst == nil {
		return
	}
	dst.AdmissionEvents += src.AdmissionEvents
	dst.Admitted += src.Admitted
	dst.CannotFit += src.CannotFit
	dst.CompactionCommits += src.CompactionCommits
	dst.Downshifts += src.Downshifts
	dst.DownshiftExhaustions += src.DownshiftExhaustions
	if src.LastDecision != "" {
		dst.LastDecision = src.LastDecision
		dst.LastModel = src.LastModel
		dst.LastRequestedTokens = src.LastRequestedTokens
		dst.LastAvailableTokens = src.LastAvailableTokens
		dst.LastCompactionCount = src.LastCompactionCount
		dst.PolicyDigest = src.PolicyDigest
	}
}

type TaskReference struct {
	ID           string           `json:"id"`
	Agent        string           `json:"agent,omitempty"`
	Desc         string           `json:"desc"`
	Status       string           `json:"status"`
	Error        string           `json:"error,omitempty"`
	FailureClass TaskFailureClass `json:"failure_class,omitempty"`
}

type ContinuationInfo struct {
	TurnCount               int                 `json:"turn_count"`
	MaxTurns                int                 `json:"max_turns"`
	Reason                  string              `json:"reason,omitempty"`
	NoProgress              *NoProgressCounters `json:"no_progress,omitempty"`
	NoProgressReplanPending bool                `json:"no_progress_replan_pending,omitempty"`
}

// ContinuationCheckpoint is the durable handoff point for a coordinator
// continuation. It is intentionally small so a restart can identify whether
// a continuation was interrupted without replaying the model transcript.
type ContinuationCheckpoint struct {
	TurnCount               int                 `json:"turn_count"`
	MaxTurns                int                 `json:"max_turns"`
	Reason                  string              `json:"reason,omitempty"`
	Status                  string              `json:"status"` // pending, resumed, completed, aborted
	NoProgress              *NoProgressCounters `json:"no_progress,omitempty"`
	NoProgressReplanPending bool                `json:"no_progress_replan_pending,omitempty"`
}

// CriterionCheckpoint is the durable, objective handoff for an acceptance
// criterion affected by an interactive or external task.  It deliberately
// stores verifier evidence rather than a worker claim so recovery can reject
// stale or incompatible proof before any unsafe replay.
type CriterionCheckpoint struct {
	ID               string                `json:"id"`
	TaskID           string                `json:"task_id,omitempty"`
	CriterionID      string                `json:"criterion_id"`
	Proven           bool                  `json:"proven"`
	Evidence         []*VerificationResult `json:"evidence,omitempty"`
	InputFingerprint string                `json:"input_fingerprint,omitempty"`
	ResumeAction     string                `json:"resume_action,omitempty"`
	ReplayPolicy     RecoveryPolicy        `json:"replay_policy,omitempty"`
	CreatedAt        string                `json:"created_at"`
}

// AcceptanceContractRevision records an immutable acceptance-contract change.
// Criterion states: pending, passed, failed, blocked.
type AcceptanceContractRevision struct {
	Revision  int             `json:"revision"`
	Timestamp string          `json:"timestamp"`
	OldSpec   *AcceptanceSpec `json:"old_spec,omitempty"`
	NewSpec   AcceptanceSpec  `json:"new_spec"`
	Reason    string          `json:"reason,omitempty"`
}

// RunMetrics is a queryable snapshot of reliability counters for a run.
type RunMetrics struct {
	RetriesByFailureClass             map[TaskFailureClass]int    `json:"retries_by_failure_class,omitempty"`
	RetrySuppressions                 int                         `json:"retry_suppressions,omitempty"`
	RetrySuppressionsByReason         map[string]int              `json:"retry_suppressions_by_reason,omitempty"`
	FailuresByClass                   map[TaskFailureClass]int    `json:"failures_by_class,omitempty"`
	FailuresByPhase                   map[string]int              `json:"failures_by_phase,omitempty"`
	RetryAttemptsAvoidedByDisposition map[RetryDisposition]int    `json:"retry_attempts_avoided_by_disposition,omitempty"`
	Compactions                       int                         `json:"compactions"`
	RepeatedFailureFingerprints       int                         `json:"repeated_failure_fingerprints,omitempty"`
	SystemicFingerprintsEscalated     int                         `json:"systemic_fingerprints_escalated,omitempty"`
	RecoveryStrategyChanges           int                         `json:"recovery_strategy_changes,omitempty"`
	LastRecoveryStrategies            map[string]RecoveryStrategy `json:"last_recovery_strategies,omitempty"`
	DiagnosticTasksSinceProgress      int                         `json:"diagnostic_tasks_since_progress,omitempty"`
	RepairAttemptsByCriterion         map[string]int              `json:"repair_attempts_by_criterion,omitempty"`
	AntiThrashingWarnings             int                         `json:"anti_thrashing_warnings,omitempty"`
	AcceptanceCriteriaPassed          int                         `json:"acceptance_criteria_passed,omitempty"`
	TasksByCriterion                  map[string]int              `json:"tasks_by_criterion,omitempty"`
	ProtocolRepairsAttempted          int                         `json:"protocol_repairs_attempted,omitempty"`
	ProtocolRepairsSucceeded          int                         `json:"protocol_repairs_succeeded,omitempty"`
	CoordinatorPolicyRepairsAttempted int                         `json:"coordinator_policy_repairs_attempted,omitempty"`
	CoordinatorPolicyRepairsSucceeded int                         `json:"coordinator_policy_repairs_succeeded,omitempty"`
	ProtocolRepairFailuresByReason    map[RepairFailureReason]int `json:"protocol_repair_failures_by_reason,omitempty"`
	// StepBudgetExhaustions counts attempts cut off by the step budget. They also
	// appear as protocol failures (the worker omitted its result), so this
	// counter is what separates "tasks needed more tool calls" from "the model
	// ignored the result contract".
	StepBudgetExhaustions              int     `json:"step_budget_exhaustions,omitempty"`
	PolicyDeniedToolCalls              int     `json:"policy_denied_tool_calls,omitempty"`
	SafeFreshAttempts                  int     `json:"safe_fresh_attempts,omitempty"`
	StepBudgetWrapUps                  int     `json:"step_budget_wrap_ups,omitempty"`
	SchemaRepairDenials                int     `json:"schema_repair_denials,omitempty"`
	ExecutionReplaysAvoided            int     `json:"execution_replays_avoided,omitempty"`
	ReplayAttempts                     int     `json:"replay_attempts,omitempty"`
	UnsafeReplaysDetected              int     `json:"unsafe_replays_detected,omitempty"`
	ReconciliationAttempts             int     `json:"reconciliation_attempts,omitempty"`
	ReconciliationSucceeded            int     `json:"reconciliation_succeeded,omitempty"`
	TimeoutTasksRecovered              int     `json:"timeout_tasks_recovered_through_reconciliation,omitempty"`
	PreflightFailuresCaught            int     `json:"preflight_failures_caught_before_dispatch,omitempty"`
	NonAssertingVerifiersRejected      int     `json:"non_asserting_verifiers_rejected,omitempty"`
	VerificationsOverturned            int     `json:"verifications_overturned_by_evidence_precedence,omitempty"`
	TypedVerifiers                     int     `json:"typed_verifiers,omitempty"`
	TasksWithVerifier                  int     `json:"tasks_with_verifier,omitempty"`
	TypedVerifierAdoptionRate          float64 `json:"typed_verifier_adoption_rate,omitempty"`
	TasksDoneWithoutObjectiveVerifier  int     `json:"tasks_done_without_objective_verifier,omitempty"`
	RepeatedFailureFingerprintsStopped int     `json:"repeated_failure_fingerprints_stopped,omitempty"`
	CancelledTasksExcludedFromRetries  int     `json:"cancelled_tasks_excluded_from_retry_statistics,omitempty"`
	WorkerSuccessRejected              int     `json:"worker_success_rejected_by_verification,omitempty"`
	WeakVerifierWarnings               int     `json:"weak_verifier_warnings,omitempty"`
	TimeSinceCriterionProgressSeconds  int64   `json:"time_since_criterion_progress_seconds,omitempty"`
	TokensSinceCriterionProgress       int64   `json:"tokens_since_criterion_progress,omitempty"`
	// No-progress budget counters (§8.1, WP-12). Mirrors the coordinator
	// fields; reset only by objective criterion progress.
	TurnsSinceCriterionProgress int `json:"turns_since_criterion_progress,omitempty"`
	TasksSinceCriterionProgress int `json:"tasks_since_criterion_progress,omitempty"`
	// No-progress budget configured limits (§8.1, WP-12). 0 = disabled.
	MaxTokensWithoutProgress int64                         `json:"max_tokens_without_progress,omitempty"`
	MaxTurnsWithoutProgress  int                           `json:"max_turns_without_progress,omitempty"`
	MaxTasksWithoutProgress  int                           `json:"max_tasks_without_progress,omitempty"`
	ContextWindowTelemetry   ContextWindowTelemetrySummary `json:"context_window_telemetry,omitempty"`
}

// RepairCost is the quantitative cost of recovery decisions made during a
// run. Token and wall-clock values are provider-independent and remain zero
// when the provider did not report them.
type RepairCost struct {
	Attempts    int   `json:"attempts"`
	Tokens      int64 `json:"tokens"`
	WallClockMS int64 `json:"wall_clock_ms"`
}

// UnmarshalJSON keeps redacted legacy telemetry loadable. Earlier session
// redaction could replace a numeric RepairCost field with a string such as
// "[REDACTED]". Cost telemetry is observational rather than authorization
// evidence, so an unparseable legacy value safely becomes zero instead of
// invalidating the whole persisted session.
func (c *RepairCost) UnmarshalJSON(data []byte) error {
	type repairCost RepairCost
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	for _, name := range []string{"attempts", "tokens", "wall_clock_ms"} {
		raw, ok := fields[name]
		if !ok {
			continue
		}
		var value int64
		if err := json.Unmarshal(raw, &value); err != nil {
			fields[name] = json.RawMessage("0")
		}
	}
	normalized, err := json.Marshal(fields)
	if err != nil {
		return err
	}
	var decoded repairCost
	if err := json.Unmarshal(normalized, &decoded); err != nil {
		return err
	}
	*c = RepairCost(decoded)
	return nil
}

type RunTelemetry struct {
	DecisionChain    []string   `json:"decision_chain,omitempty"`
	PlanRevision     string     `json:"plan_revision,omitempty"`
	EvidenceManifest string     `json:"evidence_manifest,omitempty"`
	RepairCost       RepairCost `json:"repair_cost"`
	TerminalReason   string     `json:"terminal_reason,omitempty"`
}

type TaskResolution struct {
	Status     string        `json:"status"` // "unresolved", "superseded", "reconciled", "waived"
	ResolvedBy string        `json:"resolved_by,omitempty"`
	Reason     string        `json:"reason,omitempty"`
	Evidence   []EvidenceRef `json:"evidence,omitempty"`
}

type RunStats struct {
	TasksTotal      int `json:"tasks_total"`
	TasksDone       int `json:"tasks_done"`
	TasksUnresolved int `json:"tasks_unresolved"`
	AttemptsTotal   int `json:"attempts_total"`
	AttemptsFailed  int `json:"attempts_failed"`
}

func (s RunStats) IsZero() bool {
	return s.TasksTotal == 0 && s.TasksDone == 0 && s.TasksUnresolved == 0 && s.AttemptsTotal == 0 && s.AttemptsFailed == 0
}

type RunResult struct {
	RunID            string              `json:"run_id,omitempty"`
	Outcome          RunOutcome          `json:"outcome"`
	GoalSatisfied    bool                `json:"goal_satisfied"`
	GoalMode         GoalMode            `json:"goal_mode,omitempty"`
	Response         string              `json:"response"`
	Reason           string              `json:"reason,omitempty"`
	StopReason       StopReason          `json:"stop_reason,omitempty"`
	ExitCode         int                 `json:"exit_code,omitempty"`
	Acceptance       *AcceptanceResult   `json:"acceptance,omitempty"`
	Worksets         []WorksetGroupState `json:"worksets,omitempty"`
	UnresolvedTasks  []TaskReference     `json:"unresolved_tasks,omitempty"`
	Continuation     *ContinuationInfo   `json:"continuation,omitempty"`
	Stats            RunStats            `json:"stats"`
	Metrics          RunMetrics          `json:"metrics,omitempty"`
	EvidenceManifest *EvidenceManifest   `json:"evidence_manifest,omitempty"`
	Telemetry        *RunTelemetry       `json:"telemetry,omitempty"`
	// Review completion is intentionally separate from an implementation
	// outcome: a completed exploratory review may surface findings but never
	// proves they were fixed.
	CompletedReview    bool `json:"completed_review,omitempty"`
	FindingsPresent    bool `json:"findings_present,omitempty"`
	FixedAndVerified   bool `json:"fixed_and_verified,omitempty"`
	AcceptanceAdvisory bool `json:"acceptance_advisory,omitempty"`
}

type TaskFailureClass string

const (
	FailureContract    TaskFailureClass = "contract"
	FailureEnvironment TaskFailureClass = "environment"
	FailureExecution   TaskFailureClass = "execution"
	FailureProtocol    TaskFailureClass = "protocol"
	FailureVerify      TaskFailureClass = "verification"
	FailurePolicy      TaskFailureClass = "policy"
	FailureTimeout     TaskFailureClass = "timeout"
	FailureCancelled   TaskFailureClass = "cancelled"
)

// SummarizeRunStats aggregates canonical statistics over a slice of TodoItems.
func SummarizeRunStats(items []*TodoItem) RunStats {
	stats := RunStats{}
	for _, item := range items {
		if item == nil {
			continue
		}
		stats.TasksTotal++
		stats.AttemptsTotal += 1 + item.Retries
		// Every retry represents a failed attempt, including a task that
		// eventually succeeded. The final attempt is failed as well when the
		// task remains in an error/blocked state.
		stats.AttemptsFailed += item.Retries
		switch item.Status {
		case TaskDone:
			stats.TasksDone++
		case TaskError, TaskBlocked:
			if item.Resolution != nil && (item.Resolution.Status == "superseded" || item.Resolution.Status == "reconciled" || item.Resolution.Status == "waived") {
				// Resolved failure, not counted as unresolved task
			} else {
				stats.TasksUnresolved++
				stats.AttemptsFailed++
			}
		case TaskSkipped:
		default:
			if item.Status == TaskPending || item.Status == TaskInProgress || item.Status == TaskPlanned || item.Status == TaskVerifying || item.Status == TaskPaused || item.Status == TaskProtocolIncomplete {
				stats.TasksUnresolved++
			}
		}
	}
	return stats
}

// toTaskReference converts a TodoItem to a TaskReference.
func toTaskReference(item *TodoItem) TaskReference {
	if item == nil {
		return TaskReference{}
	}
	errStr := FailureDisplayText(item)
	failureClass := TaskFailureClass("")
	if item.FailureEvent != nil {
		failureClass = item.FailureEvent.FailureClass
	}
	return TaskReference{
		ID:           item.ID,
		Agent:        item.Agent,
		Desc:         item.Desc,
		Status:       string(item.Status),
		Error:        errStr,
		FailureClass: failureClass,
	}
}

// toTaskReferences converts a slice of TodoItems to TaskReferences.
func toTaskReferences(items []*TodoItem) []TaskReference {
	if len(items) == 0 {
		return nil
	}
	refs := make([]TaskReference, 0, len(items))
	for _, item := range items {
		if item != nil {
			refs = append(refs, toTaskReference(item))
		}
	}
	return refs
}

// UnresolvedTaskReferences projects the non-terminal canonical task states
// into evaluator input. It is intentionally a state-only conversion: it does
// not inspect task text or infer outcome from descriptions.
func UnresolvedTaskReferences(items []*TodoItem) []TaskReference {
	if len(items) == 0 {
		return nil
	}
	unresolved := make([]*TodoItem, 0, len(items))
	for _, item := range items {
		if item == nil || !isUnresolvedTaskStatus(item.Status) {
			continue
		}
		if item.Resolution != nil && (item.Resolution.Status == "superseded" || item.Resolution.Status == "reconciled" || item.Resolution.Status == "waived") {
			continue
		}
		unresolved = append(unresolved, item)
	}
	return toTaskReferences(unresolved)
}

func isUnresolvedTaskStatus(status TaskStatus) bool {
	switch status {
	case TaskPending, TaskPlanned, TaskInProgress, TaskPaused, TaskVerifying, TaskProtocolIncomplete, TaskError, TaskBlocked:
		return true
	default:
		return false
	}
}

// ValidateResolution checks a TaskResolution for validity, evidence requirements, and N-node cycle prevention.
func ValidateResolution(resolution *TaskResolution, itemID string, allItems []*TodoItem, runID string) error {
	if resolution == nil {
		return nil
	}
	switch resolution.Status {
	case "unresolved", "superseded", "reconciled", "waived":
	default:
		return fmt.Errorf("invalid resolution status %q", resolution.Status)
	}

	// 1. The target item being resolved MUST be in terminal failed or blocked status (TaskError / TaskBlocked)
	var targetItem *TodoItem
	for _, it := range allItems {
		if it != nil && it.ID == itemID {
			targetItem = it
			break
		}
	}
	if targetItem != nil {
		if targetItem.Status != TaskError && targetItem.Status != TaskBlocked {
			return fmt.Errorf("task %s has status %q; only failed or blocked tasks can be resolved", itemID, targetItem.Status)
		}
	}

	if resolution.Status == "superseded" || resolution.Status == "reconciled" {
		if resolution.ResolvedBy == "" {
			return fmt.Errorf("resolution status %q requires resolved_by task ID", resolution.Status)
		}
		if resolution.ResolvedBy == itemID {
			return fmt.Errorf("task %s cannot resolve itself", itemID)
		}

		// 2. Resolver task MUST exist and be in TaskDone status
		var resolver *TodoItem
		for _, it := range allItems {
			if it != nil && it.ID == resolution.ResolvedBy {
				resolver = it
				break
			}
		}
		if resolver == nil {
			return fmt.Errorf("resolving task %s not found in todo list", resolution.ResolvedBy)
		}
		if resolver.Status != TaskDone {
			return fmt.Errorf("resolving task %s must be done (current status: %s)", resolution.ResolvedBy, resolver.Status)
		}

		// 3. Objective evidence check: resolver task MUST have passed objective verification (VerifyResult exit code 0) or contain verified TypedResult evidence with a valid system HMAC signature. Model claims or un-signed self-authored evidenceRefs are rejected.
		hasVerification := resolver.VerifyResult != nil && resolver.VerifyResult.ExitCode == 0
		hasTypedEvidence := false
		if resolver.TypedResult != nil && len(resolver.TypedResult.Evidence) > 0 {
			sec, err := GetSystemSecret()
			if err == nil && sec != "" {
				for _, ev := range resolver.TypedResult.Evidence {
					if VerifyEvidenceSignature(ev, sec, resolver.ID, runID) {
						hasTypedEvidence = true
						break
					}
				}
			}
		}
		if !hasVerification && !hasTypedEvidence {
			return fmt.Errorf("resolving task %s lacks objective verification evidence (must have passing verify result or system-signed evidence signature)", resolution.ResolvedBy)
		}

		// 4. Graph Cycle Check (N-node cycle traversal starting from resolver.ID)
		visited := map[string]bool{itemID: true}
		currID := resolution.ResolvedBy
		for currID != "" {
			if visited[currID] {
				return fmt.Errorf("resolution cycle detected involving task %s", currID)
			}
			visited[currID] = true
			var currItem *TodoItem
			for _, it := range allItems {
				if it != nil && it.ID == currID {
					currItem = it
					break
				}
			}
			if currItem != nil && currItem.Resolution != nil && (currItem.Resolution.Status == "superseded" || currItem.Resolution.Status == "reconciled") {
				currID = currItem.Resolution.ResolvedBy
			} else {
				break
			}
		}
	}
	return nil
}
