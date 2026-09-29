package team

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"charm.land/fantasy"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/cost"
	"github.com/kjelly/hufu/internal/modelprofile"
)

type costAdmissionLanguageModel struct {
	calls int
	usage fantasy.Usage
}

func (m *costAdmissionLanguageModel) Generate(context.Context, fantasy.Call) (*fantasy.Response, error) {
	m.calls++
	return &fantasy.Response{Usage: m.usage}, nil
}

func (*costAdmissionLanguageModel) Stream(context.Context, fantasy.Call) (fantasy.StreamResponse, error) {
	return nil, nil
}

func (*costAdmissionLanguageModel) GenerateObject(context.Context, fantasy.ObjectCall) (*fantasy.ObjectResponse, error) {
	return &fantasy.ObjectResponse{}, nil
}

func (*costAdmissionLanguageModel) StreamObject(context.Context, fantasy.ObjectCall) (fantasy.ObjectStreamResponse, error) {
	return nil, nil
}

func (*costAdmissionLanguageModel) Provider() string { return "test" }

func (*costAdmissionLanguageModel) Model() string { return "gpt" }

func TestProviderAdmissionBoundZeroContextDoesNotUseGlobalRegistry(t *testing.T) {
	modelID := "bound-zero-admission-model"
	preserveRegisteredModelSpec(t, GlobalModelSpecRegistry(), modelID)
	GlobalModelSpecRegistry().RegisterSpec(ModelContextSpec{
		ModelID: modelID, ContextWindow: 32_768, MaxOutputTokens: 1_024,
	})

	request := agent.ProviderRequest{
		ModelID: modelID,
		AdmissionContext: agent.ProviderAdmissionContext{
			ModelID:          modelID,
			ProviderIdentity: "remote",
			ProviderBaseURL:  "https://provider.example/v1",
			Bound:            true,
		},
	}
	err := (providerRequestAdmission{}).AdmitProviderRequest(t.Context(), request)
	var metadataErr *ContextWindowMetadataUnavailableError
	if !errors.As(err, &metadataErr) {
		t.Fatalf("admission error = %v, want bound metadata-unavailable error", err)
	}
}

type boundZeroCountingCounter struct {
	counts int
}

func (c *boundZeroCountingCounter) CountText(context.Context, string, string) (int, error) {
	c.counts++
	return 0, nil
}

func (c *boundZeroCountingCounter) CountMessages(context.Context, string, []fantasy.Message) (int, error) {
	c.counts++
	return 0, nil
}

func (c *boundZeroCountingCounter) CountTools(context.Context, string, []fantasy.AgentTool) (int, error) {
	c.counts++
	return 0, nil
}

func TestContextWindowManagerBoundZeroFailsBeforeRegistryAndCounting(t *testing.T) {
	modelID := "bound-zero-context-window-model"
	GlobalModelSpecRegistry().RegisterSpec(ModelContextSpec{
		ModelID: modelID, ContextWindow: 32_768, MaxOutputTokens: 1_024,
	})
	counter := &boundZeroCountingCounter{}
	manager := NewContextWindowManager(counter, func(context.Context, []fantasy.Message) ([]fantasy.Message, error) {
		t.Fatal("bound-zero admission unexpectedly compacted")
		return nil, nil
	})
	admission, err := manager.Admit(t.Context(), ContextWindowRequest{
		ModelID:  modelID,
		Messages: []fantasy.Message{fantasy.NewUserMessage("request")},
		AdmissionContext: agent.ProviderAdmissionContext{
			ModelID:          modelID,
			ProviderIdentity: "remote",
			ProviderBaseURL:  "https://provider.example/v1",
			Bound:            true,
		},
	})
	if metadataErr, ok := errors.AsType[*ContextWindowMetadataUnavailableError](err); !ok || metadataErr == nil {
		t.Fatalf("manager error = %v, want metadata-unavailable", err)
	}
	if admission.Decision != ContextWindowCannotFit || admission.RejectionReason != contextWindowReasonMetadataUnavailable {
		t.Fatalf("admission = %#v, want rejected metadata-unavailable", admission)
	}
	if counter.counts != 0 {
		t.Fatalf("token counter calls = %d, want zero", counter.counts)
	}
}

func TestPrepareStepBoundZeroDoesNotFallBackToGlobalCapacity(t *testing.T) {
	modelID := "bound-zero-prepare-step-model"
	GlobalModelSpecRegistry().RegisterSpec(ModelContextSpec{
		ModelID: modelID, ContextWindow: 32_768, MaxOutputTokens: 1_024,
	})
	coordinator := &Coordinator{session: &TeamSession{Config: agent.TeamConfig{Name: "bound-zero"}}}
	prepare := coordinator.prepareAgentModelRequest(agent.AgentConfig{
		Def:        &agent.AgentDef{Name: "worker", Generation: agent.GenerationParams{Model: modelID}},
		TeamConfig: &agent.TeamConfig{Name: "bound-zero"},
		AdmissionContext: agent.ProviderAdmissionContext{
			ModelID:          modelID,
			ProviderIdentity: "remote",
			ProviderBaseURL:  "https://provider.example/v1",
			Bound:            true,
		},
	}, nil)
	_, _, err := prepare(t.Context(), fantasy.PrepareStepFunctionOptions{
		Messages: []fantasy.Message{fantasy.NewUserMessage("request")},
	})
	if metadataErr, ok := errors.AsType[*ContextWindowMetadataUnavailableError](err); !ok || metadataErr == nil {
		t.Fatalf("PrepareStep error = %v, want metadata-unavailable", err)
	}
}

func TestContextWindowManagerUnboundZeroRetainsRegistryCompatibility(t *testing.T) {
	modelID := "unbound-zero-context-window-model"
	GlobalModelSpecRegistry().RegisterSpec(ModelContextSpec{
		ModelID: modelID, ContextWindow: 32_768, MaxOutputTokens: 1_024,
	})
	manager := NewContextWindowManager(&boundZeroCountingCounter{}, nil)
	if _, err := manager.Admit(t.Context(), ContextWindowRequest{
		ModelID: modelID, Messages: []fantasy.Message{fantasy.NewUserMessage("request")},
	}); err != nil {
		t.Fatalf("unbound registry-compatible admission failed: %v", err)
	}
}

func TestProviderCostReservationAndSettlementSurroundTransport(t *testing.T) {
	manager := newProviderAdmissionCostManager(t, "ollama/gpt", cost.UnknownPriceDeny)
	store, err := NewEventStore(t.TempDir(), "run-provider-cost", "session-provider-cost")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	providerManager, err := agent.NewProviderManager("http://localhost:11434/v1", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	c := &Coordinator{
		session: &TeamSession{Config: agent.TeamConfig{DefaultLLMBackend: "ollama"}, Agents: map[string]*agent.AgentDef{
			"worker": {Name: "worker", Role: "reviewer"},
		}},
		providerManager: providerManager, eventStore: store, executionRunID: "run-provider-cost", costManager: manager,
	}
	bound := providerAdmissionTestContext(t, "ollama/gpt")
	inner := &costAdmissionLanguageModel{usage: fantasy.Usage{InputTokens: 17, OutputTokens: 3, TotalTokens: 20}}
	wrapped := agent.NewAdmittedLanguageModelWithContext("ollama/gpt", inner, c.providerAdmission(), bound)
	ctx := withInvocationMetadata(t.Context(), InvocationMetadata{
		RunID: "stale-run", TaskID: "task-1", Attempt: 2, AgentName: "worker", AgentRole: "reviewer", Purpose: "task_execution",
	})
	if _, err := wrapped.Generate(ctx, fantasy.Call{Prompt: fantasy.Prompt{fantasy.NewUserMessage("review")}, MaxOutputTokens: new(int64(32))}); err != nil {
		t.Fatal(err)
	}
	if inner.calls != 1 {
		t.Fatalf("provider calls = %d, want one", inner.calls)
	}
	events, err := store.ReadEvents()
	if err != nil {
		t.Fatal(err)
	}
	wantTypes := []EventType{EventModelProfileResolved, EventCostPriceSnapshotResolved, EventCostReservationCommitted, EventCostSettled}
	if len(events) != len(wantTypes) {
		t.Fatalf("events = %#v", eventTypes(events))
	}
	for index, want := range wantTypes {
		if EventType(events[index].Type) != want {
			t.Fatalf("event %d = %q, want %q", index, events[index].Type, want)
		}
	}
	decoded, err := cost.DecodeEvent(events[2].Type, events[2].Payload)
	if err != nil {
		t.Fatal(err)
	}
	reservation := decoded.Reservation
	if reservation.RunID != "run-provider-cost" || reservation.TaskID != "task-1" || reservation.OccurrenceAttempt != 2 || reservation.Agent != "worker" || reservation.Role != "reviewer" || reservation.Purpose != cost.PurposeWorker || reservation.ReservedOutputTokens != 32 {
		t.Fatalf("reservation identity = %#v", reservation)
	}
	settled, err := cost.DecodeEvent(events[3].Type, events[3].Payload)
	if err != nil {
		t.Fatal(err)
	}
	if settled.Settlement.Outcome != cost.OutcomeSuccess || settled.Settlement.Usage == nil || settled.Settlement.Usage.TotalTokens != 20 || settled.Settlement.ProviderInvocationID != reservation.ProviderInvocationID {
		t.Fatalf("settlement = %#v", settled.Settlement)
	}
}

func TestProviderCostDenialPreventsTransport(t *testing.T) {
	manager := newProviderAdmissionCostManager(t, "ollama/other", cost.UnknownPriceDeny)
	store, err := NewEventStore(t.TempDir(), "run-provider-denied", "session-provider-denied")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	providerManager, err := agent.NewProviderManager("http://localhost:11434/v1", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	c := &Coordinator{
		session:         &TeamSession{Config: agent.TeamConfig{DefaultLLMBackend: "ollama"}},
		providerManager: providerManager, eventStore: store, executionRunID: "run-provider-denied", costManager: manager,
	}
	inner := &costAdmissionLanguageModel{}
	wrapped := agent.NewAdmittedLanguageModelWithContext("ollama/unknown", inner, c.providerAdmission(), providerAdmissionTestContext(t, "ollama/unknown"))
	ctx := withInvocationMetadata(t.Context(), InvocationMetadata{TaskID: "task-1", Attempt: 1, AgentName: "worker", AgentRole: "worker", Purpose: "task_execution"})
	_, err = wrapped.Generate(ctx, fantasy.Call{Prompt: fantasy.Prompt{fantasy.NewUserMessage("must not send")}})
	admission, ok := errors.AsType[*CostAdmissionError](err)
	if !ok || admission.Reason != cost.DenialUnknownPrice {
		t.Fatalf("provider error = %v, want unknown-price denial", err)
	}
	if inner.calls != 0 {
		t.Fatalf("provider calls = %d, want zero", inner.calls)
	}
	events, err := store.ReadEvents()
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 3 || EventType(events[0].Type) != EventModelProfileResolved || EventType(events[1].Type) != EventCostPriceSnapshotResolved || EventType(events[2].Type) != EventCostBudgetDenied {
		t.Fatalf("denial event order = %#v", eventTypes(events))
	}
}

func TestProviderSettlementFailurePreservesResultAndLatchesAdmission(t *testing.T) {
	manager := newProviderAdmissionCostManager(t, "ollama/gpt", cost.UnknownPriceDeny)
	store, err := NewEventStore(t.TempDir(), "run-settlement-failure", "session-settlement-failure")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	providerManager, err := agent.NewProviderManager("http://localhost:11434/v1", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	c := &Coordinator{
		session:         &TeamSession{Config: agent.TeamConfig{DefaultLLMBackend: "ollama"}},
		providerManager: providerManager, eventStore: store, executionRunID: "run-settlement-failure", costManager: manager,
	}
	c.SetEventJournal(costFailingJournal{EventJournal: eventStoreJournal{store: store}, failType: EventCostSettled, err: errors.New("settlement sync failed")})
	inner := &costAdmissionLanguageModel{usage: fantasy.Usage{InputTokens: 2, OutputTokens: 1, TotalTokens: 3}}
	wrapped := agent.NewAdmittedLanguageModelWithContext("ollama/gpt", inner, c.providerAdmission(), providerAdmissionTestContext(t, "ollama/gpt"))
	ctx := withInvocationMetadata(t.Context(), InvocationMetadata{TaskID: "task-1", Attempt: 1, AgentName: "worker", AgentRole: "worker", Purpose: "task_execution"})
	response, err := wrapped.Generate(ctx, fantasy.Call{Prompt: fantasy.Prompt{fantasy.NewUserMessage("first")}})
	if err != nil || response == nil {
		t.Fatalf("provider result = %#v, %v; settlement failure replaced success", response, err)
	}
	if manager.IntegrityError() == nil {
		t.Fatal("settlement failure did not latch cost integrity")
	}
	_, err = wrapped.Generate(ctx, fantasy.Call{Prompt: fantasy.Prompt{fantasy.NewUserMessage("second")}})
	admission, ok := errors.AsType[*CostAdmissionError](err)
	if !ok || admission.Reason != cost.DenialIntegrity {
		t.Fatalf("next provider error = %v, want integrity denial", err)
	}
	if inner.calls != 1 {
		t.Fatalf("provider calls = %d, want only the first transport", inner.calls)
	}
}

func TestProviderCostIdentityClassifiesRuntimePurposes(t *testing.T) {
	c := &Coordinator{executionRunID: "run-purpose", session: &TeamSession{Agents: map[string]*agent.AgentDef{
		"worker": {Name: "worker", Role: "reviewer"},
	}}}
	tests := []struct {
		name        string
		ctx         context.Context
		wantPurpose cost.Purpose
		wantAgent   string
		wantRole    string
	}{
		{name: "worker", ctx: withInvocationMetadata(t.Context(), InvocationMetadata{TaskID: "task-1", Attempt: 1, AgentName: "worker", AgentRole: "reviewer", Purpose: "task_execution"}), wantPurpose: cost.PurposeWorker, wantAgent: "worker", wantRole: "reviewer"},
		{name: "direct", ctx: withProviderCostContext(t.Context(), providerCostContext{Agent: "worker", Role: "reviewer", Purpose: cost.PurposeDirectAgent}), wantPurpose: cost.PurposeDirectAgent, wantAgent: "worker", wantRole: "reviewer"},
		{name: "judge", ctx: withProviderCostContext(t.Context(), providerCostContext{Agent: "judge", Role: "auxiliary", Purpose: providerCostPurpose("judge")}), wantPurpose: cost.PurposeJudge, wantAgent: "judge", wantRole: "auxiliary"},
		{name: "repair overrides worker", ctx: context.WithValue(withInvocationMetadata(t.Context(), InvocationMetadata{TaskID: "task-1", Attempt: 3, AgentName: "worker", AgentRole: "reviewer"}), protocolRepairExecutionKey{}, true), wantPurpose: cost.PurposeRepair, wantAgent: "worker", wantRole: "reviewer"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			identity, err := c.providerCostIdentity(test.ctx, "pinv-test")
			if err != nil {
				t.Fatal(err)
			}
			if identity.Purpose != test.wantPurpose || identity.Agent != test.wantAgent || identity.Role != test.wantRole || identity.RunID != "run-purpose" {
				t.Fatalf("identity = %#v", identity)
			}
		})
	}
}

func newProviderAdmissionCostManager(t *testing.T, target string, unknownPolicy cost.UnknownPricePolicy) *CostManager {
	t.Helper()
	catalog, err := cost.NewCatalog(map[string]cost.PriceConfig{
		target: {BillingMode: cost.BillingMetered, InputUSDPerMillion: "1", OutputUSDPerMillion: "2"},
	})
	if err != nil {
		t.Fatal(err)
	}
	policy, err := cost.ResolveRunPolicy(&cost.PolicyConfig{UnknownPricePolicy: unknownPolicy})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := cost.NewPolicySnapshot(policy, catalog.Snapshots())
	if err != nil {
		t.Fatal(err)
	}
	return newCostManager(snapshot)
}

func providerAdmissionTestContext(t *testing.T, modelID string) agent.ProviderAdmissionContext {
	t.Helper()
	projection := modelprofile.TelemetryProjection{SchemaVersion: 1, ModelID: modelID, Provider: "ollama"}
	encoded, err := json.Marshal(projection)
	if err != nil {
		t.Fatal(err)
	}
	return agent.ProviderAdmissionContext{
		ModelID: modelID, ProviderIdentity: "ollama", ProviderBaseURL: "http://localhost:11434/v1", Bound: true,
		ContextWindow: 8_192, MaxOutputTokens: 64, SafetyMarginTokens: 32, ProfileTelemetryJSON: string(encoded),
	}
}
