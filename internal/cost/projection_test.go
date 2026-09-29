package cost

import "testing"

func projectionPrice(t *testing.T, target string, config PriceConfig) PriceSnapshot {
	t.Helper()
	catalog, err := NewCatalog(map[string]PriceConfig{target: config})
	if err != nil {
		t.Fatal(err)
	}
	price, ok := catalog.Resolve(target)
	if !ok {
		t.Fatalf("missing price %q", target)
	}
	return price
}

func projectionEvents(t *testing.T, invocation string, price PriceSnapshot, reserved, settled *int64, source EstimateSource) []Event {
	t.Helper()
	identity := testCostIdentity(invocation)
	reservation := ReservationEvent{
		SchemaVersion: EventSchemaVersion, InvocationIdentity: identity, ExecutionTarget: price.ExecutionTarget,
		PriceSnapshotID: price.ID, EstimatedInputTokens: 10, ReservedOutputTokens: 20,
		ReservedMicros: reserved, EstimateSource: EstimateAdmissionBound, BillingMode: price.BillingMode,
		ReservedAt: "2026-09-29T00:00:00Z",
	}
	events := []Event{
		{Kind: EventPriceSnapshotResolved, Price: &PriceSnapshotResolvedEvent{SchemaVersion: EventSchemaVersion, RunID: identity.RunID, Price: price}},
		{Kind: EventReservationCommitted, Reservation: &reservation},
	}
	if settled != nil {
		settlement := SettlementEvent{
			SchemaVersion: EventSchemaVersion, InvocationIdentity: identity, ExecutionTarget: price.ExecutionTarget,
			PriceSnapshotID: price.ID, FinalMicros: settled, EstimateSource: source, BillingMode: price.BillingMode,
			Outcome: OutcomeSuccess, SettledAt: "2026-09-29T00:00:01Z",
		}
		events = append(events, Event{Kind: EventSettled, Settlement: &settlement})
	}
	return events
}

func TestProjectionOpenReservationAndSettlementReplacement(t *testing.T) {
	price := projectionPrice(t, "openai/gpt", PriceConfig{BillingMode: BillingMetered, InputUSDPerMillion: "1", OutputUSDPerMillion: "1"})
	reserved, actual := int64(100), int64(40)
	open, err := Reduce(projectionEvents(t, "provider-1", price, &reserved, nil, EstimateAdmissionBound))
	if err != nil {
		t.Fatal(err)
	}
	openSummary, err := open.Summary()
	if err != nil {
		t.Fatal(err)
	}
	if !open.Available || openSummary.Total.KnownMicros == nil || *openSummary.Total.KnownMicros != reserved || openSummary.Total.OpenReservationCount != 1 || openSummary.Total.AdmissionBoundMicros == nil {
		t.Fatalf("open summary = %#v", openSummary.Total)
	}

	settled, err := Reduce(projectionEvents(t, "provider-1", price, &reserved, &actual, EstimateUsage))
	if err != nil {
		t.Fatal(err)
	}
	settledSummary, err := settled.Summary()
	if err != nil {
		t.Fatal(err)
	}
	if settledSummary.Total.KnownMicros == nil || *settledSummary.Total.KnownMicros != actual || settledSummary.Total.OpenReservationCount != 0 || settledSummary.Total.UsageDerivedMicros == nil || settledSummary.Total.AdmissionBoundMicros != nil {
		t.Fatalf("settled summary = %#v", settledSummary.Total)
	}
}

func TestProjectionDuplicateAndCorruptionRules(t *testing.T) {
	price := projectionPrice(t, "openai/gpt", PriceConfig{BillingMode: BillingMetered, InputUSDPerMillion: "1", OutputUSDPerMillion: "1"})
	reserved := int64(100)
	events := projectionEvents(t, "provider-1", price, &reserved, nil, EstimateAdmissionBound)
	events = append(events, events...)
	projection, err := Reduce(events)
	if err != nil {
		t.Fatal(err)
	}
	if len(projection.Invocations) != 1 {
		t.Fatalf("duplicate replay produced %d invocations", len(projection.Invocations))
	}

	conflicting := projectionEvents(t, "provider-1", price, new(int64(101)), nil, EstimateAdmissionBound)[1]
	if err := projection.Apply(conflicting); err == nil {
		t.Fatal("conflicting duplicate reservation was accepted")
	}
	settlement := SettlementEvent{
		SchemaVersion: EventSchemaVersion, InvocationIdentity: testCostIdentity("missing"), ExecutionTarget: price.ExecutionTarget,
		PriceSnapshotID: price.ID, FinalMicros: &reserved, EstimateSource: EstimateAdmissionBound,
		BillingMode: BillingMetered, Outcome: OutcomeSuccess, SettledAt: "2026-09-29T00:00:01Z",
	}
	if err := projection.Apply(Event{Kind: EventSettled, Settlement: &settlement}); err == nil {
		t.Fatal("settlement without reservation was accepted")
	}
}

func TestProjectionKeepsNonNumericModesDistinct(t *testing.T) {
	projection := NewProjection()
	configs := []struct {
		target string
		price  PriceSnapshot
		source EstimateSource
	}{
		{target: "ollama/local", price: projectionPrice(t, "ollama/local", PriceConfig{BillingMode: BillingLocal}), source: EstimateNotMetered},
		{target: "codex/subscription", price: projectionPrice(t, "codex/subscription", PriceConfig{BillingMode: BillingSubscription}), source: EstimateSubscription},
	}
	unknown, err := NewUnknownPriceSnapshot("openai/unknown")
	if err != nil {
		t.Fatal(err)
	}
	configs = append(configs, struct {
		target string
		price  PriceSnapshot
		source EstimateSource
	}{target: unknown.ExecutionTarget, price: unknown, source: EstimateUnknown})
	for index, entry := range configs {
		identity := testCostIdentity(string(rune('a' + index)))
		priceEvent := Event{Kind: EventPriceSnapshotResolved, Price: &PriceSnapshotResolvedEvent{SchemaVersion: EventSchemaVersion, RunID: identity.RunID, Price: entry.price}}
		reservation := Event{Kind: EventReservationCommitted, Reservation: &ReservationEvent{
			SchemaVersion: EventSchemaVersion, InvocationIdentity: identity, ExecutionTarget: entry.target,
			PriceSnapshotID: entry.price.ID, EstimateSource: entry.source, BillingMode: entry.price.BillingMode,
			ReservedAt: "2026-09-29T00:00:00Z",
		}}
		if err := projection.Apply(priceEvent); err != nil {
			t.Fatal(err)
		}
		if err := projection.Apply(reservation); err != nil {
			t.Fatal(err)
		}
	}
	summary, err := projection.Summary()
	if err != nil {
		t.Fatal(err)
	}
	if summary.Total.KnownMicros != nil || summary.Total.NotMeteredInvocations != 1 || summary.Total.SubscriptionInvocations != 1 || summary.Total.UnknownInvocations != 1 {
		t.Fatalf("non-numeric summary = %#v", summary.Total)
	}
	if empty, err := Reduce(nil); err != nil || empty.Available {
		t.Fatalf("empty projection = %#v, err=%v", empty, err)
	}
}
