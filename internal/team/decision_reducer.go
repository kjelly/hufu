package team

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"strings"
)

type decisionPreparedState struct {
	Payload PrimaryDecisionPreparedPayload
}

type decisionAdmittedState struct {
	Payload PrimaryDecisionAdmittedPayload
}

type decisionCallState struct {
	Started     DecisionRoleCallStartedPayload
	Unconfirmed bool
	Settlement  *DecisionRoleCallSettledPayload
}

// LogicalDecisionReducer is a pure, detached reducer. It does not resolve
// artifacts, inspect mutable configuration, allocate IDs, or call providers.
type LogicalDecisionReducer struct {
	run          *LogicalDecisionRun
	seenEvents   map[string]string
	businessKeys map[string][]byte
	prepared     *decisionPreparedState
	admitted     *decisionAdmittedState
	calls        map[string]*decisionCallState
	stablePhase  LogicalDecisionPhase
	boundEventID string
}

func NewLogicalDecisionReducer() *LogicalDecisionReducer {
	return &LogicalDecisionReducer{
		seenEvents:   make(map[string]string),
		businessKeys: make(map[string][]byte),
		calls:        make(map[string]*decisionCallState),
	}
}

// ReplayLogicalDecisionRun rebuilds one logical run solely from verified
// journal events. Unrelated events are ignored.
func ReplayLogicalDecisionRun(events []RunEvent, logicalRunID, branchID string) (*LogicalDecisionRun, error) {
	reducer := NewLogicalDecisionReducer()
	for _, event := range events {
		if event.BranchID != branchID {
			continue
		}
		if !isDecisionCorrectnessEvent(event.Type) {
			continue
		}
		var common struct {
			LogicalRunID string `json:"logical_run_id"`
		}
		if err := json.Unmarshal(event.Payload, &common); err != nil {
			return nil, fmt.Errorf("inspect decision event %q: %w", event.ID, err)
		}
		if common.LogicalRunID != logicalRunID {
			continue
		}
		if err := reducer.Apply(event); err != nil {
			return reducer.Snapshot(), err
		}
	}
	if reducer.run == nil {
		return nil, fmt.Errorf("logical decision run %q not found on branch %q", logicalRunID, branchID)
	}
	return reducer.Snapshot(), nil
}

// Apply validates and atomically applies one correctness event.
func (r *LogicalDecisionReducer) Apply(event RunEvent) error {
	if !isDecisionCorrectnessEvent(event.Type) {
		if strings.HasPrefix(event.Type, "decision_") || strings.HasPrefix(event.Type, "primary_decision_") {
			if r.run != nil {
				r.run.Phase = LogicalDecisionRecoveryRequired
			}
			return fmt.Errorf("unknown decision correctness event %q", event.Type)
		}
		return nil
	}
	if err := ValidateEventPayload(event); err != nil {
		return err
	}
	if event.ID == "" || event.Hash == "" {
		return fmt.Errorf("decision reducer requires verified event identity and hash")
	}
	if hash, seen := r.seenEvents[event.ID]; seen {
		if hash != event.Hash {
			return fmt.Errorf("decision event %q replayed with a different hash", event.ID)
		}
		return nil
	}
	businessPayload, err := decisionBusinessPayload(event.Payload)
	if err != nil {
		return fmt.Errorf("normalize decision event business payload: %w", err)
	}
	if event.IdempotencyKey == "" {
		return fmt.Errorf("decision correctness event %q has no idempotency key", event.Type)
	}
	if existing, ok := r.businessKeys[event.IdempotencyKey]; ok {
		if !bytes.Equal(existing, businessPayload) {
			return fmt.Errorf("%w: key %q", ErrDecisionIdempotencyConflict, event.IdempotencyKey)
		}
		return nil
	}

	clone := r.clone()
	if err := clone.applyNew(event); err != nil {
		return err
	}
	clone.seenEvents[event.ID] = event.Hash
	clone.businessKeys[event.IdempotencyKey] = businessPayload
	clone.run.LastAppliedEventID = event.ID
	clone.run.LastAppliedHash = event.Hash
	*r = *clone
	return nil
}

func (r *LogicalDecisionReducer) applyNew(event RunEvent) error {
	switch EventType(event.Type) {
	case EventDecisionRunOpened:
		var payload DecisionRunOpenedPayload
		_ = json.Unmarshal(event.Payload, &payload)
		return r.applyOpened(event, payload)
	case EventDecisionRunAttached:
		var payload DecisionRunAttachedPayload
		_ = json.Unmarshal(event.Payload, &payload)
		return r.applyAttached(event, payload)
	case EventPrimaryDecisionPrepared:
		var payload PrimaryDecisionPreparedPayload
		_ = json.Unmarshal(event.Payload, &payload)
		return r.applyPrepared(event, payload)
	case EventPrimaryDecisionAdmitted:
		var payload PrimaryDecisionAdmittedPayload
		_ = json.Unmarshal(event.Payload, &payload)
		return r.applyAdmitted(event, payload)
	case EventDecisionRoleCallStarted:
		var payload DecisionRoleCallStartedPayload
		_ = json.Unmarshal(event.Payload, &payload)
		return r.applyCallStarted(event, payload)
	case EventDecisionRoleCallUnconfirmed:
		var payload DecisionRoleCallUnconfirmedPayload
		_ = json.Unmarshal(event.Payload, &payload)
		return r.applyCallUnconfirmed(event, payload)
	case EventDecisionRoleCallSettled:
		var payload DecisionRoleCallSettledPayload
		_ = json.Unmarshal(event.Payload, &payload)
		return r.applyCallSettled(event, payload)
	case EventPrimaryDecisionBlocked:
		var payload PrimaryDecisionBlockedPayload
		_ = json.Unmarshal(event.Payload, &payload)
		return r.applyBlocked(event, payload)
	case EventPrimaryDecisionBound:
		var payload PrimaryDecisionBoundPayload
		_ = json.Unmarshal(event.Payload, &payload)
		return r.applyBound(event, payload)
	case EventPrimaryDecisionInvalidated:
		var payload PrimaryDecisionInvalidatedPayload
		_ = json.Unmarshal(event.Payload, &payload)
		return r.applyInvalidated(event, payload)
	default:
		return fmt.Errorf("unsupported decision correctness event %q", event.Type)
	}
}

func (r *LogicalDecisionReducer) applyOpened(event RunEvent, payload DecisionRunOpenedPayload) error {
	if r.run != nil {
		return fmt.Errorf("logical decision run already exists")
	}
	if event.IdempotencyKey != decisionOpenedKey(payload.LogicalRunID) {
		return fmt.Errorf("opened event has invalid idempotency key")
	}
	r.run = &LogicalDecisionRun{
		SchemaVersion: 1, Kind: "logical_decision_run", LogicalRunID: payload.LogicalRunID,
		BranchID: payload.BranchID, TeamID: payload.TeamID, TeamDefinitionDigest: payload.TeamDefinitionDigest,
		RequirementRef: payload.RequirementRef, RequirementDigest: payload.RequirementDigest,
		ProfileBundleRef: payload.ProfileBundleRef, ExecutionRunIDs: []string{payload.ExecutionRunID},
		OwnerEpoch: 1, Phase: LogicalDecisionSupporting,
	}
	r.stablePhase = LogicalDecisionSupporting
	return nil
}

func (r *LogicalDecisionReducer) applyAttached(event RunEvent, payload DecisionRunAttachedPayload) error {
	if r.run == nil {
		return fmt.Errorf("decision event precedes decision_run_opened")
	}
	if payload.LogicalRunID != r.run.LogicalRunID || payload.BranchID != r.run.BranchID {
		return fmt.Errorf("decision event belongs to a foreign logical run")
	}
	if r.run.Phase == LogicalDecisionClosed || r.run.Phase == LogicalDecisionRecoveryRequired {
		return fmt.Errorf("logical decision run in phase %s cannot attach", r.run.Phase)
	}
	lastExecution := r.run.ExecutionRunIDs[len(r.run.ExecutionRunIDs)-1]
	if payload.PreviousExecutionRunID != lastExecution || payload.OwnerEpoch != r.run.OwnerEpoch+1 || event.IdempotencyKey != decisionAttachedKey(payload.LogicalRunID, payload.ExecutionRunID) {
		return fmt.Errorf("decision attach violates execution lineage or owner epoch")
	}
	if payload.ResumeFromEventID != r.run.LastAppliedEventID {
		return fmt.Errorf("decision attach resume cursor does not match projection")
	}
	r.run.ExecutionRunIDs = append(r.run.ExecutionRunIDs, payload.ExecutionRunID)
	r.run.OwnerEpoch = payload.OwnerEpoch
	if r.run.Phase == LogicalDecisionSuspended {
		r.run.Phase = r.stablePhase
	}
	return nil
}

func (r *LogicalDecisionReducer) applyPrepared(event RunEvent, payload PrimaryDecisionPreparedPayload) error {
	if err := r.requireGenerationOwner(payload.decisionGenerationEventCommon); err != nil {
		return err
	}
	if r.run.Phase != LogicalDecisionSupporting {
		return fmt.Errorf("prepare requires SUPPORTING, got %s", r.run.Phase)
	}
	wantGeneration := r.run.CurrentGeneration
	if wantGeneration == 0 {
		wantGeneration = 1
	}
	if payload.Generation != wantGeneration || event.IdempotencyKey != decisionPreparedKey(payload.LogicalRunID, payload.Generation, payload.PreparationDigest) {
		return fmt.Errorf("prepare has invalid generation or idempotency key")
	}
	if err := validateDerivedPrimaryIDs(payload.BranchID, payload.LogicalRunID, payload.Generation, payload.TaskID, payload.DecisionID); err != nil {
		return err
	}
	r.run.CurrentGeneration = payload.Generation
	r.run.Phase = LogicalDecisionPrepared
	r.stablePhase = LogicalDecisionPrepared
	r.run.Usage.GenerationsStarted++
	r.prepared = &decisionPreparedState{Payload: payload}
	r.admitted = nil
	r.calls = make(map[string]*decisionCallState)
	return nil
}

func (r *LogicalDecisionReducer) applyAdmitted(event RunEvent, payload PrimaryDecisionAdmittedPayload) error {
	if err := r.requireGenerationOwner(payload.decisionGenerationEventCommon); err != nil {
		return err
	}
	if r.run.Phase != LogicalDecisionPrepared || r.prepared == nil {
		return fmt.Errorf("admission requires PREPARED")
	}
	prepared := r.prepared.Payload
	if payload.TaskID != prepared.TaskID || payload.DecisionID != prepared.DecisionID || payload.PreparationDigest != prepared.PreparationDigest || event.IdempotencyKey != decisionAdmittedKey(payload.LogicalRunID, payload.Generation) {
		return fmt.Errorf("admission does not match prepared generation")
	}
	r.run.Phase = LogicalDecisionAdmitted
	r.stablePhase = LogicalDecisionAdmitted
	r.admitted = &decisionAdmittedState{Payload: payload}
	return nil
}

func (r *LogicalDecisionReducer) applyCallStarted(event RunEvent, payload DecisionRoleCallStartedPayload) error {
	if err := r.requireGenerationOwner(payload.decisionGenerationEventCommon); err != nil {
		return err
	}
	if r.run.Phase != LogicalDecisionAdmitted || r.admitted == nil || payload.DecisionID != r.admitted.Payload.DecisionID {
		return fmt.Errorf("role call requires matching ADMITTED generation")
	}
	wantKey := decisionCallKey(payload.LogicalRunID, payload.Generation, payload.Role, payload.Ordinal, payload.Round, payload.InvocationAttempt, "started")
	if event.IdempotencyKey != wantKey {
		return fmt.Errorf("role call started has invalid idempotency key")
	}
	if _, exists := r.calls[payload.InvocationID]; exists {
		return fmt.Errorf("role call invocation %q already exists", payload.InvocationID)
	}
	r.calls[payload.InvocationID] = &decisionCallState{Started: payload}
	return nil
}

func (r *LogicalDecisionReducer) applyCallUnconfirmed(event RunEvent, payload DecisionRoleCallUnconfirmedPayload) error {
	if err := r.requireGenerationOwner(payload.decisionGenerationEventCommon); err != nil {
		return err
	}
	call := r.calls[payload.InvocationID]
	if r.run.Phase != LogicalDecisionAdmitted || call == nil || call.Settlement != nil || call.Started.ReservationRef != payload.ReservationRef {
		return fmt.Errorf("unconfirmed role call has no matching unsettled start")
	}
	wantKey := decisionCallKey(payload.LogicalRunID, payload.Generation, call.Started.Role, call.Started.Ordinal, call.Started.Round, call.Started.InvocationAttempt, "unconfirmed")
	if event.IdempotencyKey != wantKey {
		return fmt.Errorf("unconfirmed role call has invalid idempotency key")
	}
	call.Unconfirmed = true
	return nil
}

func (r *LogicalDecisionReducer) applyCallSettled(event RunEvent, payload DecisionRoleCallSettledPayload) error {
	if err := r.requireGenerationOwner(payload.decisionGenerationEventCommon); err != nil {
		return err
	}
	call := r.calls[payload.InvocationID]
	if r.run.Phase != LogicalDecisionAdmitted || call == nil || call.Settlement != nil {
		return fmt.Errorf("settled role call has no unique matching start")
	}
	wantKey := decisionCallKey(payload.LogicalRunID, payload.Generation, call.Started.Role, call.Started.Ordinal, call.Started.Round, call.Started.InvocationAttempt, "settled")
	if event.IdempotencyKey != wantKey {
		return fmt.Errorf("settled role call has invalid idempotency key")
	}
	if math.MaxUint64-r.run.Usage.TokensUsed < payload.TokensUsed {
		return fmt.Errorf("decision token usage overflow")
	}
	settlement := payload
	call.Settlement = &settlement
	r.run.Usage.TokensUsed += payload.TokensUsed
	return nil
}

func (r *LogicalDecisionReducer) applyBlocked(event RunEvent, payload PrimaryDecisionBlockedPayload) error {
	if err := r.requireGenerationOwner(payload.decisionGenerationEventCommon); err != nil {
		return err
	}
	if r.run.Phase != LogicalDecisionSupporting && r.run.Phase != LogicalDecisionPrepared && r.run.Phase != LogicalDecisionAdmitted {
		return fmt.Errorf("blocked event is illegal from %s", r.run.Phase)
	}
	wantKey := decisionBlockedKey(payload.LogicalRunID, payload.Generation, payload.SupportRevisionDigest, payload.ReasonCode)
	if event.IdempotencyKey != wantKey {
		return fmt.Errorf("blocked event has invalid idempotency key")
	}
	if r.admitted != nil && r.admitted.Payload.Generation == payload.Generation {
		r.run.Phase = LogicalDecisionAdmitted
		r.stablePhase = LogicalDecisionAdmitted
	} else {
		r.run.Phase = LogicalDecisionSupporting
		r.stablePhase = LogicalDecisionSupporting
	}
	return nil
}

func (r *LogicalDecisionReducer) applyBound(event RunEvent, payload PrimaryDecisionBoundPayload) error {
	if err := r.requireGenerationOwner(payload.decisionGenerationEventCommon); err != nil {
		return err
	}
	if r.run.Phase != LogicalDecisionAdmitted || r.admitted == nil || event.IdempotencyKey != decisionBoundKey(payload.LogicalRunID, payload.Generation) {
		return fmt.Errorf("binding requires matching ADMITTED generation")
	}
	binding := payload.Binding
	prepared, admitted := r.prepared.Payload, r.admitted.Payload
	if binding.LogicalRunID != r.run.LogicalRunID || binding.BranchID != r.run.BranchID || binding.Generation != r.run.CurrentGeneration || binding.TaskID != admitted.TaskID || binding.DecisionID != admitted.DecisionID || binding.RequirementDigest != r.run.RequirementDigest || binding.SupportRevisionDigest != prepared.SupportRevisionDigest || binding.AdmissionRef != admitted.AdmissionRef || binding.BaseEvidenceRef != prepared.BaseEvidenceRef || binding.RolePlanRef != prepared.RolePlanRef {
		return fmt.Errorf("primary binding does not match prepared admission")
	}
	bindingCopy := binding
	r.run.ActivePrimary = &bindingCopy
	r.run.Phase = LogicalDecisionBound
	r.stablePhase = LogicalDecisionBound
	r.boundEventID = event.ID
	return nil
}

func (r *LogicalDecisionReducer) applyInvalidated(event RunEvent, payload PrimaryDecisionInvalidatedPayload) error {
	if err := r.requireGenerationOwner(payload.decisionGenerationEventCommon); err != nil {
		return err
	}
	if r.run.Phase != LogicalDecisionPrepared && r.run.Phase != LogicalDecisionAdmitted && r.run.Phase != LogicalDecisionBound {
		return fmt.Errorf("invalidation is illegal from %s", r.run.Phase)
	}
	if event.IdempotencyKey != decisionInvalidatedKey(payload.LogicalRunID, payload.Generation) || payload.NextGeneration != r.run.CurrentGeneration+1 {
		return fmt.Errorf("invalidation has invalid key or generation")
	}
	if r.prepared == nil || payload.TaskID != r.prepared.Payload.TaskID || payload.DecisionID != r.prepared.Payload.DecisionID {
		return fmt.Errorf("invalidation does not identify the current primary generation")
	}
	if r.run.ActivePrimary != nil {
		if payload.PriorBindingEventID == nil || *payload.PriorBindingEventID != r.boundEventID {
			return fmt.Errorf("invalidation does not identify active binding")
		}
	} else if payload.PriorBindingEventID != nil {
		return fmt.Errorf("invalidation names a binding when none is active")
	}
	r.run.ActivePrimary = nil
	r.run.CurrentGeneration = payload.NextGeneration
	r.run.Phase = LogicalDecisionSupporting
	r.stablePhase = LogicalDecisionSupporting
	r.prepared = nil
	r.admitted = nil
	r.calls = make(map[string]*decisionCallState)
	r.boundEventID = ""
	return nil
}

func (r *LogicalDecisionReducer) requireRunAndBranch(common decisionEventCommon) error {
	if r.run == nil {
		return fmt.Errorf("decision event precedes decision_run_opened")
	}
	if common.LogicalRunID != r.run.LogicalRunID || common.BranchID != r.run.BranchID {
		return fmt.Errorf("decision event belongs to a foreign logical run")
	}
	if common.OwnerEpoch != r.run.OwnerEpoch || common.ExecutionRunID != r.run.ExecutionRunIDs[len(r.run.ExecutionRunIDs)-1] {
		return fmt.Errorf("decision event rejected by owner fencing")
	}
	return nil
}

func (r *LogicalDecisionReducer) requireGenerationOwner(common decisionGenerationEventCommon) error {
	if err := r.requireRunAndBranch(common.decisionEventCommon); err != nil {
		return err
	}
	if common.Generation != r.run.CurrentGeneration && (r.run.CurrentGeneration != 0 || common.Generation != 1) {
		return fmt.Errorf("decision event generation %d does not match current generation %d", common.Generation, r.run.CurrentGeneration)
	}
	return nil
}

func (r *LogicalDecisionReducer) Snapshot() *LogicalDecisionRun {
	if r == nil || r.run == nil {
		return nil
	}
	copy := *r.run
	copy.ExecutionRunIDs = append([]string(nil), r.run.ExecutionRunIDs...)
	if r.run.ActivePrimary != nil {
		binding := *r.run.ActivePrimary
		copy.ActivePrimary = &binding
	}
	if r.run.Usage.InvocationSettlementsRef != nil {
		ref := *r.run.Usage.InvocationSettlementsRef
		copy.Usage.InvocationSettlementsRef = &ref
	}
	return &copy
}

func (r *LogicalDecisionReducer) clone() *LogicalDecisionReducer {
	clone := NewLogicalDecisionReducer()
	clone.run = r.Snapshot()
	clone.stablePhase = r.stablePhase
	clone.boundEventID = r.boundEventID
	for id, hash := range r.seenEvents {
		clone.seenEvents[id] = hash
	}
	for key, payload := range r.businessKeys {
		clone.businessKeys[key] = append([]byte(nil), payload...)
	}
	if r.prepared != nil {
		prepared := *r.prepared
		clone.prepared = &prepared
	}
	if r.admitted != nil {
		admitted := *r.admitted
		clone.admitted = &admitted
	}
	for id, call := range r.calls {
		callCopy := *call
		if call.Settlement != nil {
			settlement := *call.Settlement
			callCopy.Settlement = &settlement
		}
		clone.calls[id] = &callCopy
	}
	return clone
}

func validateDerivedPrimaryIDs(branchID, logicalRunID string, generation uint32, taskID, decisionID string) error {
	wantTask, err := PrimaryDecisionTaskID(branchID, logicalRunID)
	if err != nil {
		return err
	}
	wantDecision, err := PrimaryDecisionID(branchID, logicalRunID, generation)
	if err != nil {
		return err
	}
	if taskID != wantTask || decisionID != wantDecision {
		return fmt.Errorf("primary task or decision identity does not match logical run")
	}
	return nil
}

func decisionOpenedKey(logicalRunID string) string {
	return "udr:v1:" + logicalRunID + ":opened"
}

func decisionAttachedKey(logicalRunID, executionRunID string) string {
	return "udr:v1:" + logicalRunID + ":attach:" + executionRunID
}

func decisionPreparedKey(logicalRunID string, generation uint32, digest string) string {
	return fmt.Sprintf("udr:v1:%s:g:%d:prepared:%s", logicalRunID, generation, digest)
}

func decisionAdmittedKey(logicalRunID string, generation uint32) string {
	return fmt.Sprintf("udr:v1:%s:g:%d:admitted", logicalRunID, generation)
}

func decisionCallKey(logicalRunID string, generation uint32, role string, ordinal, round, attempt uint32, suffix string) string {
	return fmt.Sprintf("udr:v1:%s:g:%d:%s:%d:r:%d:a:%d:%s", logicalRunID, generation, role, ordinal, round, attempt, suffix)
}

func decisionBlockedKey(logicalRunID string, generation uint32, supportDigest, reason string) string {
	return fmt.Sprintf("udr:v1:%s:g:%d:blocked:%s:%s", logicalRunID, generation, supportDigest, reason)
}

func decisionBoundKey(logicalRunID string, generation uint32) string {
	return fmt.Sprintf("udr:v1:%s:g:%d:bound", logicalRunID, generation)
}

func decisionInvalidatedKey(logicalRunID string, generation uint32) string {
	return fmt.Sprintf("udr:v1:%s:g:%d:invalidated", logicalRunID, generation)
}
