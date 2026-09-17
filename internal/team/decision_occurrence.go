package team

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

const (
	primaryDecisionTaskPrefix = "__hufu_pd_"
	primaryDecisionAgentLabel = "runtime:primary-decision"
)

var (
	ErrDecisionOccurrenceWrongOwner       = errors.New("decision occurrence has the wrong execution owner")
	ErrDecisionOccurrenceAdmissionMissing = errors.New("decision occurrence has no matching durable admission")
)

type RuntimeOccurrenceMetaV1 struct {
	SchemaVersion  int    `json:"schema_version"`
	Origin         string `json:"origin"`
	Purpose        string `json:"purpose"`
	ExecutionOwner string `json:"execution_owner"`
	LogicalRunID   string `json:"logical_run_id"`
	BranchID       string `json:"branch_id"`
	Generation     uint32 `json:"generation"`
	DecisionID     string `json:"decision_id"`
}

type RuntimeOccurrenceV1 struct {
	SchemaVersion      int                     `json:"schema_version"`
	Kind               string                  `json:"kind"`
	TaskID             string                  `json:"task_id"`
	TaskKind           TaskKind                `json:"task_kind"`
	OccurrenceRevision uint32                  `json:"occurrence_revision"`
	Status             TaskStatus              `json:"status"`
	Runtime            RuntimeOccurrenceMetaV1 `json:"runtime"`
}

// PrimaryOccurrenceAdmission binds a projected runtime occurrence to the one
// admitted correctness event that created it.
type PrimaryOccurrenceAdmission struct {
	SchemaVersion     int                 `json:"schema_version"`
	EventID           string              `json:"event_id"`
	EventHash         string              `json:"event_hash"`
	LogicalRunID      string              `json:"logical_run_id"`
	BranchID          string              `json:"branch_id"`
	Generation        uint32              `json:"generation"`
	TaskID            string              `json:"task_id"`
	DecisionID        string              `json:"decision_id"`
	OccurrenceRef     DecisionArtifactRef `json:"occurrence_ref"`
	AdmissionRef      DecisionArtifactRef `json:"admission_ref"`
	PreparationDigest string              `json:"preparation_digest"`
}

// PrimaryManifestProofV1 is the typed process-verification receipt required
// by a completed decision manifest.
type PrimaryManifestProofV1 struct {
	SchemaVersion         int                 `json:"schema_version"`
	Kind                  string              `json:"kind"`
	State                 string              `json:"state"`
	LogicalRunID          string              `json:"logical_run_id"`
	BranchID              string              `json:"branch_id"`
	RequirementDigest     string              `json:"requirement_digest"`
	PrimaryTaskID         string              `json:"primary_task_id"`
	Generation            uint32              `json:"generation"`
	DecisionID            string              `json:"decision_id"`
	AdmissionRef          DecisionArtifactRef `json:"admission_ref"`
	RecordRef             DecisionArtifactRef `json:"record_ref"`
	BaseEvidenceRef       DecisionArtifactRef `json:"base_evidence_ref"`
	SealedEvidenceHash    string              `json:"sealed_evidence_hash"`
	RolePlanRef           DecisionArtifactRef `json:"role_plan_ref"`
	BindingEventID        string              `json:"binding_event_id"`
	SupportRevisionDigest string              `json:"support_revision_digest"`
	ValidationVersion     string              `json:"validation_version"`
}

func ProjectPrimaryDecisionOccurrence(event RunEvent) (*TodoItem, error) {
	var payload PrimaryDecisionAdmittedPayload
	if err := json.Unmarshal(event.Payload, &payload); err != nil {
		return nil, err
	}
	occurrence := RuntimeOccurrenceV1{
		SchemaVersion: 1, Kind: "runtime_task_occurrence", TaskID: payload.TaskID, TaskKind: TaskKindOutcome,
		OccurrenceRevision: payload.Generation, Status: TaskPending,
		Runtime: RuntimeOccurrenceMetaV1{
			SchemaVersion: 1, Origin: "runtime", Purpose: "primary_decision", ExecutionOwner: "decision_engine",
			LogicalRunID: payload.LogicalRunID, BranchID: payload.BranchID, Generation: payload.Generation, DecisionID: payload.DecisionID,
		},
	}
	return NewPrimaryDecisionOccurrence(event, occurrence)
}

// NewPrimaryDecisionOccurrence is the only constructor that accepts the
// strict occurrence artifact and its matching durable admission event.
func NewPrimaryDecisionOccurrence(event RunEvent, occurrence RuntimeOccurrenceV1) (*TodoItem, error) {
	if EventType(event.Type) != EventPrimaryDecisionAdmitted {
		return nil, fmt.Errorf("project primary occurrence: expected %s, got %s", EventPrimaryDecisionAdmitted, event.Type)
	}
	if err := ValidateEventPayload(event); err != nil {
		return nil, fmt.Errorf("project primary occurrence: %w", err)
	}
	if event.ID == "" || !validDecisionDigest(event.Hash) {
		return nil, fmt.Errorf("project primary occurrence: admitted event is not verified")
	}
	var payload PrimaryDecisionAdmittedPayload
	if err := json.Unmarshal(event.Payload, &payload); err != nil {
		return nil, err
	}
	if err := validateDerivedPrimaryIDs(payload.BranchID, payload.LogicalRunID, payload.Generation, payload.TaskID, payload.DecisionID); err != nil {
		return nil, err
	}
	if err := ValidateRuntimeOccurrence(occurrence); err != nil {
		return nil, err
	}
	meta := occurrence.Runtime
	if occurrence.TaskID != payload.TaskID || meta.LogicalRunID != payload.LogicalRunID || meta.BranchID != payload.BranchID || meta.Generation != payload.Generation || meta.DecisionID != payload.DecisionID {
		return nil, fmt.Errorf("project primary occurrence: artifact does not match admission")
	}
	admission := PrimaryOccurrenceAdmission{
		SchemaVersion: 1, EventID: event.ID, EventHash: event.Hash, LogicalRunID: payload.LogicalRunID,
		BranchID: payload.BranchID, Generation: payload.Generation, TaskID: payload.TaskID, DecisionID: payload.DecisionID,
		OccurrenceRef: payload.OccurrenceRef, AdmissionRef: payload.AdmissionRef, PreparationDigest: payload.PreparationDigest,
	}
	item := &TodoItem{
		ID: payload.TaskID, Agent: primaryDecisionAgentLabel, Desc: "Primary decision", Goal: "Produce the primary decision record",
		Status: occurrence.Status, OccurrenceRevision: int(occurrence.OccurrenceRevision), Source: TaskSourceCoordinator, Kind: TaskKindOutcome,
		RuntimeOccurrence: &meta, PrimaryAdmission: &admission,
	}
	if !IsPrimaryOccurrence(item) {
		return nil, fmt.Errorf("project primary occurrence: projection failed validation")
	}
	return item, nil
}

// ValidateRuntimeOccurrence validates the strict RuntimeOccurrenceV1 wire
// contract without consulting mutable runtime state.
func ValidateRuntimeOccurrence(occurrence RuntimeOccurrenceV1) error {
	meta := occurrence.Runtime
	if occurrence.SchemaVersion != 1 || occurrence.Kind != "runtime_task_occurrence" || occurrence.TaskKind != TaskKindOutcome || occurrence.OccurrenceRevision == 0 ||
		meta.SchemaVersion != 1 || meta.Origin != "runtime" || meta.Purpose != "primary_decision" || meta.ExecutionOwner != "decision_engine" ||
		!decisionLogicalIDPattern.MatchString(meta.LogicalRunID) || !validDecisionIdentifier(meta.BranchID) || meta.Generation == 0 ||
		!isRuntimeOccurrenceStatus(occurrence.Status) {
		return fmt.Errorf("invalid primary runtime occurrence")
	}
	return validateDerivedPrimaryIDs(meta.BranchID, meta.LogicalRunID, meta.Generation, occurrence.TaskID, meta.DecisionID)
}

func isRuntimeOccurrenceStatus(status TaskStatus) bool {
	switch status {
	case TaskPending, TaskPlanned, TaskInProgress, TaskVerifying, TaskPaused, TaskDone, TaskError, TaskBlocked, TaskSkipped, TaskProtocolIncomplete:
		return true
	default:
		return false
	}
}

func IsPrimaryOccurrence(item *TodoItem) bool {
	if item == nil || item.RuntimeOccurrence == nil || item.PrimaryAdmission == nil {
		return false
	}
	meta, admission := item.RuntimeOccurrence, item.PrimaryAdmission
	if meta.SchemaVersion != 1 || meta.Origin != "runtime" || meta.Purpose != "primary_decision" || meta.ExecutionOwner != "decision_engine" ||
		admission.SchemaVersion != 1 || !validDecisionIdentifier(admission.EventID) || !validDecisionDigest(admission.EventHash) ||
		!decisionLogicalIDPattern.MatchString(meta.LogicalRunID) || !validDecisionIdentifier(meta.BranchID) || meta.Generation == 0 ||
		meta.LogicalRunID != admission.LogicalRunID || meta.BranchID != admission.BranchID || meta.Generation != admission.Generation ||
		meta.DecisionID != admission.DecisionID || item.ID != admission.TaskID || item.Kind != TaskKindOutcome || item.OccurrenceRevision <= 0 ||
		validateDecisionArtifactRef(admission.OccurrenceRef) != nil || validateDecisionArtifactRef(admission.AdmissionRef) != nil || !validDecisionDigest(admission.PreparationDigest) {
		return false
	}
	return validateDerivedPrimaryIDs(meta.BranchID, meta.LogicalRunID, meta.Generation, item.ID, meta.DecisionID) == nil
}

func IsSchedulableWorker(item *TodoItem) bool {
	return item != nil && !IsPrimaryOccurrence(item) && !strings.HasPrefix(item.ID, primaryDecisionTaskPrefix)
}

func IsBlockingSupportingWork(item *TodoItem) bool {
	return IsSchedulableWorker(item) && isUnresolvedTaskStatus(item.Status)
}

func validatePrimaryOccurrenceForExecution(item *TodoItem) error {
	if item == nil || !strings.HasPrefix(item.ID, primaryDecisionTaskPrefix) {
		return nil
	}
	if !IsPrimaryOccurrence(item) {
		return fmt.Errorf("%w: task %q", ErrDecisionOccurrenceAdmissionMissing, item.ID)
	}
	return nil
}

func validatePrimaryManifestProof(proof *PrimaryManifestProofV1, occurrence *TodoItem) error {
	if proof == nil || proof.SchemaVersion != 1 || proof.Kind != "primary_decision_valid" || proof.State != "satisfied" || proof.ValidationVersion != "primary-decision-process@v1" {
		return fmt.Errorf("primary manifest proof has invalid version, kind, state, or validator")
	}
	if !decisionLogicalIDPattern.MatchString(proof.LogicalRunID) || !validDecisionIdentifier(proof.BranchID) || !validDecisionDigest(proof.RequirementDigest) ||
		proof.Generation == 0 || !validDecisionIdentifier(proof.BindingEventID) || !validDecisionDigest(proof.SealedEvidenceHash) || !validDecisionDigest(proof.SupportRevisionDigest) {
		return fmt.Errorf("primary manifest proof has invalid identity or digest")
	}
	for _, ref := range []DecisionArtifactRef{proof.AdmissionRef, proof.RecordRef, proof.BaseEvidenceRef, proof.RolePlanRef} {
		if err := validateDecisionArtifactRef(ref); err != nil {
			return err
		}
	}
	if err := validateDerivedPrimaryIDs(proof.BranchID, proof.LogicalRunID, proof.Generation, proof.PrimaryTaskID, proof.DecisionID); err != nil {
		return err
	}
	if occurrence != nil {
		if !IsPrimaryOccurrence(occurrence) || occurrence.ID != proof.PrimaryTaskID || occurrence.RuntimeOccurrence.LogicalRunID != proof.LogicalRunID ||
			occurrence.RuntimeOccurrence.BranchID != proof.BranchID || occurrence.RuntimeOccurrence.Generation != proof.Generation ||
			occurrence.RuntimeOccurrence.DecisionID != proof.DecisionID || occurrence.PrimaryAdmission.AdmissionRef != proof.AdmissionRef {
			return fmt.Errorf("primary manifest proof does not match runtime occurrence")
		}
	}
	return nil
}

func cloneRuntimeOccurrenceMeta(value *RuntimeOccurrenceMetaV1) *RuntimeOccurrenceMetaV1 {
	if value == nil {
		return nil
	}
	return new(*value)
}

func clonePrimaryOccurrenceAdmission(value *PrimaryOccurrenceAdmission) *PrimaryOccurrenceAdmission {
	if value == nil {
		return nil
	}
	return new(*value)
}

func clonePrimaryManifestProof(value *PrimaryManifestProofV1) *PrimaryManifestProofV1 {
	if value == nil {
		return nil
	}
	return new(*value)
}
