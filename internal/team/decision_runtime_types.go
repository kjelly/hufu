package team

// DecisionArtifactRef is the immutable, content-addressed reference used by
// the universal decision runtime. It intentionally excludes workspace paths.
type DecisionArtifactRef struct {
	ID        string `json:"id"`
	SHA256    string `json:"sha256"`
	MediaType string `json:"media_type"`
	SizeBytes uint64 `json:"size_bytes"`
}

// DecisionEffectiveLimits is frozen when the logical run is opened.
type DecisionEffectiveLimits struct {
	TotalDecisionTokens        uint64 `json:"total_decision_tokens"`
	ActiveDecisionDurationMS   uint64 `json:"active_decision_duration_ms"`
	MaxGenerations             uint32 `json:"max_generations"`
	MaxStageInvocationAttempts uint32 `json:"max_stage_invocation_attempts"`
	CleanupTimeoutMS           uint64 `json:"cleanup_timeout_ms"`
}

// LogicalDecisionPhase is the durable phase reduced from correctness events.
type LogicalDecisionPhase string

const (
	LogicalDecisionSupporting       LogicalDecisionPhase = "SUPPORTING"
	LogicalDecisionPrepared         LogicalDecisionPhase = "PREPARED"
	LogicalDecisionAdmitted         LogicalDecisionPhase = "ADMITTED"
	LogicalDecisionBound            LogicalDecisionPhase = "BOUND"
	LogicalDecisionSuspended        LogicalDecisionPhase = "SUSPENDED"
	LogicalDecisionClosed           LogicalDecisionPhase = "CLOSED"
	LogicalDecisionRecoveryRequired LogicalDecisionPhase = "RECOVERY_REQUIRED"
)

// LogicalDecisionUsage is a projection of durable reservations and receipts,
// never a second accounting authority.
type LogicalDecisionUsage struct {
	TokensUsed               uint64               `json:"tokens_used"`
	ReservedTokens           uint64               `json:"reserved_tokens"`
	ActiveDurationMS         uint64               `json:"active_duration_ms"`
	GenerationsStarted       uint32               `json:"generations_started"`
	InvocationSettlementsRef *DecisionArtifactRef `json:"invocation_settlements_ref"`
}

// PrimaryBindingV1 identifies the only active primary decision for a logical
// run. Historical bindings remain in the event stream, not in this slot.
type PrimaryBindingV1 struct {
	SchemaVersion         int                 `json:"schema_version"`
	LogicalRunID          string              `json:"logical_run_id"`
	BranchID              string              `json:"branch_id"`
	TaskID                string              `json:"task_id"`
	Generation            uint32              `json:"generation"`
	DecisionID            string              `json:"decision_id"`
	RequirementDigest     string              `json:"requirement_digest"`
	SupportRevisionDigest string              `json:"support_revision_digest"`
	AdmissionRef          DecisionArtifactRef `json:"admission_ref"`
	BaseEvidenceRef       DecisionArtifactRef `json:"base_evidence_ref"`
	RolePlanRef           DecisionArtifactRef `json:"role_plan_ref"`
	RecordRef             DecisionArtifactRef `json:"record_ref"`
	SealedEvidenceHash    string              `json:"sealed_evidence_hash"`
}

// LogicalDecisionRun is the fully rebuildable projection for one branch-bound
// logical decision run.
type LogicalDecisionRun struct {
	SchemaVersion        int                  `json:"schema_version"`
	Kind                 string               `json:"kind"`
	LogicalRunID         string               `json:"logical_run_id"`
	BranchID             string               `json:"branch_id"`
	TeamID               string               `json:"team_id"`
	TeamDefinitionDigest string               `json:"team_definition_digest"`
	RequirementRef       DecisionArtifactRef  `json:"requirement_ref"`
	RequirementDigest    string               `json:"requirement_digest"`
	ProfileBundleRef     DecisionArtifactRef  `json:"profile_bundle_ref"`
	ExecutionRunIDs      []string             `json:"execution_run_ids"`
	OwnerEpoch           uint64               `json:"owner_epoch"`
	Phase                LogicalDecisionPhase `json:"phase"`
	CurrentGeneration    uint32               `json:"current_generation"`
	ActivePrimary        *PrimaryBindingV1    `json:"active_primary"`
	LastAppliedEventID   string               `json:"last_applied_event_id"`
	LastAppliedHash      string               `json:"last_applied_hash"`
	Usage                LogicalDecisionUsage `json:"usage"`
}

type decisionEventCommon struct {
	SchemaVersion  int    `json:"schema_version"`
	LogicalRunID   string `json:"logical_run_id"`
	BranchID       string `json:"branch_id"`
	ExecutionRunID string `json:"execution_run_id"`
	OwnerEpoch     uint64 `json:"owner_epoch"`
}

type decisionGenerationEventCommon struct {
	decisionEventCommon
	Generation uint32 `json:"generation"`
}

type DecisionRunOpenedPayload struct {
	decisionEventCommon
	TeamID               string                  `json:"team_id"`
	TeamDefinitionDigest string                  `json:"team_definition_digest"`
	RequirementRef       DecisionArtifactRef     `json:"requirement_ref"`
	RequirementDigest    string                  `json:"requirement_digest"`
	ProfileBundleRef     DecisionArtifactRef     `json:"profile_bundle_ref"`
	AuthoritySnapshotRef DecisionArtifactRef     `json:"authority_snapshot_ref"`
	EffectiveLimits      DecisionEffectiveLimits `json:"effective_limits"`
}

type DecisionRunAttachedPayload struct {
	decisionEventCommon
	PreviousExecutionRunID string `json:"previous_execution_run_id"`
	ResumeFromEventID      string `json:"resume_from_event_id"`
}

type DecisionSupportCursor struct {
	EventID       string `json:"event_id"`
	EventHash     string `json:"event_hash"`
	LineageDigest string `json:"lineage_digest"`
}

type PrimaryDecisionPreparedPayload struct {
	decisionGenerationEventCommon
	TaskID                string                `json:"task_id"`
	DecisionID            string                `json:"decision_id"`
	SupportCursor         DecisionSupportCursor `json:"support_cursor"`
	SupportRevisionDigest string                `json:"support_revision_digest"`
	BaseEvidenceRef       DecisionArtifactRef   `json:"base_evidence_ref"`
	RolePlanRef           DecisionArtifactRef   `json:"role_plan_ref"`
	PreparationDigest     string                `json:"preparation_digest"`
}

type PrimaryDecisionAdmittedPayload struct {
	decisionGenerationEventCommon
	TaskID            string              `json:"task_id"`
	DecisionID        string              `json:"decision_id"`
	OccurrenceRef     DecisionArtifactRef `json:"occurrence_ref"`
	AdmissionRef      DecisionArtifactRef `json:"admission_ref"`
	PreparationDigest string              `json:"preparation_digest"`
}

type DecisionRoleCallStartedPayload struct {
	decisionGenerationEventCommon
	DecisionID        string              `json:"decision_id"`
	Role              string              `json:"role"`
	Ordinal           uint32              `json:"ordinal"`
	Round             uint32              `json:"round"`
	InvocationAttempt uint32              `json:"invocation_attempt"`
	BindingID         string              `json:"binding_id"`
	InvocationID      string              `json:"invocation_id"`
	InputDigest       string              `json:"input_digest"`
	ReservationRef    DecisionArtifactRef `json:"reservation_ref"`
}

type DecisionRoleCallUnconfirmedPayload struct {
	decisionGenerationEventCommon
	InvocationID   string              `json:"invocation_id"`
	ReasonCode     string              `json:"reason_code"`
	ReservationRef DecisionArtifactRef `json:"reservation_ref"`
}

type DecisionRoleCallSettledPayload struct {
	decisionGenerationEventCommon
	InvocationID string               `json:"invocation_id"`
	Status       string               `json:"status"`
	ReceiptRef   DecisionArtifactRef  `json:"receipt_ref"`
	OutputRef    *DecisionArtifactRef `json:"output_ref"`
	UsageState   string               `json:"usage_state"`
	TokensUsed   uint64               `json:"tokens_used"`
	ReasonCode   *string              `json:"reason_code"`
}

type DecisionMissingRequirement struct {
	Code          string  `json:"code"`
	CriterionID   *string `json:"criterion_id"`
	RequiredCount uint32  `json:"required_count"`
	ObservedCount uint32  `json:"observed_count"`
}

type PrimaryDecisionBlockedPayload struct {
	decisionGenerationEventCommon
	TaskID                *string                      `json:"task_id"`
	ReasonCode            string                       `json:"reason_code"`
	SupportRevisionDigest string                       `json:"support_revision_digest"`
	Repairable            bool                         `json:"repairable"`
	MissingRequirements   []DecisionMissingRequirement `json:"missing_requirements"`
}

type PrimaryDecisionBoundPayload struct {
	decisionGenerationEventCommon
	Binding                    PrimaryBindingV1    `json:"binding"`
	RecordValidationReceiptRef DecisionArtifactRef `json:"record_validation_receipt_ref"`
}

type PrimaryDecisionInvalidatedPayload struct {
	decisionGenerationEventCommon
	TaskID              string   `json:"task_id"`
	DecisionID          string   `json:"decision_id"`
	PriorBindingEventID *string  `json:"prior_binding_event_id"`
	TriggerEventIDs     []string `json:"trigger_event_ids"`
	ReasonCode          string   `json:"reason_code"`
	NextGeneration      uint32   `json:"next_generation"`
}
