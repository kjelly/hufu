package team

// RetryDisposition is the recovery action prescribed by DecideRecovery for a
// failed task attempt. It is the single decision point that determines
// whether the retry loop retries, stops, or blocks for reconciliation.
//
// Refs: docs/hufu-generic-task-reliability-mechanisms.md §5, §6.1, WP-07
type RetryDisposition string

const (
	// RetryNone — do not retry; the attempt was the last one (budget
	// exhausted, context cancelled, or terminal cancelled).
	RetryNone RetryDisposition = "none"
	// RetryWorker — re-dispatch the worker agent for another attempt.
	RetryWorker RetryDisposition = "retry_worker"
	// ReconcileOnly — do not replay worker tools; block the task for
	// reconciliation via read-only evidence or a reconcile command.
	ReconcileOnly RetryDisposition = "reconcile_only"
	// ReplanRequired — stop retrying; the coordinator must produce a new
	// recovery hypothesis or replan the task before any further dispatch.
	ReplanRequired RetryDisposition = "replan_required"
	// NeedsHuman — stop and escalate; the failure requires human
	// intervention (e.g. an active terminal session, systemic defect).
	NeedsHuman RetryDisposition = "needs_human"
)

const (
	retrySuppressionRepeatedFingerprint = "repeated_fingerprint"
	retrySuppressionEvidenceIncomplete  = "evidence_incomplete"
	retrySuppressionUnfixableVerifier   = "unfixable_verifier"
	retrySuppressionRejectedStrategy    = "rejected_recovery_strategy"
	retrySuppressionAntiThrashingLimit  = "anti_thrashing_limit"
)

// RecoveryDecisionInput carries the structured signals that DecideRecovery uses
// to prescribe a RetryDisposition. It combines the §5 failure-class taxonomy,
// the §6.1 retry-policy rules, and the five loop-level early-break signals
// that previously lived as separate if-statements in the retry loop.
//
// All fields specified by §6.1 (FailureClass, SideEffect, RecoveryPolicy,
// Attempt, MaxRetries, EvidenceComplete, FailureFingerprint,
// PreviousFingerprint) are read by DecideRecovery and influence the
// disposition.
//
// Refs: docs/hufu-generic-task-reliability-mechanisms.md §5, §6.1, WP-07, WP-08
type RecoveryDecisionInput struct {
	// FailureClass is the structured classification of the current attempt's
	// failure (§5).
	FailureClass TaskFailureClass
	// SideEffect is the task's side-effect class, used by §6.1 to decide
	// whether a timeout warrants reconcile-first.
	SideEffect SideEffectClass
	// RecoveryPolicy is the task's resolved recovery policy (retry /
	// reconcile / manual / never). This is the value from
	// ResolveRecoveryPolicy, which incorporates execution-profile defaults
	// (§6.1: 僅 none 或可安全重放的 workspace_write，才可在明確 policy 下
	// 重試 execution). DecideRecovery uses this to gate RetryWorker so a
	// profile that resolves to reconcile/manual/never cannot be bypassed
	// by the raw task.Recovery field.
	RecoveryPolicy RecoveryPolicy
	// Attempt is the current attempt number (1-based).
	Attempt int
	// MaxRetries is the configured retry budget.
	MaxRetries int
	// EvidenceComplete indicates whether execution evidence (transcript,
	// tool calls, artifacts) was fully captured for this attempt. When
	// false, DecideRecovery will not prescribe RetryWorker because the
	// retry prompt cannot include the required class, evidence, and
	// last command/exit (§6.1: retry prompt 必須包含 class、證據、上次
	// command/exit、以及明確可改變的欄位).
	EvidenceComplete bool
	// FailureFingerprint is the normalised digest of the current failure
	// (§6.1, §6.2). When non-empty and matching PreviousFingerprint, the
	// failure is a repeat and must escalate to replan_required. This is
	// the canonical repeat-detection mechanism — not raw err.Error()
	// comparison.
	FailureFingerprint string
	// PreviousFingerprint is the digest of the previous attempt's failure,
	// or empty on the first attempt. A non-empty match with
	// FailureFingerprint indicates a repeated failure (§6.1: 相同
	// FailureFingerprint 不得無限制重試).
	PreviousFingerprint string

	// --- Loop-level signals (WP-08: five early-break paths as inputs) ---

	// TerminalBlocked is true when an owned terminal session is still
	// active or in an unknown state, blocking safe retry (path 1).
	TerminalBlocked bool
	// ProtocolFailure is true when the agent finished execution but
	// omitted submit_result (path 2 prerequisite).
	ProtocolFailure bool
	// UnfixableVerify is true when the failure is a wrong-polarity verify
	// command that the worker cannot fix by retrying (path 4).
	UnfixableVerify bool
	// SameFailureRepeated is a legacy fallback for repeat detection used
	// only when FailureFingerprint/PreviousFingerprint are both empty
	// (e.g. unit tests without fingerprint computation).
	SameFailureRepeated bool
	// ContextCancelled is true when the parent context has been cancelled
	// (deadline, budget, or user SIGINT propagation).
	ContextCancelled bool
	// Replayable is true when CanAutomaticallyReplay(task) is true — the
	// task's structural side effect and raw recovery policy allow
	// automatic worker replay (paths 2 and 3). The resolved
	// RecoveryPolicy is additionally checked for the class-based
	// disposition to ensure profile policies are not bypassed.
	Replayable bool
	// ProtocolRepairRetry is true when protocolRepairAllowsRetry(task) is
	// true — the resolved recovery policy permits retrying after a protocol
	// repair failure (path 2).
	ProtocolRepairRetry bool
}

// isRepeatedFailure derives repeat detection from the fingerprint inputs
// (§6.1). When both fingerprints are non-empty, the normalised digest
// comparison is used. When fingerprints are empty (e.g. unit tests), the
// legacy SameFailureRepeated flag is used as a fallback.
func isRepeatedFailure(in RecoveryDecisionInput) bool {
	if in.FailureFingerprint != "" && in.PreviousFingerprint != "" {
		return in.FailureFingerprint == in.PreviousFingerprint
	}
	return in.SameFailureRepeated
}

// retrySuppressionReason identifies a safety guard that prevented a worker
// replay. It intentionally excludes cancellation, ordinary retry-budget
// exhaustion, and policy dispositions that are not evidence/fingerprint
// suppression; those remain observable through their existing events.
func retrySuppressionReason(in RecoveryDecisionInput, disposition RetryDisposition) (string, bool) {
	if disposition == RetryWorker || in.ContextCancelled || in.FailureClass == FailureCancelled || in.Attempt >= in.MaxRetries {
		return "", false
	}
	if isRepeatedFailure(in) {
		return retrySuppressionRepeatedFingerprint, true
	}
	if in.UnfixableVerify {
		return retrySuppressionUnfixableVerifier, true
	}
	if !in.EvidenceComplete && in.Replayable && !in.TerminalBlocked && !in.ProtocolFailure {
		return retrySuppressionEvidenceIncomplete, true
	}
	return "", false
}

// DecideRecovery is the single decision point that prescribes a
// RetryDisposition for a failed task attempt. It implements the §5
// failure-class → disposition mapping and the §6.1 retry-policy rules,
// while preserving the five early-break paths that previously existed as
// separate if-statements in the retry loop.
//
// Cancellation is checked first because it is not a failure eligible for
// recovery. The five early-break paths then preserve their original safety
// ordering, while non-retry failure classes retain their §5 disposition even
// after the worker retry budget is exhausted. Finally, the resolved
// RecoveryPolicy and EvidenceComplete gate RetryWorker to ensure profile
// policies are not bypassed (§6.1).
//
// Refs: docs/hufu-generic-task-reliability-mechanisms.md §5, §5.3, §6.1, WP-07, WP-08
func DecideRecovery(in RecoveryDecisionInput) (RetryDisposition, string) {
	// §5.3: cancellation is never a worker failure. Whether it originated
	// from SIGINT, a parent deadline/budget, or a child that observed its
	// cancelled context, it must not enter retry, fingerprint, or
	// anti-thrashing paths. Check it before safety/replay gates so a cancelled
	// non-replayable task remains an honest no-retry outcome rather than being
	// recast as a reconciliation failure.
	if in.ContextCancelled || in.FailureClass == FailureCancelled {
		return RetryNone, "cancelled"
	}

	// Path 1: terminal blocked — an owned terminal session is still active
	// or in an unknown state. Retrying is unsafe; escalate to human.
	if in.TerminalBlocked {
		return NeedsHuman, "an owned terminal session remains active or unknown"
	}

	// These classes never permit worker replay, regardless of retry budget or
	// task replayability. The budget only limits RetryWorker; it must not erase
	// the actionable disposition required to repair a broken contract or
	// protocol boundary.
	switch in.FailureClass {
	case FailureContract:
		return ReplanRequired, "contract failure requires replan"
	case FailureEnvironment:
		return ReplanRequired, "environment failure requires replan"
	case FailurePolicy:
		return ReplanRequired, "policy failure requires replan"
	case FailureProtocol:
		return ReconcileOnly, "protocol failure requires result-only repair; worker tools must not be replayed"
	case FailureTimeout:
		if nonReplayableSideEffect(in.SideEffect) {
			return ReconcileOnly, "timeout on non-replayable side effect; reconcile first"
		}
	}

	// Path 2: protocol-only failure on a task whose side effect or recovery
	// policy disallows automatic replay. Preserve evidence and block for
	// reconciliation; do not replay worker tools (§6.1: protocol 只允許
	// result-only repair；不得重放工具).
	if in.ProtocolFailure && (!in.Replayable || !in.ProtocolRepairRetry) {
		return ReconcileOnly, "protocol-only failure cannot be automatically replayed; reconciliation required"
	}

	// Path 3: non-replayable task — the side effect or recovery policy
	// requires reconciliation before any second execution.
	if !in.Replayable {
		return ReconcileOnly, "task replay policy requires reconciliation"
	}

	// Derive repeat detection from fingerprint inputs (§6.1).
	repeated := isRepeatedFailure(in)

	// Path 4: unfixable verify failure — a wrong-polarity verify command set
	// by the coordinator at task-assignment time. The worker cannot fix it
	// by retrying, so stop early when there are remaining attempts. On the
	// last attempt, fall through to the budget check (matching the
	// pre-refactoring loop which gated this on attempt < maxRetries).
	if in.UnfixableVerify && in.Attempt < in.MaxRetries {
		return ReplanRequired, "verify command has unfixable wrong polarity"
	}

	// Path 5: same failure repeated — the agent is stuck repeating the same
	// failing action. Stop early when there are remaining attempts (§6.1:
	// 相同 FailureFingerprint 不得無限制重試).
	if repeated && in.Attempt < in.MaxRetries {
		return ReplanRequired, "same failure repeated"
	}

	// Budget exhausted — no remaining attempts. The loop would exit
	// naturally, but returning RetryNone makes the decision explicit.
	if in.Attempt >= in.MaxRetries {
		return RetryNone, "retry budget exhausted"
	}

	// Class-based disposition (§5 table, §6.1 rules). These apply only when
	// none of the five early-break paths triggered and the retry budget has
	// not been exhausted.
	disposition, reason := classBasedDisposition(in)

	// §6.1 post-check: the resolved RecoveryPolicy gates RetryWorker so a
	// profile that resolves to reconcile/manual/never cannot be bypassed
	// by the raw task.Recovery field used in CanAutomaticallyReplay.
	if disposition == RetryWorker {
		switch in.RecoveryPolicy {
		case RecoveryReconcile:
			return ReconcileOnly, "resolved recovery policy is reconcile; block for reconciliation"
		case RecoveryManual:
			return NeedsHuman, "resolved recovery policy is manual; human intervention required"
		case RecoveryNever:
			return RetryNone, "resolved recovery policy is never; no retry"
		}
		// §6.1: evidence must be complete to attempt a retry. The retry
		// prompt must include class, evidence, last command/exit, and
		// the fields that can change. Without complete evidence, the
		// retry cannot meet this requirement.
		if !in.EvidenceComplete {
			return ReplanRequired, "evidence incomplete; cannot retry without complete evidence"
		}
	}

	return disposition, reason
}

// classBasedDisposition implements the §5 failure-class → disposition
// mapping for replayable tasks where none of the five early-break paths
// trigger and the retry budget has not been exhausted.
func classBasedDisposition(in RecoveryDecisionInput) (RetryDisposition, string) {
	switch in.FailureClass {
	case FailureTimeout:
		return RetryWorker, "timeout on replayable task"
	case FailureExecution:
		// §5: genuine execution failures may retry the worker, subject to
		// the retry budget (already checked above) and the resolved
		// recovery policy (checked by the caller).
		return RetryWorker, "execution failure"
	case FailureVerify:
		// §5: verification failures (artifact exists but assertion fails)
		// may retry the worker; if the verifier itself is broken, the class
		// would be FailureContract (caught above).
		return RetryWorker, "verification failure"
	case FailureCancelled:
		// Handled before the retry budget; retained for exhaustive enum
		// coverage when this helper is called independently.
		return RetryNone, "cancelled"
	default:
		return RetryWorker, "retry"
	}
}

// IsRetryDisposition returns true when the disposition allows the worker to
// be re-dispatched for another attempt.
func IsRetryDisposition(d RetryDisposition) bool {
	return d == RetryWorker
}

// IsStopDisposition returns true when the disposition causes the retry loop
// to stop (break) without re-dispatching the worker.
func IsStopDisposition(d RetryDisposition) bool {
	switch d {
	case RetryNone, ReconcileOnly, ReplanRequired, NeedsHuman:
		return true
	default:
		return false
	}
}

// ShouldBlockTask returns true when the disposition requires the task to be
// set to TaskBlocked for reconciliation.
func ShouldBlockTask(d RetryDisposition) bool {
	return d == ReconcileOnly
}
