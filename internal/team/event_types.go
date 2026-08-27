package team

// EventType is the stable catalog identifier for a runtime event. RunEvent
// deliberately keeps Type as a string so existing JSONL workspaces and API
// callers remain source and wire compatible while producers migrate.
type EventType string

const (
	EventRunStarted                              EventType = "run_started"
	EventRunFinished                             EventType = "run_finished"
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
)

func (e EventType) String() string { return string(e) }

// IsKnownEventType reports whether an event is part of the current runtime
// catalog. Unknown events remain persistable for forward compatibility; the
// reducers intentionally ignore what they do not understand.
func IsKnownEventType(eventType string) bool {
	switch EventType(eventType) {
	case EventRunStarted, EventRunFinished,
		EventUserMessageAdded, EventAssistantMessageAdded,
		EventTaskCreated, EventTaskPlanned, EventTaskStarted, EventTaskVerifying, EventTaskPaused, EventTaskCompleted,
		EventTaskFailed, EventTaskBlocked, EventTaskSkipped,
		EventTaskProtocolIncomplete, EventTaskCancelled,
		EventTaskRemoved, EventTaskResolution,
		EventArtifactCreated, EventCriterionReevaluated,
		EventCriterionCheckpoint, EventMemoryRetrieved,
		EventMemoryUsageRecorded, EventMemoryOutcomeRecorded,
		EventPolicyDecision, EventRecoveryDecision, EventWorkflowStateChanged,
		EventCoordinatorCompactionCommitted, EventCoordinatorCompactionCheckpointAttested,
		EventCoordinatorModelContinuationAdmitted:
		return true
	default:
		return false
	}
}
