package cost

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"
)

type InvocationProjection struct {
	Reservation ReservationEvent `json:"reservation"`
	Settlement  *SettlementEvent `json:"settlement,omitempty"`
}

type Aggregate struct {
	InvocationCount         int    `json:"invocation_count"`
	OpenReservationCount    int    `json:"open_reservation_count,omitzero"`
	KnownMicros             *int64 `json:"known_micros,omitempty"`
	UsageDerivedMicros      *int64 `json:"usage_derived_micros,omitempty"`
	AdmissionBoundMicros    *int64 `json:"admission_bound_micros,omitempty"`
	NotMeteredInvocations   int    `json:"not_metered_invocations,omitzero"`
	SubscriptionInvocations int    `json:"subscription_invocations,omitzero"`
	UnknownInvocations      int    `json:"unknown_invocations,omitzero"`
}

type Summary struct {
	Total     Aggregate            `json:"total"`
	ByRun     map[string]Aggregate `json:"by_run,omitempty"`
	ByTask    map[string]Aggregate `json:"by_task,omitempty"`
	ByRole    map[string]Aggregate `json:"by_role,omitempty"`
	ByPurpose map[string]Aggregate `json:"by_purpose,omitempty"`
	ByTarget  map[string]Aggregate `json:"by_target,omitempty"`
	BySource  map[string]Aggregate `json:"by_source,omitempty"`
}

// Projection is the pure replay state for canonical cost events. Available is
// false only when no cost event was observed; this keeps an old workspace
// distinguishable from a known zero-cost run.
type Projection struct {
	Available   bool                            `json:"available"`
	Prices      map[string]PriceSnapshot        `json:"prices,omitempty"`
	Invocations map[string]InvocationProjection `json:"invocations,omitempty"`
	Warnings    map[string]BudgetWarningEvent   `json:"warnings,omitempty"`
	Denials     map[string]BudgetDeniedEvent    `json:"denials,omitempty"`
}

func NewProjection() Projection {
	return Projection{
		Prices: make(map[string]PriceSnapshot), Invocations: make(map[string]InvocationProjection),
		Warnings: make(map[string]BudgetWarningEvent), Denials: make(map[string]BudgetDeniedEvent),
	}
}

func Reduce(events []Event) (Projection, error) {
	projection := NewProjection()
	for index, event := range events {
		if err := projection.Apply(event); err != nil {
			return Projection{}, fmt.Errorf("reduce cost event %d (%s): %w", index, event.Kind, err)
		}
	}
	return projection, nil
}

func (p *Projection) Apply(event Event) error {
	if p == nil {
		return fmt.Errorf("cost projection is nil")
	}
	if err := event.Validate(); err != nil {
		return err
	}
	p.ensureMaps()
	p.Available = true
	switch event.Kind {
	case EventPriceSnapshotResolved:
		key := priceKey(event.Price.RunID, event.Price.Price.ID)
		existing, ok := p.Prices[key]
		if ok && !jsonEqual(existing, event.Price.Price) {
			return fmt.Errorf("price snapshot %q changed contents", event.Price.Price.ID)
		}
		p.Prices[key] = clonePriceSnapshot(event.Price.Price)
	case EventReservationCommitted:
		price, ok := p.Prices[priceKey(event.Reservation.RunID, event.Reservation.PriceSnapshotID)]
		if !ok {
			return fmt.Errorf("reservation %q references unresolved price %q", event.Reservation.ProviderInvocationID, event.Reservation.PriceSnapshotID)
		}
		if price.ExecutionTarget != event.Reservation.ExecutionTarget || price.BillingMode != event.Reservation.BillingMode {
			return fmt.Errorf("reservation %q disagrees with price snapshot", event.Reservation.ProviderInvocationID)
		}
		key := invocationKey(event.Reservation.RunID, event.Reservation.ProviderInvocationID)
		existing, ok := p.Invocations[key]
		if ok {
			if !jsonEqual(existing.Reservation, *event.Reservation) {
				return fmt.Errorf("reservation %q changed contents", event.Reservation.ProviderInvocationID)
			}
			return nil
		}
		p.Invocations[key] = InvocationProjection{Reservation: cloneReservation(*event.Reservation)}
	case EventSettled:
		key := invocationKey(event.Settlement.RunID, event.Settlement.ProviderInvocationID)
		invocation, ok := p.Invocations[key]
		if !ok {
			return fmt.Errorf("settlement %q has no reservation", event.Settlement.ProviderInvocationID)
		}
		if !sameInvocation(invocation.Reservation, *event.Settlement) {
			return fmt.Errorf("settlement %q disagrees with its reservation", event.Settlement.ProviderInvocationID)
		}
		if invocation.Settlement != nil {
			if !jsonEqual(*invocation.Settlement, *event.Settlement) {
				return fmt.Errorf("settlement %q changed contents", event.Settlement.ProviderInvocationID)
			}
			return nil
		}
		invocation.Settlement = cloneSettlement(event.Settlement)
		p.Invocations[key] = invocation
	case EventBudgetWarning:
		key := fmt.Sprintf("%s\x00%d", event.Warning.RunID, event.Warning.ThresholdMicros)
		if existing, ok := p.Warnings[key]; ok && !jsonEqual(existing, *event.Warning) {
			return fmt.Errorf("warning threshold %d changed contents", event.Warning.ThresholdMicros)
		}
		p.Warnings[key] = *event.Warning
	case EventBudgetDenied:
		key := invocationKey(event.Denied.RunID, event.Denied.ProviderInvocationID)
		if existing, ok := p.Denials[key]; ok && !jsonEqual(existing, *event.Denied) {
			return fmt.Errorf("denial %q changed contents", event.Denied.ProviderInvocationID)
		}
		p.Denials[key] = *event.Denied
	}
	return nil
}

func (p Projection) Clone() Projection {
	clone := Projection{Available: p.Available, Prices: make(map[string]PriceSnapshot, len(p.Prices)), Invocations: make(map[string]InvocationProjection, len(p.Invocations)), Warnings: maps.Clone(p.Warnings), Denials: maps.Clone(p.Denials)}
	for key, price := range p.Prices {
		clone.Prices[key] = clonePriceSnapshot(price)
	}
	for key, invocation := range p.Invocations {
		invocation.Reservation = cloneReservation(invocation.Reservation)
		invocation.Settlement = cloneSettlement(invocation.Settlement)
		clone.Invocations[key] = invocation
	}
	return clone
}

func (p Projection) Summary() (Summary, error) {
	summary := Summary{
		ByRun: make(map[string]Aggregate), ByTask: make(map[string]Aggregate), ByRole: make(map[string]Aggregate),
		ByPurpose: make(map[string]Aggregate), ByTarget: make(map[string]Aggregate), BySource: make(map[string]Aggregate),
	}
	for _, key := range slices.Sorted(maps.Keys(p.Invocations)) {
		invocation := p.Invocations[key]
		estimate := reservationEstimate(invocation.Reservation)
		open := invocation.Settlement == nil
		if invocation.Settlement != nil {
			estimate = settlementEstimate(*invocation.Settlement)
		}
		if err := addAggregate(&summary.Total, estimate, open); err != nil {
			return Summary{}, err
		}
		keys := []struct {
			values map[string]Aggregate
			key    string
		}{
			{values: summary.ByRun, key: invocation.Reservation.RunID},
			{values: summary.ByTask, key: invocation.Reservation.TaskID},
			{values: summary.ByRole, key: invocation.Reservation.Role},
			{values: summary.ByPurpose, key: string(invocation.Reservation.Purpose)},
			{values: summary.ByTarget, key: invocation.Reservation.ExecutionTarget},
			{values: summary.BySource, key: string(estimate.Source)},
		}
		for _, dimension := range keys {
			if dimension.key == "" {
				continue
			}
			aggregate := dimension.values[dimension.key]
			if err := addAggregate(&aggregate, estimate, open); err != nil {
				return Summary{}, err
			}
			dimension.values[dimension.key] = aggregate
		}
	}
	return summary, nil
}

func (p Projection) KnownMicrosForRun(runID string) (int64, error) {
	summary, err := p.Summary()
	if err != nil {
		return 0, err
	}
	value := summary.ByRun[runID].KnownMicros
	if value == nil {
		return 0, nil
	}
	return *value, nil
}

func (p Projection) Invocation(runID, providerInvocationID string) (InvocationProjection, bool) {
	invocation, ok := p.Invocations[invocationKey(runID, providerInvocationID)]
	if !ok {
		return InvocationProjection{}, false
	}
	invocation.Reservation = cloneReservation(invocation.Reservation)
	invocation.Settlement = cloneSettlement(invocation.Settlement)
	return invocation, true
}

func (p Projection) Price(runID, id string) (PriceSnapshot, bool) {
	price, ok := p.Prices[priceKey(runID, id)]
	return clonePriceSnapshot(price), ok
}

func (p Projection) HasWarning(runID string, thresholdMicros int64) bool {
	_, ok := p.Warnings[fmt.Sprintf("%s\x00%d", runID, thresholdMicros)]
	return ok
}

func (p Projection) Denial(runID, providerInvocationID string) (BudgetDeniedEvent, bool) {
	denial, ok := p.Denials[invocationKey(runID, providerInvocationID)]
	return denial, ok
}

func (p *Projection) ensureMaps() {
	if p.Prices == nil {
		p.Prices = make(map[string]PriceSnapshot)
	}
	if p.Invocations == nil {
		p.Invocations = make(map[string]InvocationProjection)
	}
	if p.Warnings == nil {
		p.Warnings = make(map[string]BudgetWarningEvent)
	}
	if p.Denials == nil {
		p.Denials = make(map[string]BudgetDeniedEvent)
	}
}

func addAggregate(aggregate *Aggregate, estimate Estimate, open bool) error {
	aggregate.InvocationCount++
	if open {
		aggregate.OpenReservationCount++
	}
	switch estimate.Source {
	case EstimateUsage, EstimateAdmissionBound:
		if estimate.Micros == nil {
			return fmt.Errorf("numeric cost source %q has no amount", estimate.Source)
		}
		if err := addAggregateMicros(&aggregate.KnownMicros, *estimate.Micros); err != nil {
			return err
		}
		if estimate.Source == EstimateUsage {
			return addAggregateMicros(&aggregate.UsageDerivedMicros, *estimate.Micros)
		}
		return addAggregateMicros(&aggregate.AdmissionBoundMicros, *estimate.Micros)
	case EstimateNotMetered:
		aggregate.NotMeteredInvocations++
	case EstimateSubscription:
		aggregate.SubscriptionInvocations++
	case EstimateUnknown:
		aggregate.UnknownInvocations++
	default:
		return fmt.Errorf("unsupported estimate source %q", estimate.Source)
	}
	return nil
}

func addAggregateMicros(destination **int64, amount int64) error {
	current := int64(0)
	if *destination != nil {
		current = **destination
	}
	total, err := addMicros(current, amount)
	if err != nil {
		return err
	}
	*destination = new(total)
	return nil
}

func reservationEstimate(event ReservationEvent) Estimate {
	return Estimate{Micros: cloneInt64(event.ReservedMicros), Source: event.EstimateSource, BillingMode: event.BillingMode}
}

func settlementEstimate(event SettlementEvent) Estimate {
	return Estimate{Micros: cloneInt64(event.FinalMicros), Source: event.EstimateSource, BillingMode: event.BillingMode}
}

func sameInvocation(reservation ReservationEvent, settlement SettlementEvent) bool {
	return reservation.InvocationIdentity == settlement.InvocationIdentity && reservation.ExecutionTarget == settlement.ExecutionTarget && reservation.PriceSnapshotID == settlement.PriceSnapshotID && reservation.BillingMode == settlement.BillingMode
}

func invocationKey(runID, providerInvocationID string) string {
	return runID + "\x00" + providerInvocationID
}

func priceKey(runID, priceID string) string {
	return runID + "\x00" + priceID
}

func jsonEqual(left, right any) bool {
	leftJSON, leftErr := json.Marshal(left)
	rightJSON, rightErr := json.Marshal(right)
	return leftErr == nil && rightErr == nil && string(leftJSON) == string(rightJSON)
}

func cloneReservation(event ReservationEvent) ReservationEvent {
	event.ReservedMicros = cloneInt64(event.ReservedMicros)
	return event
}

func cloneSettlement(event *SettlementEvent) *SettlementEvent {
	if event == nil {
		return nil
	}
	clone := *event
	clone.FinalMicros = cloneInt64(event.FinalMicros)
	if event.Usage != nil {
		clone.Usage = new(*event.Usage)
	}
	return &clone
}
