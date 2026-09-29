package team

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/cost"
)

func newTestCostManager(t *testing.T, maxUSD, warningUSD string) (*CostManager, cost.PriceSnapshot) {
	t.Helper()
	catalog, err := cost.NewCatalog(map[string]cost.PriceConfig{
		"openai/gpt": {BillingMode: cost.BillingMetered, InputUSDPerMillion: "1", OutputUSDPerMillion: "1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	price, _ := catalog.Resolve("openai/gpt")
	policy, err := cost.ResolveRunPolicy(&cost.PolicyConfig{MaxRunUSD: maxUSD, WarningRunUSD: warningUSD})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := cost.NewPolicySnapshot(policy, []cost.PriceSnapshot{price})
	if err != nil {
		t.Fatal(err)
	}
	manager := newCostManager(snapshot)
	fixed := time.Date(2026, 9, 29, 0, 0, 0, 0, time.UTC)
	manager.now = func() time.Time { return fixed }
	return manager, price
}

func costReservationRequest(invocation string, input, output int64) CostReservationRequest {
	return CostReservationRequest{
		Identity: cost.InvocationIdentity{
			ProviderInvocationID: invocation, RunID: "run-cost", TaskID: "task-1", OccurrenceAttempt: 1,
			Agent: "worker", Role: "worker", Purpose: cost.PurposeWorker,
		},
		ExecutionTarget: "openai/gpt", EstimatedInputTokens: input, ReservedOutputTokens: output,
	}
}

func newCostEventStore(t *testing.T) (*EventStore, EventJournal) {
	t.Helper()
	store, err := NewEventStore(t.TempDir(), "run-cost", "session-cost")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store, eventStoreJournal{store: store}
}

func TestCostManagerSerializesConcurrentReservationsAndPersistsOrder(t *testing.T) {
	manager, _ := newTestCostManager(t, "1.50", "0.50")
	store, journal := newCostEventStore(t)
	requests := []CostReservationRequest{
		costReservationRequest("provider-a", 500_000, 500_000),
		costReservationRequest("provider-b", 500_000, 500_000),
	}
	var wg sync.WaitGroup
	errs := make([]error, len(requests))
	for index, request := range requests {
		wg.Go(func() {
			_, errs[index] = manager.Reserve(t.Context(), journal, request)
		})
	}
	wg.Wait()
	successes, denials, deniedIndex := 0, 0, -1
	for index, err := range errs {
		if err == nil {
			successes++
			continue
		}
		admission, ok := errors.AsType[*CostAdmissionError](err)
		if !ok || admission.Reason != cost.DenialBudgetExceeded {
			t.Fatalf("reservation error = %v", err)
		}
		denials++
		deniedIndex = index
	}
	if successes != 1 || denials != 1 {
		t.Fatalf("successes=%d denials=%d, want 1/1", successes, denials)
	}
	events, err := store.ReadEvents()
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 4 || EventType(events[0].Type) != EventCostPriceSnapshotResolved || EventType(events[1].Type) != EventCostBudgetWarning || EventType(events[2].Type) != EventCostReservationCommitted || EventType(events[3].Type) != EventCostBudgetDenied {
		t.Fatalf("cost event order = %#v", eventTypes(events))
	}
	if _, err := manager.Reserve(t.Context(), journal, requests[deniedIndex]); err == nil {
		t.Fatal("replayed denied reservation succeeded")
	}
	replayedEvents, err := store.ReadEvents()
	if err != nil {
		t.Fatal(err)
	}
	if len(replayedEvents) != len(events) {
		t.Fatalf("denial replay appended %d extra events", len(replayedEvents)-len(events))
	}
	summary, err := manager.Projection().Summary()
	if err != nil {
		t.Fatal(err)
	}
	if summary.Total.KnownMicros == nil || *summary.Total.KnownMicros != 1_000_000 || summary.Total.OpenReservationCount != 1 {
		t.Fatalf("cost total = %#v", summary.Total)
	}
}

func TestCostManagerSettlementReplacesReservationAndIsIdempotent(t *testing.T) {
	manager, _ := newTestCostManager(t, "2", "")
	_, journal := newCostEventStore(t)
	reservation, err := manager.Reserve(t.Context(), journal, costReservationRequest("provider-1", 500_000, 500_000))
	if err != nil {
		t.Fatal(err)
	}
	if reservation.ReservedMicros == nil || *reservation.ReservedMicros != 1_000_000 {
		t.Fatalf("reservation = %#v", reservation)
	}
	request := CostSettlementRequest{
		RunID: "run-cost", ProviderInvocationID: "provider-1",
		Usage:   new(cost.TokenUsage{InputTokens: 100_000, OutputTokens: 100_000, TotalTokens: 200_000}),
		Outcome: cost.OutcomeSuccess,
	}
	first, err := manager.Settle(t.Context(), journal, request)
	if err != nil {
		t.Fatal(err)
	}
	second, err := manager.Settle(t.Context(), journal, request)
	if err != nil {
		t.Fatal(err)
	}
	if first.FinalMicros == nil || *first.FinalMicros != 200_000 || second.SettledAt != first.SettledAt || first.EstimateSource != cost.EstimateUsage {
		t.Fatalf("settlements = %#v %#v", first, second)
	}
	summary, err := manager.Projection().Summary()
	if err != nil {
		t.Fatal(err)
	}
	if summary.Total.KnownMicros == nil || *summary.Total.KnownMicros != 200_000 || summary.Total.OpenReservationCount != 0 {
		t.Fatalf("settled total = %#v", summary.Total)
	}
}

func TestCostManagerRehydrateKeepsOpenReservationCharged(t *testing.T) {
	first, _ := newTestCostManager(t, "1.50", "")
	store, journal := newCostEventStore(t)
	if _, err := first.Reserve(t.Context(), journal, costReservationRequest("provider-open", 500_000, 500_000)); err != nil {
		t.Fatal(err)
	}
	events, err := store.ReadEvents()
	if err != nil {
		t.Fatal(err)
	}
	restarted, _ := newTestCostManager(t, "1.50", "")
	if err := restarted.Rehydrate(events); err != nil {
		t.Fatal(err)
	}
	_, err = restarted.Reserve(t.Context(), journal, costReservationRequest("provider-next", 300_000, 300_000))
	admission, ok := errors.AsType[*CostAdmissionError](err)
	if !ok || admission.Reason != cost.DenialBudgetExceeded {
		t.Fatalf("replayed admission error = %v", err)
	}
}

type costFailingJournal struct {
	EventJournal
	failType EventType
	err      error
}

func (j costFailingJournal) Append(ctx context.Context, event RunEvent) (RunEvent, error) {
	if EventType(event.Type) == j.failType {
		return RunEvent{}, j.err
	}
	return j.EventJournal.Append(ctx, event)
}

func TestCostManagerPersistenceFailuresFailClosed(t *testing.T) {
	t.Run("warning before reservation", func(t *testing.T) {
		manager, _ := newTestCostManager(t, "2", "0.50")
		store, journal := newCostEventStore(t)
		failing := costFailingJournal{EventJournal: journal, failType: EventCostBudgetWarning, err: errors.New("warning sync failed")}
		if _, err := manager.Reserve(t.Context(), failing, costReservationRequest("provider-1", 500_000, 500_000)); err == nil {
			t.Fatal("reservation succeeded after warning append failure")
		}
		events, err := store.ReadEvents()
		if err != nil {
			t.Fatal(err)
		}
		if len(events) != 1 || EventType(events[0].Type) != EventCostPriceSnapshotResolved {
			t.Fatalf("events after warning failure = %#v", eventTypes(events))
		}
	})

	t.Run("settlement latches integrity", func(t *testing.T) {
		manager, _ := newTestCostManager(t, "2", "")
		_, journal := newCostEventStore(t)
		if _, err := manager.Reserve(t.Context(), journal, costReservationRequest("provider-1", 500_000, 500_000)); err != nil {
			t.Fatal(err)
		}
		failing := costFailingJournal{EventJournal: journal, failType: EventCostSettled, err: errors.New("settlement sync failed")}
		_, err := manager.Settle(t.Context(), failing, CostSettlementRequest{RunID: "run-cost", ProviderInvocationID: "provider-1", Outcome: cost.OutcomeProviderError})
		if err == nil || manager.IntegrityError() == nil {
			t.Fatalf("settlement error=%v integrity=%v", err, manager.IntegrityError())
		}
		_, err = manager.Reserve(t.Context(), journal, costReservationRequest("provider-2", 1, 1))
		admission, ok := errors.AsType[*CostAdmissionError](err)
		if !ok || admission.Reason != cost.DenialIntegrity {
			t.Fatalf("post-failure admission = %v", err)
		}
	})
}

func TestCloneCoordinatorSharesCostManager(t *testing.T) {
	manager, _ := newTestCostManager(t, "2", "")
	parent := &Coordinator{taskTracker: NewTaskTracker(), costManager: manager, session: &TeamSession{Config: agent.TeamConfig{Name: "cost-clone"}}}
	clone := cloneCoordinator(parent, parent.session)
	if clone.costManager == nil || clone.costManager != parent.costManager {
		t.Fatal("extra-model coordinator did not share root cost manager")
	}
}

func TestCostEventPayloadMustMatchEnvelope(t *testing.T) {
	manager, _ := newTestCostManager(t, "2", "")
	_, journal := newCostEventStore(t)
	reservation, _, err := manager.buildReservation(costReservationRequest("provider-1", 1, 1))
	if err != nil {
		t.Fatal(err)
	}
	payload, err := cost.EncodeEvent(cost.Event{Kind: cost.EventReservationCommitted, Reservation: &reservation})
	if err != nil {
		t.Fatal(err)
	}
	_, err = journal.Append(t.Context(), RunEvent{
		RunID: "different-run", TaskID: reservation.TaskID, Attempt: reservation.OccurrenceAttempt,
		Actor: "runtime", Type: string(EventCostReservationCommitted), Payload: payload,
	})
	if err == nil {
		t.Fatal("event store accepted a cost payload bound to a different run")
	}
}

func eventTypes(events []RunEvent) []string {
	result := make([]string, len(events))
	for index, event := range events {
		result[index] = fmt.Sprintf("%d:%s", index, event.Type)
	}
	return result
}
