package cost

import (
	"strings"
	"testing"
)

func testCostIdentity(invocation string) InvocationIdentity {
	return InvocationIdentity{
		ProviderInvocationID: invocation, RunID: "run-1", TaskID: "task-1", OccurrenceAttempt: 1,
		Agent: "worker", Role: "worker", Purpose: PurposeWorker,
	}
}

func TestCostEventsStrictRoundTrip(t *testing.T) {
	reserved := int64(25)
	event := Event{Kind: EventReservationCommitted, Reservation: &ReservationEvent{
		SchemaVersion: EventSchemaVersion, InvocationIdentity: testCostIdentity("provider-1"),
		ExecutionTarget: "openai/gpt", PriceSnapshotID: "price-1", EstimatedInputTokens: 10,
		ReservedOutputTokens: 20, ReservedMicros: &reserved, EstimateSource: EstimateAdmissionBound,
		BillingMode: BillingMetered, ReservedAt: "2026-09-29T00:00:00Z",
	}}
	payload, err := EncodeEvent(event)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeEvent(string(event.Kind), payload)
	if err != nil {
		t.Fatal(err)
	}
	if !jsonEqual(event.Reservation, decoded.Reservation) {
		t.Fatalf("decoded event = %#v, want %#v", decoded, event)
	}

	unknownField := strings.TrimSuffix(string(payload), "}") + `,"raw_error":"secret"}`
	if _, err := DecodeEvent(string(event.Kind), []byte(unknownField)); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("unknown field error = %v", err)
	}
}

func TestUnknownPriceSnapshotIsDeterministicAndExplicit(t *testing.T) {
	first, err := NewUnknownPriceSnapshot("openai/unpriced")
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewUnknownPriceSnapshot("openai/unpriced")
	if err != nil {
		t.Fatal(err)
	}
	if first.ID == "" || first.CatalogHash == "" || !jsonEqual(first, second) || first.BillingMode != BillingUnknown {
		t.Fatalf("unknown snapshots = %#v %#v", first, second)
	}
	if err := ValidatePriceSnapshot(first); err != nil {
		t.Fatal(err)
	}
}
