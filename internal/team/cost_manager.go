package team

import (
	"bytes"
	"context"
	"fmt"
	"math"
	"sync"
	"time"

	"github.com/kjelly/hufu/internal/cost"
)

const costSettlementPersistenceTimeout = 5 * time.Second

type CostReservationRequest struct {
	Identity             cost.InvocationIdentity
	ExecutionTarget      string
	EstimatedInputTokens int64
	ReservedOutputTokens int64
	// Opaque requires admission from the configured per-invocation maximum,
	// even when the target also publishes token rates. External execution
	// backends cannot prove their serialized request size before launch.
	Opaque bool
}

type CostSettlementRequest struct {
	RunID                string
	ProviderInvocationID string
	Usage                *cost.TokenUsage
	Outcome              cost.Outcome
}

type CostAdmissionError struct {
	Reason cost.DenialReason
	Detail string
	Cause  error
}

func (e *CostAdmissionError) Error() string {
	if e == nil {
		return "cost admission denied"
	}
	message := "cost admission denied"
	if e.Reason != "" {
		message += ": " + string(e.Reason)
	}
	if e.Detail != "" {
		message += " (" + e.Detail + ")"
	}
	if e.Cause != nil {
		message += ": " + e.Cause.Error()
	}
	return message
}

func (e *CostAdmissionError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

// CostManager owns one coordinator tree's in-process economic ledger. Its
// mutex covers the final budget comparison and durable append so concurrent
// workers cannot reserve the same remaining run budget.
type CostManager struct {
	mu         sync.Mutex
	policy     *cost.PolicySnapshot
	prices     map[string]cost.PriceSnapshot
	projection cost.Projection
	integrity  error
	now        func() time.Time
}

func newCostManager(policy *cost.PolicySnapshot) *CostManager {
	if policy == nil {
		return nil
	}
	frozen := cost.ClonePolicySnapshot(policy)
	prices := make(map[string]cost.PriceSnapshot, len(frozen.Prices))
	for _, price := range frozen.Prices {
		prices[price.ExecutionTarget] = price
	}
	return &CostManager{
		policy: frozen, prices: prices,
		projection: cost.NewProjection(), now: time.Now,
	}
}

func (m *CostManager) Rehydrate(events []RunEvent) error {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	decoded, err := costEventsFromRunEvents(events)
	if err != nil {
		m.integrity = err
		return err
	}
	projection, err := cost.Reduce(decoded)
	if err != nil {
		m.integrity = err
		return err
	}
	m.projection = projection
	m.integrity = nil
	return nil
}

func (m *CostManager) Projection() cost.Projection {
	if m == nil {
		return cost.Projection{}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.projection.Clone()
}

func (m *CostManager) IntegrityError() error {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.integrity
}

func (m *CostManager) Reserve(ctx context.Context, journal EventJournal, request CostReservationRequest) (cost.ReservationEvent, error) {
	if m == nil {
		return cost.ReservationEvent{}, fmt.Errorf("cost manager is disabled")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if ctx == nil {
		ctx = context.Background()
	}
	if m.integrity != nil {
		return cost.ReservationEvent{}, &CostAdmissionError{Reason: cost.DenialIntegrity, Detail: "cost ledger integrity is degraded", Cause: m.integrity}
	}
	if denied, ok := m.projection.Denial(request.Identity.RunID, request.Identity.ProviderInvocationID); ok {
		if denied.InvocationIdentity != request.Identity || denied.ExecutionTarget != request.ExecutionTarget {
			return cost.ReservationEvent{}, fmt.Errorf("cost denial idempotency conflict for provider invocation %q", request.Identity.ProviderInvocationID)
		}
		return cost.ReservationEvent{}, &CostAdmissionError{Reason: denied.Reason, Detail: "provider invocation was already denied"}
	}
	if existing, ok := m.projection.Invocation(request.Identity.RunID, request.Identity.ProviderInvocationID); ok {
		candidate, _, err := m.buildReservation(request)
		if err != nil {
			return cost.ReservationEvent{}, err
		}
		candidate.ReservedAt = existing.Reservation.ReservedAt
		if !costEventsEqual(cost.Event{Kind: cost.EventReservationCommitted, Reservation: &candidate}, cost.Event{Kind: cost.EventReservationCommitted, Reservation: &existing.Reservation}) {
			return cost.ReservationEvent{}, fmt.Errorf("cost reservation idempotency conflict for provider invocation %q", request.Identity.ProviderInvocationID)
		}
		return existing.Reservation, nil
	}

	reservation, price, err := m.buildReservation(request)
	if err != nil {
		return cost.ReservationEvent{}, err
	}
	if err := m.ensurePriceSnapshot(ctx, journal, request.Identity.RunID, price); err != nil {
		return cost.ReservationEvent{}, &CostAdmissionError{Reason: cost.DenialIntegrity, Detail: "price snapshot persistence failed", Cause: err}
	}
	if m.opaquePriceUnbounded(request) &&
		(m.policy.MaxRunMicros != nil || m.policy.UnknownPricePolicy == cost.UnknownPriceDeny) {
		return cost.ReservationEvent{}, m.deny(ctx, journal, request, cost.DenialUnboundedCost, "external execution target has no opaque invocation maximum")
	}
	if reservation.EstimateSource == cost.EstimateUnknown && m.policy.UnknownPricePolicy == cost.UnknownPriceDeny {
		return cost.ReservationEvent{}, m.deny(ctx, journal, request, cost.DenialUnknownPrice, "execution target has no configured price")
	}

	current, err := m.projection.KnownMicrosForRun(request.Identity.RunID)
	if err != nil {
		m.integrity = err
		return cost.ReservationEvent{}, &CostAdmissionError{Reason: cost.DenialIntegrity, Detail: "cost projection failed", Cause: err}
	}
	projected := current
	if reservation.ReservedMicros != nil {
		if current > math.MaxInt64-*reservation.ReservedMicros {
			return cost.ReservationEvent{}, m.deny(ctx, journal, request, cost.DenialUnboundedCost, "projected run cost overflows")
		}
		projected += *reservation.ReservedMicros
	}
	if m.policy.MaxRunMicros != nil && projected > *m.policy.MaxRunMicros {
		detail := fmt.Sprintf("projected %d micros exceeds %d micros", projected, *m.policy.MaxRunMicros)
		return cost.ReservationEvent{}, m.deny(ctx, journal, request, cost.DenialBudgetExceeded, detail)
	}
	if threshold := m.policy.WarningRunMicros; threshold != nil && current < *threshold && projected >= *threshold && !m.projection.HasWarning(request.Identity.RunID, *threshold) {
		warning := cost.BudgetWarningEvent{
			SchemaVersion: cost.EventSchemaVersion, RunID: request.Identity.RunID,
			ThresholdMicros: *threshold, ProjectedMicros: projected, WarnedAt: m.now().UTC().Format(time.RFC3339Nano),
		}
		costEvent := cost.Event{Kind: cost.EventBudgetWarning, Warning: &warning}
		if err := m.append(ctx, journal, costEvent, costWarningKey(warning)); err != nil {
			return cost.ReservationEvent{}, &CostAdmissionError{Reason: cost.DenialIntegrity, Detail: "cost warning persistence failed", Cause: err}
		}
		if err := m.projection.Apply(costEvent); err != nil {
			m.integrity = err
			return cost.ReservationEvent{}, &CostAdmissionError{Reason: cost.DenialIntegrity, Detail: "cost warning projection failed", Cause: err}
		}
	}

	event := cost.Event{Kind: cost.EventReservationCommitted, Reservation: &reservation}
	if err := m.append(ctx, journal, event, costReservationKey(reservation)); err != nil {
		return cost.ReservationEvent{}, &CostAdmissionError{Reason: cost.DenialIntegrity, Detail: "cost reservation persistence failed", Cause: err}
	}
	if err := m.projection.Apply(event); err != nil {
		m.integrity = err
		return cost.ReservationEvent{}, &CostAdmissionError{Reason: cost.DenialIntegrity, Detail: "cost reservation projection failed", Cause: err}
	}
	return reservation, nil
}

func (m *CostManager) Settle(ctx context.Context, journal EventJournal, request CostSettlementRequest) (cost.SettlementEvent, error) {
	if m == nil {
		return cost.SettlementEvent{}, fmt.Errorf("cost manager is disabled")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if ctx == nil {
		ctx = context.Background()
	}
	invocation, ok := m.projection.Invocation(request.RunID, request.ProviderInvocationID)
	if !ok {
		return cost.SettlementEvent{}, fmt.Errorf("cost settlement has no reservation for provider invocation %q", request.ProviderInvocationID)
	}
	settlement := m.buildSettlement(invocation.Reservation, request)
	if invocation.Settlement != nil {
		settlement.SettledAt = invocation.Settlement.SettledAt
		if !costEventsEqual(cost.Event{Kind: cost.EventSettled, Settlement: &settlement}, cost.Event{Kind: cost.EventSettled, Settlement: invocation.Settlement}) {
			return cost.SettlementEvent{}, fmt.Errorf("cost settlement idempotency conflict for provider invocation %q", request.ProviderInvocationID)
		}
		return *invocation.Settlement, nil
	}
	event := cost.Event{Kind: cost.EventSettled, Settlement: &settlement}
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), costSettlementPersistenceTimeout)
	defer cancel()
	if err := m.append(cleanupCtx, journal, event, costSettlementKey(settlement)); err != nil {
		m.integrity = err
		return cost.SettlementEvent{}, fmt.Errorf("persist cost settlement: %w", err)
	}
	if err := m.projection.Apply(event); err != nil {
		m.integrity = err
		return cost.SettlementEvent{}, fmt.Errorf("project cost settlement: %w", err)
	}
	return settlement, nil
}

func (m *CostManager) buildReservation(request CostReservationRequest) (cost.ReservationEvent, cost.PriceSnapshot, error) {
	price, ok := m.prices[request.ExecutionTarget]
	if !ok {
		var err error
		price, err = cost.NewUnknownPriceSnapshot(request.ExecutionTarget)
		if err != nil {
			return cost.ReservationEvent{}, cost.PriceSnapshot{}, err
		}
	}
	if request.Opaque && price.BillingMode == cost.BillingMetered && price.OpaqueMaxMicrosPerInvocation == nil {
		var err error
		price, err = cost.NewUnknownPriceSnapshot(request.ExecutionTarget)
		if err != nil {
			return cost.ReservationEvent{}, cost.PriceSnapshot{}, err
		}
	}
	var estimate cost.Estimate
	var err error
	if request.Opaque && price.BillingMode == cost.BillingMetered {
		if price.OpaqueMaxMicrosPerInvocation == nil {
			estimate = cost.Estimate{Source: cost.EstimateUnknown, BillingMode: cost.BillingUnknown}
		} else {
			estimate, err = cost.EstimateOpaqueBound(price)
		}
	} else {
		estimate, err = cost.EstimateAdmission(price, request.EstimatedInputTokens, request.ReservedOutputTokens)
	}
	if err != nil {
		return cost.ReservationEvent{}, cost.PriceSnapshot{}, fmt.Errorf("estimate cost admission: %w", err)
	}
	reservation := cost.ReservationEvent{
		SchemaVersion: cost.EventSchemaVersion, InvocationIdentity: request.Identity,
		ExecutionTarget: request.ExecutionTarget, PriceSnapshotID: price.ID,
		EstimatedInputTokens: request.EstimatedInputTokens, ReservedOutputTokens: request.ReservedOutputTokens,
		ReservedMicros: estimate.Micros, EstimateSource: estimate.Source, BillingMode: estimate.BillingMode,
		ReservedAt: m.now().UTC().Format(time.RFC3339Nano),
	}
	if _, err := cost.EncodeEvent(cost.Event{Kind: cost.EventReservationCommitted, Reservation: &reservation}); err != nil {
		return cost.ReservationEvent{}, cost.PriceSnapshot{}, err
	}
	return reservation, price, nil
}

func (m *CostManager) opaquePriceUnbounded(request CostReservationRequest) bool {
	price, ok := m.prices[request.ExecutionTarget]
	return request.Opaque && ok && price.BillingMode == cost.BillingMetered && price.OpaqueMaxMicrosPerInvocation == nil
}

func (m *CostManager) buildSettlement(reservation cost.ReservationEvent, request CostSettlementRequest) cost.SettlementEvent {
	estimate := cost.Estimate{Micros: cloneCostMicros(reservation.ReservedMicros), Source: reservation.EstimateSource, BillingMode: reservation.BillingMode}
	var usage *cost.TokenUsage
	if request.Usage != nil && completeCostUsage(*request.Usage) {
		if price, ok := m.projection.Price(reservation.RunID, reservation.PriceSnapshotID); ok {
			if observed, err := cost.EstimateUsageCost(price, *request.Usage); err == nil {
				estimate = observed
				usage = new(*request.Usage)
			}
		}
	}
	return cost.SettlementEvent{
		SchemaVersion: cost.EventSchemaVersion, InvocationIdentity: reservation.InvocationIdentity,
		ExecutionTarget: reservation.ExecutionTarget, PriceSnapshotID: reservation.PriceSnapshotID,
		Usage: usage, FinalMicros: estimate.Micros, EstimateSource: estimate.Source, BillingMode: estimate.BillingMode,
		Outcome: request.Outcome, SettledAt: m.now().UTC().Format(time.RFC3339Nano),
	}
}

func (m *CostManager) ensurePriceSnapshot(ctx context.Context, journal EventJournal, runID string, price cost.PriceSnapshot) error {
	if _, ok := m.projection.Price(runID, price.ID); ok {
		return nil
	}
	payload := cost.PriceSnapshotResolvedEvent{SchemaVersion: cost.EventSchemaVersion, RunID: runID, Price: price}
	event := cost.Event{Kind: cost.EventPriceSnapshotResolved, Price: &payload}
	if err := m.append(ctx, journal, event, costPriceKey(payload)); err != nil {
		return err
	}
	if err := m.projection.Apply(event); err != nil {
		m.integrity = err
		return err
	}
	return nil
}

func (m *CostManager) deny(ctx context.Context, journal EventJournal, request CostReservationRequest, reason cost.DenialReason, detail string) error {
	denied := cost.BudgetDeniedEvent{
		SchemaVersion: cost.EventSchemaVersion, InvocationIdentity: request.Identity,
		ExecutionTarget: request.ExecutionTarget, Reason: reason, DeniedAt: m.now().UTC().Format(time.RFC3339Nano),
	}
	event := cost.Event{Kind: cost.EventBudgetDenied, Denied: &denied}
	appendErr := m.append(ctx, journal, event, costDeniedKey(denied))
	if appendErr == nil {
		if err := m.projection.Apply(event); err != nil {
			m.integrity = err
			appendErr = err
		}
	}
	return &CostAdmissionError{Reason: reason, Detail: detail, Cause: appendErr}
}

func (m *CostManager) append(ctx context.Context, journal EventJournal, event cost.Event, key string) error {
	if journal == nil {
		return fmt.Errorf("cost event journal is unavailable")
	}
	payload, err := cost.EncodeEvent(event)
	if err != nil {
		return err
	}
	runEvent := RunEvent{
		RunID: costEventRunID(event), TaskID: costEventTaskID(event), Attempt: costEventAttempt(event),
		Actor: "runtime", Type: string(event.Kind), IdempotencyKey: key, Payload: payload,
	}
	durable, err := journal.Append(ctx, runEvent)
	if err != nil {
		return err
	}
	decoded, err := cost.DecodeEvent(durable.Type, durable.Payload)
	if err != nil {
		return fmt.Errorf("decode durable cost event: %w", err)
	}
	if durable.RunID != runEvent.RunID || durable.TaskID != runEvent.TaskID || durable.Attempt != runEvent.Attempt || !costEventsEqual(event, decoded) {
		return fmt.Errorf("durable cost event disagrees with idempotency request")
	}
	return nil
}

func completeCostUsage(usage cost.TokenUsage) bool {
	if usage.InputTokens < 0 || usage.CacheReadTokens < 0 || usage.CacheCreationTokens < 0 || usage.OutputTokens < 0 || usage.TotalTokens < 0 {
		return false
	}
	if usage.InputTokens == 0 && usage.CacheReadTokens == 0 && usage.CacheCreationTokens == 0 && usage.OutputTokens == 0 {
		return false
	}
	return usage.TotalTokens == 0 || usage.TotalTokens >= usage.InputTokens+usage.OutputTokens
}

func costEventsEqual(left, right cost.Event) bool {
	leftPayload, leftErr := cost.EncodeEvent(left)
	rightPayload, rightErr := cost.EncodeEvent(right)
	return leftErr == nil && rightErr == nil && left.Kind == right.Kind && bytes.Equal(leftPayload, rightPayload)
}

func cloneCostMicros(value *int64) *int64 {
	if value == nil {
		return nil
	}
	return new(*value)
}

func costPriceKey(event cost.PriceSnapshotResolvedEvent) string {
	return fmt.Sprintf("cost-price:v1:%s:%s", event.RunID, event.Price.ID)
}

func costReservationKey(event cost.ReservationEvent) string {
	return fmt.Sprintf("cost-reserve:v1:%s:%s", event.RunID, event.ProviderInvocationID)
}

func costSettlementKey(event cost.SettlementEvent) string {
	return fmt.Sprintf("cost-settle:v1:%s:%s", event.RunID, event.ProviderInvocationID)
}

func costWarningKey(event cost.BudgetWarningEvent) string {
	return fmt.Sprintf("cost-warning:v1:%s:%d", event.RunID, event.ThresholdMicros)
}

func costDeniedKey(event cost.BudgetDeniedEvent) string {
	return fmt.Sprintf("cost-denied:v1:%s:%s", event.RunID, event.ProviderInvocationID)
}
