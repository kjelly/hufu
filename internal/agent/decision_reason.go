package agent

// Canonical failure reason codes and runtime event type names for the
// decision-aware runtime (docs/architecture/decision-runtime.md §36-§37).
//
// They are defined in the agent package so both the configuration layer (this
// package) and the runtime layer (internal/team) can reference one list;
// internal/team re-exports them so runtime call sites read naturally.

// Failure reason codes (spec §37).
const (
	ReasonDecisionProfileUnknown            = "decision_profile_unknown"
	ReasonDecisionMissingObjective          = "decision_missing_objective"
	ReasonDecisionMissingAlternative        = "decision_missing_alternative"
	ReasonDecisionNoNoGoOption              = "decision_no_no_go_option"
	ReasonDecisionOutsideViewMissing        = "decision_outside_view_missing"
	ReasonDecisionPremortemRequired         = "decision_premortem_required"
	ReasonDecisionBudgetInsufficient        = "decision_budget_insufficient"
	ReasonDecisionOpinionInvalid            = "decision_opinion_invalid"
	ReasonDecisionInsufficientValidOpinions = "decision_insufficient_valid_opinions"
	ReasonDecisionEvidenceNotSealed         = "decision_evidence_not_sealed"
	ReasonDecisionCanonicalFormChanged      = "decision_canonical_form_changed"
	ReasonDecisionStale                     = "decision_stale"

	ReasonCommitGateMissingRecovery      = "commit_gate_missing_recovery"
	ReasonCommitGateMissingReconcile     = "commit_gate_missing_reconcile"
	ReasonCommitGateMissingObservability = "commit_gate_missing_observability"
	ReasonCommitGateMissingVerification  = "commit_gate_missing_verification"
	ReasonCommitGateMissingEvidence      = "commit_gate_missing_evidence"

	ReasonStopPolicyMissingKillCriteria = "stop_policy_missing_kill_criteria"
	ReasonStopPolicyUnknownKillKind     = "stop_policy_unknown_kill_kind"
	ReasonKillCriterionReached          = "kill_criterion_reached"

	ReasonAssumptionInvalidated = "assumption_invalidated"

	ReasonReconcileRequired     = "reconcile_required"
	ReasonReconcileUnknownState = "reconcile_unknown_state"
)

// Runtime event type names (spec §36). They follow the existing snake_case
// convention used by internal/team's event store.
const (
	EventDecisionStarted             = "decision_started"
	EventDecisionOptionsProposed     = "decision_options_proposed"
	EventDecisionEvidenceSealed      = "decision_evidence_sealed"
	EventDecisionEvidenceChanged     = "decision_evidence_changed"
	EventDecisionReferenceCompleted  = "decision_reference_completed"
	EventDecisionReferenceStarted    = "decision_reference_started"
	EventDecisionReferenceFailed     = "decision_reference_failed"
	EventDecisionOpinionSubmitted    = "decision_opinion_submitted"
	EventDecisionOpinionRejected     = "decision_opinion_rejected"
	EventDecisionJudgeOverallIgnored = "decision_judge_overall_ignored"
	EventDecisionAggregateComputed   = "decision_aggregate_computed"
	EventDecisionChallengeSubmitted  = "decision_challenge_submitted"
	EventDecisionChallengeSkipped    = "decision_challenge_skipped"
	EventDecisionPremortemSubmitted  = "decision_premortem_submitted"
	EventDecisionRevisionSubmitted   = "decision_revision_submitted"
	EventDecisionFinalizationResult  = "decision_finalization_result"
	// EventDecisionFinalizationOverride records a finalizer choosing an option
	// the aggregate did not lead with. Spec §26 requires it as its own event:
	// an override is the one finalization outcome a reader must be able to
	// find without reconstructing the aggregate to compare against.
	EventDecisionFinalizationOverride = "decision_finalization_override"
	EventDecisionFinalized            = "decision_finalized"
	EventDecisionAlternativesOverride = "decision_alternatives_override"
	EventDecisionBudgetDegraded       = "decision_budget_degraded"
	EventDecisionEvidenceSharedOrigin = "decision_evidence_shared_origin"
	EventDecisionInvalidated          = "decision_invalidated"
	EventDecisionAdmitted             = "decision_admitted"
	EventDecisionRunEnvelopeAnchored  = "decision_run_envelope_anchored"
	EventRequestContractCommitted     = "request_contract_committed"

	EventAssumptionDeclared     = "assumption_declared"
	EventAssumptionSupported    = "assumption_supported"
	EventAssumptionContradicted = "assumption_contradicted"
	EventAssumptionStale        = "assumption_stale"

	EventReplanRequested = "replan_requested"
	EventReplanCompleted = "replan_completed"

	EventCommitGateBlocked      = "commit_gate_blocked"
	EventKillCriterionTriggered = "kill_criterion_triggered"
)

// DecisionReasonCodes lists every canonical reason code. Tests assert this set
// so a code can never be silently renamed or dropped.
var DecisionReasonCodes = []string{
	ReasonDecisionProfileUnknown,
	ReasonDecisionMissingObjective,
	ReasonDecisionMissingAlternative,
	ReasonDecisionNoNoGoOption,
	ReasonDecisionOutsideViewMissing,
	ReasonDecisionPremortemRequired,
	ReasonDecisionBudgetInsufficient,
	ReasonDecisionOpinionInvalid,
	ReasonDecisionInsufficientValidOpinions,
	ReasonDecisionEvidenceNotSealed,
	ReasonDecisionCanonicalFormChanged,
	ReasonDecisionStale,
	ReasonCommitGateMissingRecovery,
	ReasonCommitGateMissingReconcile,
	ReasonCommitGateMissingObservability,
	ReasonCommitGateMissingVerification,
	ReasonCommitGateMissingEvidence,
	ReasonStopPolicyMissingKillCriteria,
	ReasonStopPolicyUnknownKillKind,
	ReasonKillCriterionReached,
	ReasonAssumptionInvalidated,
	ReasonReconcileRequired,
	ReasonReconcileUnknownState,
}

// DecisionEventTypes lists every canonical decision event type.
var DecisionEventTypes = []string{
	EventDecisionStarted,
	EventDecisionOptionsProposed,
	EventDecisionEvidenceSealed,
	EventDecisionEvidenceChanged,
	EventDecisionReferenceStarted,
	EventDecisionReferenceCompleted,
	EventDecisionReferenceFailed,
	EventDecisionOpinionSubmitted,
	EventDecisionOpinionRejected,
	EventDecisionJudgeOverallIgnored,
	EventDecisionAggregateComputed,
	EventDecisionChallengeSubmitted,
	EventDecisionChallengeSkipped,
	EventDecisionPremortemSubmitted,
	EventDecisionRevisionSubmitted,
	EventDecisionFinalizationResult,
	EventDecisionFinalized,
	EventDecisionAlternativesOverride,
	EventDecisionBudgetDegraded,
	EventDecisionEvidenceSharedOrigin,
	EventDecisionInvalidated,
	EventDecisionAdmitted,
	EventDecisionRunEnvelopeAnchored,
	EventRequestContractCommitted,
	EventAssumptionDeclared,
	EventAssumptionSupported,
	EventAssumptionContradicted,
	EventAssumptionStale,
	EventReplanRequested,
	EventReplanCompleted,
	EventCommitGateBlocked,
	EventKillCriterionTriggered,
}
