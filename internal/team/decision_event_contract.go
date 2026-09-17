package team

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
)

var (
	decisionIdentifierPattern = regexp.MustCompile(`^[A-Za-z0-9_.:@/-]+$`)
	decisionLogicalIDPattern  = regexp.MustCompile(`^ldr_[0-9a-f]{32}$`)
	decisionDigestPattern     = regexp.MustCompile(`^[0-9a-f]{64}$`)

	// ErrDecisionIdempotencyConflict is returned when a correctness event key
	// is reused with a different business payload.
	ErrDecisionIdempotencyConflict = errors.New("decision idempotency conflict")
)

func isDecisionCorrectnessEvent(eventType string) bool {
	switch EventType(eventType) {
	case EventDecisionRunOpened, EventDecisionRunAttached,
		EventPrimaryDecisionPrepared, EventPrimaryDecisionAdmitted,
		EventDecisionRoleCallStarted, EventDecisionRoleCallUnconfirmed,
		EventDecisionRoleCallSettled, EventPrimaryDecisionBlocked,
		EventPrimaryDecisionBound, EventPrimaryDecisionInvalidated:
		return true
	default:
		return false
	}
}

func validateDecisionCorrectnessEvent(event RunEvent) error {
	var err error
	switch EventType(event.Type) {
	case EventDecisionRunOpened:
		err = validateDecisionRunOpenedEvent(event)
	case EventDecisionRunAttached:
		err = validateDecisionRunAttachedEvent(event)
	case EventPrimaryDecisionPrepared:
		err = validatePrimaryDecisionPreparedEvent(event)
	case EventPrimaryDecisionAdmitted:
		err = validatePrimaryDecisionAdmittedEvent(event)
	case EventDecisionRoleCallStarted:
		err = validateDecisionRoleCallStartedEvent(event)
	case EventDecisionRoleCallUnconfirmed:
		err = validateDecisionRoleCallUnconfirmedEvent(event)
	case EventDecisionRoleCallSettled:
		err = validateDecisionRoleCallSettledEvent(event)
	case EventPrimaryDecisionBlocked:
		err = validatePrimaryDecisionBlockedEvent(event)
	case EventPrimaryDecisionBound:
		err = validatePrimaryDecisionBoundEvent(event)
	case EventPrimaryDecisionInvalidated:
		err = validatePrimaryDecisionInvalidatedEvent(event)
	default:
		return fmt.Errorf("unknown decision correctness event %q", event.Type)
	}
	if err != nil {
		return fmt.Errorf("invalid %s payload: %w", event.Type, err)
	}
	return nil
}

func validateDecisionRunOpenedEvent(event RunEvent) error {
	var payload DecisionRunOpenedPayload
	if err := decodeStrictDecisionPayload(event.Payload, &payload); err != nil {
		return err
	}
	if err := validateDecisionCommon(event, payload.decisionEventCommon); err != nil {
		return err
	}
	return validateDecisionOpenedPayload(payload)
}

func validateDecisionRunAttachedEvent(event RunEvent) error {
	var payload DecisionRunAttachedPayload
	if err := decodeStrictDecisionPayload(event.Payload, &payload); err != nil {
		return err
	}
	if err := validateDecisionCommon(event, payload.decisionEventCommon); err != nil {
		return err
	}
	if !validDecisionIdentifier(payload.PreviousExecutionRunID) || !validDecisionIdentifier(payload.ResumeFromEventID) {
		return fmt.Errorf("attached payload has invalid resume identity")
	}
	return nil
}

func validatePrimaryDecisionPreparedEvent(event RunEvent) error {
	var payload PrimaryDecisionPreparedPayload
	if err := decodeStrictDecisionPayload(event.Payload, &payload); err != nil {
		return err
	}
	if err := validateDecisionGenerationCommon(event, payload.decisionGenerationEventCommon); err != nil {
		return err
	}
	return validatePreparedPayload(payload)
}

func validatePrimaryDecisionAdmittedEvent(event RunEvent) error {
	var payload PrimaryDecisionAdmittedPayload
	if err := decodeStrictDecisionPayload(event.Payload, &payload); err != nil {
		return err
	}
	if err := validateDecisionGenerationCommon(event, payload.decisionGenerationEventCommon); err != nil {
		return err
	}
	if !validDecisionIdentifier(payload.TaskID) || !validDecisionIdentifier(payload.DecisionID) || !validDecisionDigest(payload.PreparationDigest) || validateDecisionArtifactRef(payload.OccurrenceRef) != nil || validateDecisionArtifactRef(payload.AdmissionRef) != nil {
		return fmt.Errorf("admitted payload has invalid identity or artifact reference")
	}
	return nil
}

func validateDecisionRoleCallStartedEvent(event RunEvent) error {
	var payload DecisionRoleCallStartedPayload
	if err := decodeStrictDecisionPayload(event.Payload, &payload); err != nil {
		return err
	}
	if err := validateDecisionGenerationCommon(event, payload.decisionGenerationEventCommon); err != nil {
		return err
	}
	if !validDecisionIdentifier(payload.DecisionID) || !validDecisionIdentifier(payload.Role) || payload.Ordinal == 0 || payload.Round == 0 || payload.InvocationAttempt == 0 || !validDecisionIdentifier(payload.BindingID) || !validDecisionIdentifier(payload.InvocationID) || !validDecisionDigest(payload.InputDigest) || validateDecisionArtifactRef(payload.ReservationRef) != nil {
		return fmt.Errorf("role-call-started payload has invalid identity, ordinal, or reference")
	}
	return nil
}

func validateDecisionRoleCallUnconfirmedEvent(event RunEvent) error {
	var payload DecisionRoleCallUnconfirmedPayload
	if err := decodeStrictDecisionPayload(event.Payload, &payload); err != nil {
		return err
	}
	if err := validateDecisionGenerationCommon(event, payload.decisionGenerationEventCommon); err != nil {
		return err
	}
	if !validDecisionIdentifier(payload.InvocationID) || !validDecisionIdentifier(payload.ReasonCode) || validateDecisionArtifactRef(payload.ReservationRef) != nil {
		return fmt.Errorf("unconfirmed role call has invalid identity or reservation")
	}
	return nil
}

func validateDecisionRoleCallSettledEvent(event RunEvent) error {
	var payload DecisionRoleCallSettledPayload
	if err := decodeStrictDecisionPayload(event.Payload, &payload); err != nil {
		return err
	}
	if err := validateDecisionGenerationCommon(event, payload.decisionGenerationEventCommon); err != nil {
		return err
	}
	return validateRoleCallSettlement(payload)
}

func validatePrimaryDecisionBlockedEvent(event RunEvent) error {
	var payload PrimaryDecisionBlockedPayload
	if err := decodeStrictDecisionPayload(event.Payload, &payload); err != nil {
		return err
	}
	if err := validateDecisionGenerationCommon(event, payload.decisionGenerationEventCommon); err != nil {
		return err
	}
	return validateBlockedPayload(payload)
}

func validatePrimaryDecisionBoundEvent(event RunEvent) error {
	var payload PrimaryDecisionBoundPayload
	if err := decodeStrictDecisionPayload(event.Payload, &payload); err != nil {
		return err
	}
	if err := validateDecisionGenerationCommon(event, payload.decisionGenerationEventCommon); err != nil {
		return err
	}
	if err := validatePrimaryBinding(payload.Binding); err != nil {
		return err
	}
	return validateDecisionArtifactRef(payload.RecordValidationReceiptRef)
}

func validatePrimaryDecisionInvalidatedEvent(event RunEvent) error {
	var payload PrimaryDecisionInvalidatedPayload
	if err := decodeStrictDecisionPayload(event.Payload, &payload); err != nil {
		return err
	}
	if err := validateDecisionGenerationCommon(event, payload.decisionGenerationEventCommon); err != nil {
		return err
	}
	return validateInvalidatedPayload(payload)
}

func decodeStrictDecisionPayload(data []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err == nil {
		return fmt.Errorf("trailing JSON value")
	} else if err != io.EOF {
		return err
	}
	return nil
}

func validateDecisionCommon(event RunEvent, common decisionEventCommon) error {
	if common.SchemaVersion != 1 {
		return fmt.Errorf("unsupported payload schema version %d", common.SchemaVersion)
	}
	if !decisionLogicalIDPattern.MatchString(common.LogicalRunID) || !validDecisionIdentifier(common.BranchID) || !validDecisionIdentifier(common.ExecutionRunID) || common.OwnerEpoch == 0 || common.OwnerEpoch > maxCanonicalJSONInteger {
		return fmt.Errorf("invalid logical, branch, execution, or owner identity")
	}
	if event.BranchID == "" || common.BranchID != event.BranchID || common.ExecutionRunID != event.RunID {
		return fmt.Errorf("payload does not match event envelope")
	}
	return nil
}

func validateDecisionGenerationCommon(event RunEvent, common decisionGenerationEventCommon) error {
	if err := validateDecisionCommon(event, common.decisionEventCommon); err != nil {
		return err
	}
	if common.Generation == 0 {
		return fmt.Errorf("generation must be positive")
	}
	return nil
}

func validateDecisionOpenedPayload(payload DecisionRunOpenedPayload) error {
	if payload.OwnerEpoch != 1 {
		return fmt.Errorf("opened payload has invalid common identity")
	}
	if !validDecisionIdentifier(payload.TeamID) || !validDecisionDigest(payload.TeamDefinitionDigest) || !validDecisionDigest(payload.RequirementDigest) {
		return fmt.Errorf("opened payload has invalid team or requirement identity")
	}
	for _, ref := range []DecisionArtifactRef{payload.RequirementRef, payload.ProfileBundleRef, payload.AuthoritySnapshotRef} {
		if err := validateDecisionArtifactRef(ref); err != nil {
			return err
		}
	}
	limits := payload.EffectiveLimits
	if limits.TotalDecisionTokens == 0 || limits.ActiveDecisionDurationMS == 0 || limits.MaxGenerations == 0 || limits.MaxStageInvocationAttempts == 0 || limits.CleanupTimeoutMS == 0 {
		return fmt.Errorf("opened payload has non-positive effective limit")
	}
	return nil
}

func validatePreparedPayload(payload PrimaryDecisionPreparedPayload) error {
	if !validDecisionIdentifier(payload.TaskID) || !validDecisionIdentifier(payload.DecisionID) || !validDecisionIdentifier(payload.SupportCursor.EventID) || !validDecisionDigest(payload.SupportCursor.EventHash) || !validDecisionDigest(payload.SupportCursor.LineageDigest) || !validDecisionDigest(payload.SupportRevisionDigest) || !validDecisionDigest(payload.PreparationDigest) {
		return fmt.Errorf("prepared payload has invalid identity or digest")
	}
	if err := validateDecisionArtifactRef(payload.BaseEvidenceRef); err != nil {
		return err
	}
	return validateDecisionArtifactRef(payload.RolePlanRef)
}

func validateRoleCallSettlement(payload DecisionRoleCallSettledPayload) error {
	if !validDecisionIdentifier(payload.InvocationID) || validateDecisionArtifactRef(payload.ReceiptRef) != nil {
		return fmt.Errorf("settlement has invalid invocation or receipt")
	}
	if payload.OutputRef != nil {
		if err := validateDecisionArtifactRef(*payload.OutputRef); err != nil {
			return err
		}
	}
	if payload.Status != "accepted" && payload.Status != "rejected" && payload.Status != "failed" {
		return fmt.Errorf("settlement has unsupported status %q", payload.Status)
	}
	if payload.UsageState != "observed" && payload.UsageState != "conservative" {
		return fmt.Errorf("settlement has unsupported usage state %q", payload.UsageState)
	}
	if payload.ReasonCode != nil && !validDecisionIdentifier(*payload.ReasonCode) {
		return fmt.Errorf("settlement has invalid reason code")
	}
	return nil
}

func validateBlockedPayload(payload PrimaryDecisionBlockedPayload) error {
	if payload.TaskID != nil && !validDecisionIdentifier(*payload.TaskID) || !validDecisionIdentifier(payload.ReasonCode) || !validDecisionDigest(payload.SupportRevisionDigest) || len(payload.MissingRequirements) > 128 {
		return fmt.Errorf("blocked payload has invalid identity or requirements")
	}
	for _, missing := range payload.MissingRequirements {
		if !validDecisionIdentifier(missing.Code) || missing.CriterionID != nil && !validDecisionIdentifier(*missing.CriterionID) {
			return fmt.Errorf("blocked payload has invalid missing requirement")
		}
	}
	return nil
}

func validateInvalidatedPayload(payload PrimaryDecisionInvalidatedPayload) error {
	if !validDecisionIdentifier(payload.TaskID) || !validDecisionIdentifier(payload.DecisionID) || payload.NextGeneration != payload.Generation+1 || !validDecisionIdentifier(payload.ReasonCode) || len(payload.TriggerEventIDs) == 0 {
		return fmt.Errorf("invalidation has invalid identity, generation, or trigger set")
	}
	if payload.PriorBindingEventID != nil && !validDecisionIdentifier(*payload.PriorBindingEventID) {
		return fmt.Errorf("invalidation has invalid prior binding event")
	}
	for _, id := range payload.TriggerEventIDs {
		if !validDecisionIdentifier(id) {
			return fmt.Errorf("invalidation has invalid trigger event")
		}
	}
	return nil
}

func validatePrimaryBinding(binding PrimaryBindingV1) error {
	if binding.SchemaVersion != 1 || !decisionLogicalIDPattern.MatchString(binding.LogicalRunID) || !validDecisionIdentifier(binding.BranchID) || !validDecisionIdentifier(binding.TaskID) || binding.Generation == 0 || !validDecisionIdentifier(binding.DecisionID) || !validDecisionDigest(binding.RequirementDigest) || !validDecisionDigest(binding.SupportRevisionDigest) || !validDecisionDigest(binding.SealedEvidenceHash) {
		return fmt.Errorf("primary binding has invalid identity or digest")
	}
	for _, ref := range []DecisionArtifactRef{binding.AdmissionRef, binding.BaseEvidenceRef, binding.RolePlanRef, binding.RecordRef} {
		if err := validateDecisionArtifactRef(ref); err != nil {
			return err
		}
	}
	return nil
}

func validateDecisionArtifactRef(ref DecisionArtifactRef) error {
	if !validDecisionIdentifier(ref.ID) || !validDecisionDigest(ref.SHA256) || strings.TrimSpace(ref.MediaType) == "" || len(ref.MediaType) > 128 || ref.SizeBytes > maxCanonicalJSONInteger {
		return fmt.Errorf("invalid decision artifact reference %q", ref.ID)
	}
	return nil
}

func validDecisionIdentifier(value string) bool {
	return len(value) <= 160 && decisionIdentifierPattern.MatchString(value)
}

func validDecisionDigest(value string) bool {
	return decisionDigestPattern.MatchString(value)
}

func decisionBusinessPayload(payload json.RawMessage) ([]byte, error) {
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.UseNumber()
	var value map[string]any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	delete(value, "execution_run_id")
	delete(value, "owner_epoch")
	return CanonicalDecisionJSON(value)
}

func decisionIdempotencyEquivalent(existing, incoming RunEvent) (bool, error) {
	if existing.Type != incoming.Type {
		return false, nil
	}
	left, err := decisionBusinessPayload(existing.Payload)
	if err != nil {
		return false, err
	}
	right, err := decisionBusinessPayload(incoming.Payload)
	if err != nil {
		return false, err
	}
	return bytes.Equal(left, right), nil
}
