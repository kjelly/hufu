package team

import "github.com/kjelly/hufu/internal/cost"

// EventType is the stable catalog identifier for a runtime event. RunEvent
// deliberately keeps Type as a string so existing JSONL workspaces and API
// callers remain source and wire compatible while producers migrate.
type EventType string

const (
	EventRunStarted                              EventType = "run_started"
	EventRunFinished                             EventType = "run_finished"
	EventWrapUpPhase                             EventType = "wrap_up_phase"
	EventRunCancellationRequested                EventType = "run_cancellation_requested"
	EventUserMessageAdded                        EventType = "user_message_added"
	EventAssistantMessageAdded                   EventType = "assistant_message_added"
	EventTaskCreated                             EventType = "task_created"
	EventTaskPlanned                             EventType = "task_planned"
	EventTaskStarted                             EventType = "task_started"
	EventTaskVerifying                           EventType = "task_verifying"
	EventTaskPaused                              EventType = "task_paused"
	EventTaskCompleted                           EventType = "task_completed"
	EventTaskFailed                              EventType = "task_failed"
	EventTaskBlocked                             EventType = "task_blocked"
	EventTaskSkipped                             EventType = "task_skipped"
	EventTaskProtocolIncomplete                  EventType = "task_protocol_incomplete"
	EventTaskCancelled                           EventType = "task_cancelled"
	EventTaskRemoved                             EventType = "task_removed"
	EventDynamicToolUnavailable                  EventType = "dynamic_tool_unavailable"
	EventContextArtifactPublished                EventType = "context_artifact_published"
	EventStaticToolGrantNarrowed                 EventType = "static_tool_grant_narrowed"
	EventResourceClaimsResolved                  EventType = "resource_claims_resolved"
	EventTaskResolution                          EventType = "task_resolution"
	EventArtifactCreated                         EventType = "artifact_created"
	EventCriterionReevaluated                    EventType = "criterion_re_evaluated"
	EventCriterionCheckpoint                     EventType = "criterion_checkpoint_saved"
	EventMemoryRetrieved                         EventType = "memory_retrieved"
	EventMemoryUsageRecorded                     EventType = "memory_usage_recorded"
	EventMemoryOutcomeRecorded                   EventType = "memory_outcome_recorded"
	EventPolicyDecision                          EventType = "policy_decision"
	EventRecoveryDecision                        EventType = "recovery_decision"
	EventWorkflowStateChanged                    EventType = "workflow_state_changed"
	EventCoordinatorCompactionCommitted          EventType = "coordinator_compaction_committed"
	EventCoordinatorCompactionCheckpointAttested EventType = "coordinator_compaction_checkpoint_attested"
	EventCoordinatorModelContinuationAdmitted    EventType = "coordinator_model_continuation_admitted"
	EventContextWindowAdmission                  EventType = "context_window_admission"
	EventContextWindowCompactionCommitted        EventType = "context_window_compaction_committed"
	EventContextWindowDownshift                  EventType = "context_window_downshift"
	EventModelProfileResolved                    EventType = "model_profile_resolved"
	EventExecutionPolicySnapshot                 EventType = "execution_policy_snapshot"
	EventRunInputsResolved                       EventType = "run_inputs_resolved"
	EventDecisionPrimitiveStarted                EventType = "decision_primitive_started"
	EventDecisionPrimitiveSettled                EventType = "decision_primitive_settled"
	EventDecisionAdmitted                        EventType = "decision_admitted"
	EventDecisionRunOpened                       EventType = "decision_run_opened"
	EventDecisionRunAttached                     EventType = "decision_run_attached"
	EventPrimaryDecisionPrepared                 EventType = "primary_decision_prepared"
	EventPrimaryDecisionAdmitted                 EventType = "primary_decision_admitted"
	EventDecisionRoleCallStarted                 EventType = "decision_role_call_started"
	EventDecisionRoleCallUnconfirmed             EventType = "decision_role_call_unconfirmed"
	EventDecisionRoleCallSettled                 EventType = "decision_role_call_settled"
	EventPrimaryDecisionBlocked                  EventType = "primary_decision_blocked"
	EventPrimaryDecisionBound                    EventType = "primary_decision_bound"
	EventPrimaryDecisionInvalidated              EventType = "primary_decision_invalidated"
	// EventProviderSessionBound records an external SubagentProvider's
	// durable session identity (e.g. a Codex thread_id) established mid-attempt,
	// after task_created but before the first turn that depends on it
	// (docs/architecture/execution-runtime.md: "persist
	// provider_session_bound" happens between thread/start and turn/start).
	// EventProviderSessionBound remains a legacy replay-only event name.
	EventProviderSessionBound EventType = "provider_session_bound"
	// EventBackendSessionBound is the canonical mutable backend-session event.
	EventBackendSessionBound     EventType = "backend_session_bound"
	EventExecutionTargetMigrated EventType = "execution_target_migrated"
	// EventExecutionCompatibilityMigrated is the self-contained canonical
	// replacement for a legacy task execution identity. It is written only by
	// the explicit append-only compatibility materializer.
	EventExecutionCompatibilityMigrated EventType = "execution_compatibility_migrated"
	// EventExecutionPolicySnapshotMigrated carries a current canonical policy
	// snapshot for one historical v3 snapshot subject.
	EventExecutionPolicySnapshotMigrated EventType = "execution_policy_snapshot_migrated"
	// EventExecutionCompatibilityObserved is a metadata-only observation that
	// a run encountered still-actionable legacy execution state. Its payload
	// contains feature counts only; it never carries the workspace subjects
	// that led to the observation.
	EventExecutionCompatibilityObserved EventType = "execution_compatibility_observed"
	// EventResourceLocked records a durably admitted LockedResourceSet
	// (spec.md "Generic Required Resource Lock"). The payload is
	// metadata-only — canonical path, sha256, byte size — never content
	// (runtime invariant 9).
	EventResourceLocked EventType = "resource_locked"
	// EventWorkspaceSnapshotCommitted is the durable commit point of one
	// workspace version snapshot (docs/archive/implementation-plans/
	// workspace-versioning.md §16). The payload carries only IDs, hashes, and
	// counts, never file content.
	EventWorkspaceSnapshotCommitted EventType = "workspace_snapshot_committed"
	// Attempt-world lifecycle (isolated worker workspaces). Payloads carry
	// world IDs, digests, counts, and paths; never file content. The
	// apply_started payload also carries the verified result the task
	// completes with, so a crash after the apply can finish the task
	// without re-running the worker.
	EventAttemptWorkspacePrepared        EventType = "attempt_workspace_prepared"
	EventAttemptWorkspaceApplyStarted    EventType = "attempt_workspace_apply_started"
	EventAttemptWorkspaceApplyConflicted EventType = "attempt_workspace_apply_conflicted"
	EventAttemptWorkspaceApplied         EventType = "attempt_workspace_applied"
	EventAttemptWorkspaceDiscarded       EventType = "attempt_workspace_discarded"
	EventAttemptWorkspaceOrphanRemoved   EventType = "attempt_workspace_orphan_removed"
	// EventExecutionFallbackDecided records that a route-bound attempt's
	// provider failure moves the next attempt to the route's next candidate.
	EventExecutionFallbackDecided EventType = "execution_fallback_decided"
	// EventTeamActionProposed records a worker's typed recommendation that the
	// coordinator run one catalog action with specific arguments.
	EventTeamActionProposed        EventType = "team_action_proposed"
	EventCostPriceSnapshotResolved EventType = EventType(cost.EventPriceSnapshotResolved)
	EventCostReservationCommitted  EventType = EventType(cost.EventReservationCommitted)
	EventCostSettled               EventType = EventType(cost.EventSettled)
	EventCostBudgetWarning         EventType = EventType(cost.EventBudgetWarning)
	EventCostBudgetDenied          EventType = EventType(cost.EventBudgetDenied)
)

func (e EventType) String() string { return string(e) }

// IsKnownEventType reports whether an event is part of the current runtime
// catalog. Unknown events remain persistable for forward compatibility; the
// reducers intentionally ignore what they do not understand.
func IsKnownEventType(eventType string) bool {
	switch EventType(eventType) {
	case EventRunStarted, EventRunFinished, EventWrapUpPhase, EventRunCancellationRequested,
		EventDecisionPrimitiveStarted, EventDecisionPrimitiveSettled, EventControlDecisionObserved, EventRecoveryChangeObserved,
		EventUserMessageAdded, EventAssistantMessageAdded,
		EventTaskCreated, EventTaskPlanned, EventTaskStarted, EventTaskVerifying, EventTaskPaused, EventTaskCompleted,
		EventTaskFailed, EventTaskBlocked, EventTaskSkipped,
		EventTaskProtocolIncomplete, EventTaskCancelled,
		EventTaskRemoved, EventTaskResolution, EventDynamicToolUnavailable, EventContextArtifactPublished, EventStaticToolGrantNarrowed, EventResourceClaimsResolved,
		EventArtifactCreated, EventCriterionReevaluated,
		EventCriterionCheckpoint, EventMemoryRetrieved,
		EventMemoryUsageRecorded, EventMemoryOutcomeRecorded,
		EventPolicyDecision, EventRecoveryDecision, EventWorkflowStateChanged,
		EventCoordinatorCompactionCommitted, EventCoordinatorCompactionCheckpointAttested,
		EventCoordinatorModelContinuationAdmitted, EventContextWindowAdmission,
		EventContextWindowCompactionCommitted, EventContextWindowDownshift,
		EventModelProfileResolved, EventExecutionPolicySnapshot, EventRunInputsResolved, EventDecisionAdmitted, EventProviderSessionBound, EventBackendSessionBound, EventExecutionTargetMigrated,
		EventDecisionRunOpened, EventDecisionRunAttached, EventPrimaryDecisionPrepared, EventPrimaryDecisionAdmitted,
		EventDecisionRoleCallStarted, EventDecisionRoleCallUnconfirmed, EventDecisionRoleCallSettled,
		EventPrimaryDecisionBlocked, EventPrimaryDecisionBound, EventPrimaryDecisionInvalidated,
		EventExecutionCompatibilityMigrated, EventExecutionPolicySnapshotMigrated, EventExecutionCompatibilityObserved,
		EventResourceLocked, EventWorkspaceSnapshotCommitted,
		EventAttemptWorkspacePrepared, EventAttemptWorkspaceApplyStarted, EventAttemptWorkspaceApplyConflicted,
		EventAttemptWorkspaceApplied, EventAttemptWorkspaceDiscarded, EventAttemptWorkspaceOrphanRemoved,
		EventExecutionFallbackDecided, EventTeamActionProposed,
		EventCostPriceSnapshotResolved, EventCostReservationCommitted, EventCostSettled, EventCostBudgetWarning, EventCostBudgetDenied:
		return true
	default:
		return false
	}
}
