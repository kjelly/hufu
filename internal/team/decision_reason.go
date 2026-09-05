package team

import "github.com/kjelly/hufu/internal/agent"

// Canonical decision reason codes and event type names, re-exported from
// internal/agent so runtime call sites read naturally while a single list
// stays authoritative (docs/hufu-decision-aware-runtime-spec.md §36-§37).

// Failure reason codes (spec §37).
const (
	ReasonDecisionProfileUnknown            = agent.ReasonDecisionProfileUnknown
	ReasonDecisionMissingObjective          = agent.ReasonDecisionMissingObjective
	ReasonDecisionMissingAlternative        = agent.ReasonDecisionMissingAlternative
	ReasonDecisionNoNoGoOption              = agent.ReasonDecisionNoNoGoOption
	ReasonDecisionOutsideViewMissing        = agent.ReasonDecisionOutsideViewMissing
	ReasonDecisionPremortemRequired         = agent.ReasonDecisionPremortemRequired
	ReasonDecisionBudgetInsufficient        = agent.ReasonDecisionBudgetInsufficient
	ReasonDecisionOpinionInvalid            = agent.ReasonDecisionOpinionInvalid
	ReasonDecisionInsufficientValidOpinions = agent.ReasonDecisionInsufficientValidOpinions
	ReasonDecisionEvidenceNotSealed         = agent.ReasonDecisionEvidenceNotSealed
	ReasonDecisionStale                     = agent.ReasonDecisionStale

	ReasonCommitGateMissingRecovery      = agent.ReasonCommitGateMissingRecovery
	ReasonCommitGateMissingReconcile     = agent.ReasonCommitGateMissingReconcile
	ReasonCommitGateMissingObservability = agent.ReasonCommitGateMissingObservability
	ReasonCommitGateMissingVerification  = agent.ReasonCommitGateMissingVerification
	ReasonCommitGateMissingEvidence      = agent.ReasonCommitGateMissingEvidence

	ReasonStopPolicyMissingKillCriteria = agent.ReasonStopPolicyMissingKillCriteria
	ReasonStopPolicyUnknownKillKind     = agent.ReasonStopPolicyUnknownKillKind
	ReasonKillCriterionReached          = agent.ReasonKillCriterionReached

	ReasonAssumptionInvalidated = agent.ReasonAssumptionInvalidated

	ReasonReconcileRequired     = agent.ReasonReconcileRequired
	ReasonReconcileUnknownState = agent.ReasonReconcileUnknownState
)

// Runtime event type names (spec §36).
const (
	EventDecisionStarted              = agent.EventDecisionStarted
	EventDecisionEvidenceSealed       = agent.EventDecisionEvidenceSealed
	EventDecisionEvidenceChanged      = agent.EventDecisionEvidenceChanged
	EventDecisionReferenceCompleted   = agent.EventDecisionReferenceCompleted
	EventDecisionOpinionSubmitted     = agent.EventDecisionOpinionSubmitted
	EventDecisionOpinionRejected      = agent.EventDecisionOpinionRejected
	EventDecisionJudgeOverallIgnored  = agent.EventDecisionJudgeOverallIgnored
	EventDecisionAggregateComputed    = agent.EventDecisionAggregateComputed
	EventDecisionChallengeSubmitted   = agent.EventDecisionChallengeSubmitted
	EventDecisionChallengeSkipped     = agent.EventDecisionChallengeSkipped
	EventDecisionPremortemSubmitted   = agent.EventDecisionPremortemSubmitted
	EventDecisionRevisionSubmitted    = agent.EventDecisionRevisionSubmitted
	EventDecisionFinalized            = agent.EventDecisionFinalized
	EventDecisionFinalizationOverride = agent.EventDecisionFinalizationOverride
	EventDecisionAlternativesOverride = agent.EventDecisionAlternativesOverride
	EventDecisionBudgetDegraded       = agent.EventDecisionBudgetDegraded
	EventDecisionEvidenceSharedOrigin = agent.EventDecisionEvidenceSharedOrigin
	EventDecisionInvalidated          = agent.EventDecisionInvalidated

	EventAssumptionDeclared     = agent.EventAssumptionDeclared
	EventAssumptionSupported    = agent.EventAssumptionSupported
	EventAssumptionContradicted = agent.EventAssumptionContradicted
	EventAssumptionStale        = agent.EventAssumptionStale

	EventReplanRequested = agent.EventReplanRequested
	EventReplanCompleted = agent.EventReplanCompleted

	EventCommitGateBlocked      = agent.EventCommitGateBlocked
	EventKillCriterionTriggered = agent.EventKillCriterionTriggered
)

// AssumptionStatusEvent maps an assumption lifecycle state to its event type
// so status transitions are always recorded with a stable name (spec §18.1).
func AssumptionStatusEvent(status string) string {
	switch status {
	case AssumptionSupported:
		return EventAssumptionSupported
	case AssumptionContradicted:
		return EventAssumptionContradicted
	case AssumptionStale:
		return EventAssumptionStale
	default:
		return EventAssumptionDeclared
	}
}
