package team

import (
	"context"
	"errors"
	"testing"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/cost"
	"github.com/kjelly/hufu/internal/execution"
)

type externalCostTestProvider struct {
	name    string
	calls   int
	request AttemptRequest
	result  AttemptResult
	err     error
}

func (p *externalCostTestProvider) Name() string { return p.name }

func (*externalCostTestProvider) Capabilities() SubagentCapabilities {
	return SubagentCapabilities{}
}

func (p *externalCostTestProvider) RunAttempt(_ context.Context, request AttemptRequest) (AttemptResult, error) {
	p.calls++
	p.request = request
	return p.result, p.err
}

func newExternalCostManager(t *testing.T, priceConfig cost.PriceConfig, policyConfig cost.PolicyConfig) *CostManager {
	t.Helper()
	catalog, err := cost.NewCatalog(map[string]cost.PriceConfig{"codex/gpt": priceConfig})
	if err != nil {
		t.Fatal(err)
	}
	price, ok := catalog.Resolve("codex/gpt")
	if !ok {
		t.Fatal("compiled price is unavailable")
	}
	policy, err := cost.ResolveRunPolicy(&policyConfig)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := cost.NewPolicySnapshot(policy, []cost.PriceSnapshot{price})
	if err != nil {
		t.Fatal(err)
	}
	return newCostManager(snapshot)
}

func newExternalCostBackend(t *testing.T, manager *CostManager, provider *externalCostTestProvider) (*AgentExecutionBackend, *Coordinator, *EventStore) {
	t.Helper()
	store, err := NewEventStore(t.TempDir(), "run-cost", "session-cost")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	coordinator := &Coordinator{eventStore: store, executionRunID: "run-cost", costManager: manager}
	backend, err := NewAgentExecutionBackend("codex", provider, coordinatorAgentExecutionCost{c: coordinator})
	if err != nil {
		t.Fatal(err)
	}
	return backend, coordinator, store
}

func externalCostAttempt() AttemptRequest {
	return AttemptRequest{
		RunID: "run-cost", TaskID: "task-1", Attempt: 2,
		Agent:           &agent.AgentDef{Name: "coder", Role: "implementation"},
		Task:            TaskDef{ID: "task-1", Agent: "coder"},
		ExecutionTarget: execution.ExecutionTarget{Backend: "codex", Model: "gpt"},
	}
}

func readCostEvents(t *testing.T, store *EventStore) []cost.Event {
	t.Helper()
	events, err := store.ReadEvents()
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := costEventsFromRunEvents(events)
	if err != nil {
		t.Fatal(err)
	}
	return decoded
}

func TestAgentExecutionBackendRequiresCostBoundary(t *testing.T) {
	provider := &externalCostTestProvider{name: "codex"}
	if _, err := NewAgentExecutionBackend("codex", provider, nil); err == nil {
		t.Fatal("external backend accepted without cost boundary")
	}
}

func TestAgentExecutionBackendRejectsRunIdentityDriftBeforeProvider(t *testing.T) {
	manager := newExternalCostManager(t, cost.PriceConfig{
		BillingMode: cost.BillingMetered, OpaqueMaxUSDPerInvocation: "0.75",
	}, cost.PolicyConfig{MaxRunUSD: "2"})
	provider := &externalCostTestProvider{name: "codex"}
	backend, _, store := newExternalCostBackend(t, manager, provider)
	request := externalCostAttempt()
	request.RunID = "different-run"

	if _, err := backend.RunAttempt(t.Context(), request); err == nil {
		t.Fatal("RunAttempt accepted a request from another run")
	}
	if provider.calls != 0 {
		t.Fatalf("provider calls = %d after run drift", provider.calls)
	}
	if events := readCostEvents(t, store); len(events) != 0 {
		t.Fatalf("cost events after run drift = %#v", events)
	}
}

func TestAgentExecutionBackendUsesOpaqueAdmissionThenObservedUsage(t *testing.T) {
	manager := newExternalCostManager(t, cost.PriceConfig{
		BillingMode: cost.BillingMetered, InputUSDPerMillion: "1", OutputUSDPerMillion: "1",
		OpaqueMaxUSDPerInvocation: "0.75",
	}, cost.PolicyConfig{MaxRunUSD: "2"})
	provider := &externalCostTestProvider{name: "codex", result: AttemptResult{
		Output: "done", Usage: ExecutionUsage{InputTokens: 100_000, OutputTokens: 100_000, TotalTokens: 200_000},
	}}
	backend, _, store := newExternalCostBackend(t, manager, provider)

	result, err := backend.RunAttempt(t.Context(), externalCostAttempt())
	if err != nil || result.Output != "done" {
		t.Fatalf("RunAttempt result=%#v error=%v", result, err)
	}
	if provider.calls != 1 || provider.request.ModelID != "gpt" || provider.request.Provider != "codex" {
		t.Fatalf("provider calls=%d request=%#v", provider.calls, provider.request)
	}
	events := readCostEvents(t, store)
	if len(events) != 3 || events[0].Kind != cost.EventPriceSnapshotResolved || events[1].Kind != cost.EventReservationCommitted || events[2].Kind != cost.EventSettled {
		t.Fatalf("cost event order = %#v", events)
	}
	reservation, settlement := events[1].Reservation, events[2].Settlement
	if reservation == nil || reservation.ReservedMicros == nil || *reservation.ReservedMicros != 750_000 || reservation.EstimateSource != cost.EstimateAdmissionBound {
		t.Fatalf("reservation = %#v", reservation)
	}
	if reservation.Purpose != cost.PurposeExternal || reservation.Agent != "coder" || reservation.Role != "implementation" || reservation.OccurrenceAttempt != 2 {
		t.Fatalf("reservation identity = %#v", reservation.InvocationIdentity)
	}
	if settlement == nil || settlement.FinalMicros == nil || *settlement.FinalMicros != 200_000 || settlement.EstimateSource != cost.EstimateUsage || settlement.Usage == nil {
		t.Fatalf("settlement = %#v", settlement)
	}
	if settlement.ProviderInvocationID != reservation.ProviderInvocationID {
		t.Fatalf("settlement identity %q != reservation %q", settlement.ProviderInvocationID, reservation.ProviderInvocationID)
	}
}

func TestAgentExecutionBackendKeepsOpaqueBoundWithoutCompleteUsage(t *testing.T) {
	manager := newExternalCostManager(t, cost.PriceConfig{
		BillingMode: cost.BillingMetered, InputUSDPerMillion: "1", OutputUSDPerMillion: "1",
		OpaqueMaxUSDPerInvocation: "0.75",
	}, cost.PolicyConfig{MaxRunUSD: "2"})
	provider := &externalCostTestProvider{name: "codex", result: AttemptResult{
		Usage: ExecutionUsage{InputTokens: 100, OutputTokens: 100, TotalTokens: 1},
	}}
	backend, _, store := newExternalCostBackend(t, manager, provider)

	if _, err := backend.RunAttempt(t.Context(), externalCostAttempt()); err != nil {
		t.Fatal(err)
	}
	events := readCostEvents(t, store)
	reservation, settlement := events[1].Reservation, events[2].Settlement
	if settlement.Usage != nil || settlement.EstimateSource != cost.EstimateAdmissionBound || settlement.FinalMicros == nil || *settlement.FinalMicros != *reservation.ReservedMicros {
		t.Fatalf("reservation=%#v settlement=%#v", reservation, settlement)
	}
}

func TestAgentExecutionBackendDeniesUnboundedOpaqueCostBeforeProvider(t *testing.T) {
	manager := newExternalCostManager(t, cost.PriceConfig{
		BillingMode: cost.BillingMetered, InputUSDPerMillion: "1", OutputUSDPerMillion: "1",
	}, cost.PolicyConfig{MaxRunUSD: "2"})
	provider := &externalCostTestProvider{name: "codex"}
	backend, _, store := newExternalCostBackend(t, manager, provider)

	_, err := backend.RunAttempt(t.Context(), externalCostAttempt())
	admission, ok := errors.AsType[*CostAdmissionError](err)
	if !ok || admission.Reason != cost.DenialUnboundedCost {
		t.Fatalf("RunAttempt error = %v", err)
	}
	if provider.calls != 0 {
		t.Fatalf("provider was called %d times after denied admission", provider.calls)
	}
	events := readCostEvents(t, store)
	if len(events) != 2 || events[0].Kind != cost.EventPriceSnapshotResolved || events[1].Kind != cost.EventBudgetDenied || events[1].Denied.Reason != cost.DenialUnboundedCost {
		t.Fatalf("cost events = %#v", events)
	}
}

func TestAgentExecutionBackendAllowsUnknownOpaqueCostOnlyWithoutHardBudget(t *testing.T) {
	manager := newExternalCostManager(t, cost.PriceConfig{
		BillingMode: cost.BillingMetered, InputUSDPerMillion: "1", OutputUSDPerMillion: "1",
	}, cost.PolicyConfig{UnknownPricePolicy: cost.UnknownPriceAllow})
	provider := &externalCostTestProvider{name: "codex"}
	backend, _, store := newExternalCostBackend(t, manager, provider)

	if _, err := backend.RunAttempt(t.Context(), externalCostAttempt()); err != nil {
		t.Fatal(err)
	}
	if provider.calls != 1 {
		t.Fatalf("provider calls = %d, want 1", provider.calls)
	}
	events := readCostEvents(t, store)
	reservation, settlement := events[1].Reservation, events[2].Settlement
	if reservation.ReservedMicros != nil || reservation.BillingMode != cost.BillingUnknown || reservation.EstimateSource != cost.EstimateUnknown {
		t.Fatalf("reservation = %#v", reservation)
	}
	if settlement.FinalMicros != nil || settlement.BillingMode != cost.BillingUnknown || settlement.EstimateSource != cost.EstimateUnknown {
		t.Fatalf("settlement = %#v", settlement)
	}
}

func TestAgentExecutionBackendKeepsNonMeteredBillingExplicit(t *testing.T) {
	tests := []struct {
		name   string
		mode   cost.BillingMode
		source cost.EstimateSource
	}{
		{name: "local", mode: cost.BillingLocal, source: cost.EstimateNotMetered},
		{name: "subscription", mode: cost.BillingSubscription, source: cost.EstimateSubscription},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			manager := newExternalCostManager(t, cost.PriceConfig{BillingMode: test.mode}, cost.PolicyConfig{})
			provider := &externalCostTestProvider{name: "codex", result: AttemptResult{
				Usage: ExecutionUsage{InputTokens: 10, OutputTokens: 5, TotalTokens: 15},
			}}
			backend, _, store := newExternalCostBackend(t, manager, provider)

			if _, err := backend.RunAttempt(t.Context(), externalCostAttempt()); err != nil {
				t.Fatal(err)
			}
			events := readCostEvents(t, store)
			reservation, settlement := events[1].Reservation, events[2].Settlement
			if reservation.ReservedMicros != nil || reservation.BillingMode != test.mode || reservation.EstimateSource != test.source {
				t.Fatalf("reservation = %#v", reservation)
			}
			if settlement.FinalMicros != nil || settlement.BillingMode != test.mode || settlement.EstimateSource != test.source {
				t.Fatalf("settlement = %#v", settlement)
			}
		})
	}
}

func TestAgentExecutionBackendSettlementFailurePreservesProviderResultAndLatchesIntegrity(t *testing.T) {
	manager := newExternalCostManager(t, cost.PriceConfig{
		BillingMode: cost.BillingMetered, OpaqueMaxUSDPerInvocation: "0.75",
	}, cost.PolicyConfig{MaxRunUSD: "2"})
	providerErr := errors.New("provider failed after launch")
	provider := &externalCostTestProvider{name: "codex", result: AttemptResult{Output: "provider output"}, err: providerErr}
	backend, coordinator, _ := newExternalCostBackend(t, manager, provider)
	coordinator.SetEventJournal(costFailingJournal{
		EventJournal: coordinator.EventJournal(), failType: EventCostSettled, err: errors.New("settlement sync failed"),
	})

	result, err := backend.RunAttempt(t.Context(), externalCostAttempt())
	if result.Output != "provider output" || !errors.Is(err, providerErr) {
		t.Fatalf("provider result=%#v error=%v", result, err)
	}
	if manager.IntegrityError() == nil {
		t.Fatal("settlement persistence failure did not latch cost integrity")
	}
	_, err = backend.RunAttempt(t.Context(), externalCostAttempt())
	admission, ok := errors.AsType[*CostAdmissionError](err)
	if !ok || admission.Reason != cost.DenialIntegrity {
		t.Fatalf("next admission error = %v", err)
	}
	if provider.calls != 1 {
		t.Fatalf("provider calls = %d, want no retry after settlement failure", provider.calls)
	}
}
