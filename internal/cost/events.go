package cost

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"
)

const EventSchemaVersion = 1

type EventKind string

const (
	EventPriceSnapshotResolved EventKind = "cost_price_snapshot_resolved"
	EventReservationCommitted  EventKind = "cost_reservation_committed"
	EventSettled               EventKind = "cost_settled"
	EventBudgetWarning         EventKind = "cost_budget_warning"
	EventBudgetDenied          EventKind = "cost_budget_denied"
)

type Purpose string

const (
	PurposeCoordinator  Purpose = "coordinator"
	PurposeWorker       Purpose = "worker"
	PurposeDirectAgent  Purpose = "direct_agent"
	PurposeSidecar      Purpose = "sidecar"
	PurposeGuard        Purpose = "guard"
	PurposeJudge        Purpose = "judge"
	PurposeSkeptic      Purpose = "skeptic"
	PurposePlanReviewer Purpose = "plan_reviewer"
	PurposeRepair       Purpose = "protocol_repair"
	PurposeExternal     Purpose = "external_agent"
)

type Outcome string

const (
	OutcomeSuccess         Outcome = "success"
	OutcomeProviderError   Outcome = "provider_error"
	OutcomeCancelled       Outcome = "cancelled"
	OutcomeStreamAbandoned Outcome = "stream_abandoned"
)

type DenialReason string

const (
	DenialUnknownPrice   DenialReason = "unknown_price"
	DenialUnboundedCost  DenialReason = "unbounded_cost"
	DenialBudgetExceeded DenialReason = "budget_exceeded"
	DenialIntegrity      DenialReason = "integrity_degraded"
)

type InvocationIdentity struct {
	ProviderInvocationID string  `json:"provider_invocation_id"`
	RunID                string  `json:"run_id"`
	TaskID               string  `json:"task_id,omitempty"`
	OccurrenceAttempt    int     `json:"occurrence_attempt,omitzero"`
	Agent                string  `json:"agent"`
	Role                 string  `json:"role"`
	Purpose              Purpose `json:"purpose"`
}

type PriceSnapshotResolvedEvent struct {
	SchemaVersion int           `json:"schema_version"`
	RunID         string        `json:"run_id"`
	Price         PriceSnapshot `json:"price"`
}

type ReservationEvent struct {
	SchemaVersion int `json:"schema_version"`
	InvocationIdentity
	ExecutionTarget      string         `json:"execution_target"`
	PriceSnapshotID      string         `json:"price_snapshot_id"`
	EstimatedInputTokens int64          `json:"estimated_input_tokens"`
	ReservedOutputTokens int64          `json:"reserved_output_tokens"`
	ReservedMicros       *int64         `json:"reserved_micros,omitempty"`
	EstimateSource       EstimateSource `json:"estimate_source"`
	BillingMode          BillingMode    `json:"billing_mode"`
	ReservedAt           string         `json:"reserved_at"`
}

type SettlementEvent struct {
	SchemaVersion int `json:"schema_version"`
	InvocationIdentity
	ExecutionTarget string         `json:"execution_target"`
	PriceSnapshotID string         `json:"price_snapshot_id"`
	Usage           *TokenUsage    `json:"usage,omitempty"`
	FinalMicros     *int64         `json:"final_micros,omitempty"`
	EstimateSource  EstimateSource `json:"estimate_source"`
	BillingMode     BillingMode    `json:"billing_mode"`
	Outcome         Outcome        `json:"outcome"`
	SettledAt       string         `json:"settled_at"`
}

type BudgetWarningEvent struct {
	SchemaVersion   int    `json:"schema_version"`
	RunID           string `json:"run_id"`
	ThresholdMicros int64  `json:"threshold_micros"`
	ProjectedMicros int64  `json:"projected_micros"`
	WarnedAt        string `json:"warned_at"`
}

type BudgetDeniedEvent struct {
	SchemaVersion int `json:"schema_version"`
	InvocationIdentity
	ExecutionTarget string       `json:"execution_target"`
	Reason          DenialReason `json:"reason"`
	DeniedAt        string       `json:"denied_at"`
}

type Event struct {
	Kind        EventKind
	Price       *PriceSnapshotResolvedEvent
	Reservation *ReservationEvent
	Settlement  *SettlementEvent
	Warning     *BudgetWarningEvent
	Denied      *BudgetDeniedEvent
}

func DecodeEvent(kind string, payload []byte) (Event, error) {
	event := Event{Kind: EventKind(kind)}
	var err error
	switch event.Kind {
	case EventPriceSnapshotResolved:
		event.Price, err = decodeStrict[PriceSnapshotResolvedEvent](payload)
	case EventReservationCommitted:
		event.Reservation, err = decodeStrict[ReservationEvent](payload)
	case EventSettled:
		event.Settlement, err = decodeStrict[SettlementEvent](payload)
	case EventBudgetWarning:
		event.Warning, err = decodeStrict[BudgetWarningEvent](payload)
	case EventBudgetDenied:
		event.Denied, err = decodeStrict[BudgetDeniedEvent](payload)
	default:
		return Event{}, fmt.Errorf("unsupported cost event type %q", kind)
	}
	if err != nil {
		return Event{}, fmt.Errorf("decode %s payload: %w", kind, err)
	}
	if err := event.Validate(); err != nil {
		return Event{}, err
	}
	return event, nil
}

func (e Event) Validate() error {
	switch e.Kind {
	case EventPriceSnapshotResolved:
		return validatePriceEvent(e.Price)
	case EventReservationCommitted:
		return validateReservationEvent(e.Reservation)
	case EventSettled:
		return validateSettlementEvent(e.Settlement)
	case EventBudgetWarning:
		return validateWarningEvent(e.Warning)
	case EventBudgetDenied:
		return validateDeniedEvent(e.Denied)
	default:
		return fmt.Errorf("unsupported cost event type %q", e.Kind)
	}
}

func validatePriceEvent(event *PriceSnapshotResolvedEvent) error {
	if event == nil || event.SchemaVersion != EventSchemaVersion || !validMetadata(event.RunID) {
		return fmt.Errorf("cost price snapshot event has invalid identity")
	}
	if err := ValidatePriceSnapshot(event.Price); err != nil {
		return fmt.Errorf("cost price snapshot event: %w", err)
	}
	return nil
}

func validateReservationEvent(event *ReservationEvent) error {
	if event == nil || event.SchemaVersion != EventSchemaVersion {
		return fmt.Errorf("cost reservation event has unsupported schema")
	}
	if err := validateInvocationIdentity(event.InvocationIdentity); err != nil {
		return fmt.Errorf("cost reservation event: %w", err)
	}
	if !validMetadata(event.ExecutionTarget) || !validMetadata(event.PriceSnapshotID) || event.EstimatedInputTokens < 0 || event.ReservedOutputTokens < 0 || !validTimestamp(event.ReservedAt) {
		return fmt.Errorf("cost reservation event has invalid reservation fields")
	}
	if err := validateEstimate(event.BillingMode, event.EstimateSource, event.ReservedMicros, true); err != nil {
		return fmt.Errorf("cost reservation event: %w", err)
	}
	return nil
}

func validateSettlementEvent(event *SettlementEvent) error {
	if event == nil || event.SchemaVersion != EventSchemaVersion {
		return fmt.Errorf("cost settlement event has unsupported schema")
	}
	if err := validateInvocationIdentity(event.InvocationIdentity); err != nil {
		return fmt.Errorf("cost settlement event: %w", err)
	}
	if !validMetadata(event.ExecutionTarget) || !validMetadata(event.PriceSnapshotID) || !validTimestamp(event.SettledAt) || !validOutcome(event.Outcome) {
		return fmt.Errorf("cost settlement event has invalid settlement fields")
	}
	if event.Usage != nil {
		if err := validateUsage(*event.Usage); err != nil {
			return fmt.Errorf("cost settlement event: %w", err)
		}
	}
	if err := validateEstimate(event.BillingMode, event.EstimateSource, event.FinalMicros, false); err != nil {
		return fmt.Errorf("cost settlement event: %w", err)
	}
	return nil
}

func validateWarningEvent(event *BudgetWarningEvent) error {
	if event == nil || event.SchemaVersion != EventSchemaVersion || !validMetadata(event.RunID) || event.ThresholdMicros <= 0 || event.ProjectedMicros < event.ThresholdMicros || !validTimestamp(event.WarnedAt) {
		return fmt.Errorf("cost warning event is invalid")
	}
	return nil
}

func validateDeniedEvent(event *BudgetDeniedEvent) error {
	if event == nil || event.SchemaVersion != EventSchemaVersion {
		return fmt.Errorf("cost denial event has unsupported schema")
	}
	if err := validateInvocationIdentity(event.InvocationIdentity); err != nil {
		return fmt.Errorf("cost denial event: %w", err)
	}
	if !validMetadata(event.ExecutionTarget) || !validDenialReason(event.Reason) || !validTimestamp(event.DeniedAt) {
		return fmt.Errorf("cost denial event is invalid")
	}
	return nil
}

func EncodeEvent(event Event) ([]byte, error) {
	if err := event.Validate(); err != nil {
		return nil, err
	}
	var payload any
	switch event.Kind {
	case EventPriceSnapshotResolved:
		payload = event.Price
	case EventReservationCommitted:
		payload = event.Reservation
	case EventSettled:
		payload = event.Settlement
	case EventBudgetWarning:
		payload = event.Warning
	case EventBudgetDenied:
		payload = event.Denied
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal %s payload: %w", event.Kind, err)
	}
	return encoded, nil
}

func decodeStrict[T any](payload []byte) (*T, error) {
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	var value T
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err == nil {
		return nil, fmt.Errorf("trailing JSON value")
	} else if err != io.EOF {
		return nil, err
	}
	return &value, nil
}

func validateInvocationIdentity(identity InvocationIdentity) error {
	for name, value := range map[string]string{
		"provider invocation": identity.ProviderInvocationID,
		"run":                 identity.RunID,
		"agent":               identity.Agent,
		"role":                identity.Role,
		"purpose":             string(identity.Purpose),
	} {
		if !validMetadata(value) {
			return fmt.Errorf("%s identity is invalid", name)
		}
	}
	if identity.TaskID != "" && !validMetadata(identity.TaskID) {
		return fmt.Errorf("task identity is invalid")
	}
	if identity.OccurrenceAttempt < 0 || (identity.TaskID != "" && identity.OccurrenceAttempt < 1) {
		return fmt.Errorf("occurrence attempt is invalid")
	}
	return nil
}

func validateEstimate(mode BillingMode, source EstimateSource, micros *int64, reservation bool) error {
	if micros != nil && *micros < 0 {
		return fmt.Errorf("estimated micros must be non-negative")
	}
	switch mode {
	case BillingMetered:
		if micros == nil || (reservation && source != EstimateAdmissionBound) || (!reservation && source != EstimateUsage && source != EstimateAdmissionBound) {
			return fmt.Errorf("metered estimate has invalid source or amount")
		}
	case BillingLocal:
		if micros != nil || source != EstimateNotMetered {
			return fmt.Errorf("local estimate must be non-metered")
		}
	case BillingSubscription:
		if micros != nil || source != EstimateSubscription {
			return fmt.Errorf("subscription estimate has invalid source")
		}
	case BillingUnknown:
		if micros != nil || source != EstimateUnknown {
			return fmt.Errorf("unknown estimate must remain non-numeric")
		}
	default:
		return fmt.Errorf("estimate has unsupported billing mode %q", mode)
	}
	return nil
}

func validMetadata(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 256 {
		return false
	}
	for _, char := range value {
		if char < 0x20 || char == 0x7f {
			return false
		}
	}
	return true
}

func validTimestamp(value string) bool {
	_, err := time.Parse(time.RFC3339Nano, value)
	return err == nil
}

func validOutcome(outcome Outcome) bool {
	return outcome == OutcomeSuccess || outcome == OutcomeProviderError || outcome == OutcomeCancelled || outcome == OutcomeStreamAbandoned
}

func validDenialReason(reason DenialReason) bool {
	return reason == DenialUnknownPrice || reason == DenialUnboundedCost || reason == DenialBudgetExceeded || reason == DenialIntegrity
}
