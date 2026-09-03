package team

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"charm.land/fantasy"
	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/config"
)

type p3FixedCounter struct{ messages int }

func TestContextWindowTelemetryBranchSnapshotIsRaceFree(t *testing.T) {
	c := &Coordinator{session: &TeamSession{Config: agent.TeamConfig{Name: "race"}}}
	request := ContextWindowRequest{ModelID: "race-model", Window: 1024}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Go(func() {
			for j := 0; j < 100; j++ {
				c.newContextWindowTelemetry(EventContextWindowAdmission, request, ContextWindowAdmission{}, "race", "", j)
			}
		})
	}
	wg.Go(func() {
		for i := 0; i < 800; i++ {
			c.compactionMu.Lock()
			c.compactionBranchID = fmt.Sprintf("branch-%d", i)
			c.compactionMu.Unlock()
		}
	})
	wg.Wait()
}

func (p3FixedCounter) CountText(context.Context, string, string) (int, error) { return 0, nil }
func (p p3FixedCounter) CountMessages(context.Context, string, []fantasy.Message) (int, error) {
	return p.messages, nil
}
func (p3FixedCounter) CountTools(context.Context, string, []fantasy.AgentTool) (int, error) {
	return 0, nil
}

func TestCompactionPolicyDefaultsAndYAML(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "team.yaml"), []byte(`name: p3
compaction:
  max-history-messages: 40
  retain-history-messages: 30
  verified-history-target-tokens: 8000
  tool-output-max-bytes: 1000
  tool-output-max-runes: 200
  tool-output-max-tokens: 100
  diagnostic-max-lines: 8
  diagnostic-max-tokens: 40
`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := parseTeamYML(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Compaction.MaxHistoryMessages != 40 || cfg.Compaction.RetainHistoryMessages != 30 || cfg.Compaction.DiagnosticMaxTokens != 40 {
		t.Fatalf("unexpected compaction policy: %#v", cfg.Compaction)
	}
	defaults := agent.DefaultCompactionPolicy()
	if defaults.MaxHistoryMessages != 100 || defaults.RetainHistoryMessages != 80 || defaults.ToolOutputMaxTokens != 1500 {
		t.Fatalf("unexpected defaults: %#v", defaults)
	}
}

func TestCompactionPolicyRejectsUnsafeYAMLZeroAndOrdering(t *testing.T) {
	for name, value := range map[string]string{
		"zero":     "max-history-messages: 0",
		"ordering": "max-history-messages: 10\n  retain-history-messages: 10",
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			content := "name: p3\ncompaction:\n  " + value + "\n"
			if err := os.WriteFile(filepath.Join(dir, "team.yaml"), []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := parseTeamYML(dir, nil); err == nil {
				t.Fatal("unsafe compaction policy was accepted")
			}
		})
	}
}

func TestCompactionPolicyRejectsImpossibleToolOutputCapsYAML(t *testing.T) {
	for _, key := range []string{"tool-output-max-bytes", "tool-output-max-runes", "tool-output-max-tokens"} {
		t.Run(key, func(t *testing.T) {
			dir := t.TempDir()
			content := "name: p3\ncompaction:\n  " + key + ": 1\n"
			if err := os.WriteFile(filepath.Join(dir, "team.yaml"), []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := parseTeamYML(dir, nil); err == nil {
				t.Fatal("impossible tool-output cap was accepted")
			}
		})
	}
}

func TestContextWindowAdmissionTelemetryIsDurableBeforeProviderBoundary(t *testing.T) {
	modelID := "p3-admission-model"
	GlobalModelSpecRegistry().RegisterSpec(ModelContextSpec{ModelID: modelID, ContextWindow: 100000, MaxOutputTokens: 4096, SafetyMarginTokens: 2672})
	workspace := t.TempDir()
	store, err := NewEventStore(workspace, "run-p3", "session-p3")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	store.SetBranchID("branch-p3")
	c := &Coordinator{session: &TeamSession{Workspace: workspace, Config: agent.TeamConfig{Name: "team-p3"}}, eventStore: store, executionRunID: "run-p3", compactionBranchID: "branch-p3", reportStatus: func(StatusEvent) {}}
	manager := NewContextWindowManager(p3FixedCounter{messages: 98437}, nil)
	admission, err := c.admitCoordinatorContext(context.Background(), manager, ContextWindowRequest{ModelID: modelID, Messages: []fantasy.Message{fantasy.NewUserMessage("oversized")}}, "final", "task-p3", 1)
	if err != nil {
		t.Fatal(err)
	}
	if admission.Decision != ContextWindowCannotFit || admission.RequestTokens != 98437 || admission.Budget.Available != 93232 {
		t.Fatalf("unexpected admission: %#v", admission)
	}
	events, err := store.ReadEvents()
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Type != string(EventContextWindowAdmission) {
		t.Fatalf("events = %#v, want one admission event", events)
	}
	var telemetry ContextWindowTelemetryEvent
	if err := json.Unmarshal(events[0].Payload, &telemetry); err != nil {
		t.Fatal(err)
	}
	if telemetry.Decision != string(ContextWindowCannotFit) || telemetry.RunID != "run-p3" || telemetry.BranchID != "branch-p3" || telemetry.RequestedTokens != 98437 {
		t.Fatalf("unexpected telemetry: %#v", telemetry)
	}
	if strings.Contains(string(events[0].Payload), "oversized") {
		t.Fatal("telemetry persisted request content")
	}
	if got := c.Metrics().ContextWindowTelemetry.CannotFit; got != 1 {
		t.Fatalf("CannotFit summary = %d, want 1", got)
	}
	restarted := &Coordinator{}
	restarted.hydrateContextWindowTelemetry(events)
	if got := restarted.Metrics().ContextWindowTelemetry.CannotFit; got != 1 {
		t.Fatalf("restarted CannotFit summary = %d, want 1", got)
	}
}

func TestCoordinatorRunnerBoundZeroRejectsBeforeRegistryBudgetProjection(t *testing.T) {
	modelID := "bound-zero-runner-model"
	GlobalModelSpecRegistry().RegisterSpec(ModelContextSpec{
		ModelID: modelID, ContextWindow: 32_768, MaxOutputTokens: 1_024, SafetyMarginTokens: 256,
	})
	workspace := t.TempDir()
	store, err := NewEventStore(workspace, "bound-zero-run", "bound-zero-session")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	compiler := &mockContextCompiler{}
	c := &Coordinator{
		session:        &TeamSession{Workspace: workspace, Config: agent.TeamConfig{Name: "bound-zero-runner"}},
		eventStore:     store,
		taskTracker:    NewTaskTracker(),
		executionRunID: "bound-zero-run",
		reportStatus:   func(StatusEvent) {},
	}
	c.SetContextCompiler(compiler)
	ctx := withContextWindowRequestDescriptor(t.Context(), contextWindowRequestDescriptor{
		ModelID: modelID,
		AdmissionContext: agent.ProviderAdmissionContext{
			ModelID:          modelID,
			ProviderIdentity: "remote",
			ProviderBaseURL:  "https://provider.example/v1",
			Bound:            true,
		},
	})
	ctx = context.WithValue(ctx, modelKey{}, modelID)
	calls := 0
	_, _, err = c.runAgentWithStatusAndHistory(ctx, contextWindowCountingAgent{
		model: contextWindowTestModel{modelID: modelID},
		calls: &calls,
	}, "worker", "request", nil, &taskTiming{})
	if metadataErr, ok := errors.AsType[*ContextWindowMetadataUnavailableError](err); !ok || metadataErr == nil {
		t.Fatalf("runner error = %v, want metadata-unavailable", err)
	}
	if compiler.calcCalled {
		t.Fatal("runner computed a budget before rejecting bound-zero capacity")
	}
	if calls != 0 {
		t.Fatalf("provider calls = %d, want zero", calls)
	}

	events, err := store.ReadEvents()
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Type != string(EventContextWindowAdmission) {
		t.Fatalf("events = %#v, want one admission rejection", events)
	}
	var telemetry ContextWindowTelemetryEvent
	if err := json.Unmarshal(events[0].Payload, &telemetry); err != nil {
		t.Fatal(err)
	}
	if telemetry.Decision != string(ContextWindowCannotFit) || telemetry.RejectionReason != contextWindowReasonMetadataUnavailable {
		t.Fatalf("telemetry = %#v, want metadata-unavailable rejection", telemetry)
	}
	if telemetry.RequestedTokens != 0 || telemetry.AvailableTokens != 0 || telemetry.ReservedTokens != 0 || telemetry.SafetyTokens != 0 || telemetry.WindowTokens != 0 {
		t.Fatalf("telemetry = %#v, want zero/unknown admission budget", telemetry)
	}
	if got := c.Metrics().ContextWindowTelemetry; got.Admitted != 0 || got.CannotFit != 1 {
		t.Fatalf("telemetry summary = %#v, want one rejection and no admission", got)
	}
}

func TestContextWindowTelemetryAttemptIdentitySpansPrepareStepAndChangesOnRetry(t *testing.T) {
	modelID := "p3-telemetry-identity-model"
	GlobalModelSpecRegistry().RegisterSpec(ModelContextSpec{ModelID: modelID, ContextWindow: 100000, MaxOutputTokens: 128, SafetyMarginTokens: 32})
	workspace := t.TempDir()
	store, err := NewEventStore(workspace, "run-identity", "session-identity")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	store.SetBranchID("branch-identity")
	c := &Coordinator{
		session:            &TeamSession{Workspace: workspace, Config: agent.TeamConfig{Name: "identity-team"}},
		eventStore:         store,
		executionRunID:     "run-identity",
		compactionBranchID: "branch-identity",
		reportStatus:       func(StatusEvent) {},
	}
	manager := NewContextWindowManager(p3FixedCounter{messages: 10}, nil)
	request := ContextWindowRequest{ModelID: modelID, Messages: []fantasy.Message{fantasy.NewUserMessage("stable")}}
	if _, err := c.admitCoordinatorContext(context.Background(), manager, request, "initial", "task-identity", 1); err != nil {
		t.Fatal(err)
	}
	if _, err := c.admitCoordinatorContext(context.Background(), manager, request, "final", "task-identity", 1); err != nil {
		t.Fatal(err)
	}
	if _, err := c.admitCoordinatorContext(context.Background(), manager, request, "initial", "task-identity", 2); err != nil {
		t.Fatal(err)
	}

	events, err := store.ReadEvents()
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 3 {
		t.Fatalf("events = %d, want three admission events", len(events))
	}
	telemetry := make([]ContextWindowTelemetryEvent, len(events))
	for i, event := range events {
		if event.Type != string(EventContextWindowAdmission) {
			t.Fatalf("event %d type = %q, want admission", i, event.Type)
		}
		if err := json.Unmarshal(event.Payload, &telemetry[i]); err != nil {
			t.Fatal(err)
		}
	}
	if telemetry[0].CoordinatorAttemptID == "" || telemetry[0].CoordinatorAttemptID != telemetry[1].CoordinatorAttemptID {
		t.Fatalf("initial/final coordinator attempt IDs = %q/%q, want equal", telemetry[0].CoordinatorAttemptID, telemetry[1].CoordinatorAttemptID)
	}
	if telemetry[0].TelemetryID == telemetry[1].TelemetryID {
		t.Fatalf("initial/final telemetry IDs = %q, want event-specific IDs", telemetry[0].TelemetryID)
	}
	if telemetry[0].CoordinatorAttemptID == telemetry[2].CoordinatorAttemptID {
		t.Fatalf("retry coordinator attempt ID = %q, want a new identity", telemetry[2].CoordinatorAttemptID)
	}
	if telemetry[0].StreamAttemptID == telemetry[2].StreamAttemptID {
		t.Fatalf("retry stream attempt ID = %q, want a new identity", telemetry[2].StreamAttemptID)
	}
}

func TestRunOrchestratorTelemetryUsesExecutionTurnIdentityAcrossDownshift(t *testing.T) {
	strongID := "production-telemetry-strong"
	weakID := "production-telemetry-weak"
	GlobalModelSpecRegistry().RegisterSpec(ModelContextSpec{
		ModelID: strongID, ContextWindow: 256, MaxOutputTokens: 32, SafetyMarginTokens: 32,
	})
	GlobalModelSpecRegistry().RegisterSpec(ModelContextSpec{
		ModelID: weakID, ContextWindow: 32_768, MaxOutputTokens: 32, SafetyMarginTokens: 32,
	})

	providerCalls := 0
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		providerCalls++
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "data: {\"id\":\"production-telemetry\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"weak\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"accepted\"},\"finish_reason\":null}]}\n\n")
		_, _ = fmt.Fprint(w, "data: {\"id\":\"production-telemetry\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"weak\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	})
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Skipf("network listener unavailable in this environment: %v", err)
	}
	provider := httptest.NewUnstartedServer(handler)
	provider.Listener = listener
	provider.Start()
	defer provider.Close()

	providerManager, err := agent.NewProviderManager(provider.URL+"/v1", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	workspace := t.TempDir()
	store, err := NewEventStore(workspace, "run-production-telemetry", "session-production-telemetry")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	store.SetBranchID("branch-production-telemetry")

	history := make([]fantasy.Message, 0, 20)
	for i := 0; i < 20; i++ {
		history = append(history, fantasy.NewUserMessage(fmt.Sprintf("historical message %d %s", i, strings.Repeat("evidence ", 100))))
	}
	c := &Coordinator{
		providerManager:     providerManager,
		modelList:           []config.ModelEntry{{ID: weakID}, {ID: strongID}},
		session:             &TeamSession{Workspace: workspace, Config: agent.TeamConfig{Name: "production-telemetry", Generation: agent.GenerationParams{Model: strongID}, Timeout: 10}},
		eventStore:          store,
		executionRunID:      "run-production-telemetry",
		compactionBranchID:  "branch-production-telemetry",
		reportStatus:        func(StatusEvent) {},
		conversationHistory: history,
	}

	orchDef := &agent.AgentDef{Name: "coordinator", Generation: agent.GenerationParams{Model: strongID}, System: "system", MaxSteps: 1}
	if result, _, err := c.runOrchestrator(context.Background(), orchDef, "incoming"); err != nil {
		t.Fatalf("first coordinator stream error = %v", err)
	} else if result != "accepted" {
		t.Fatalf("first coordinator result = %q, want accepted", result)
	}
	firstEvents, err := store.ReadEvents()
	if err != nil {
		t.Fatal(err)
	}
	firstTelemetry := contextWindowTelemetryEvents(firstEvents)
	if len(firstTelemetry) == 0 {
		t.Fatal("first coordinator stream emitted no context-window telemetry")
	}
	if providerCalls != 1 {
		t.Fatalf("provider calls after first stream = %d, want 1", providerCalls)
	}

	if result, _, err := c.runOrchestrator(context.Background(), orchDef, "incoming"); err != nil {
		t.Fatalf("second coordinator stream error = %v", err)
	} else if result != "accepted" {
		t.Fatalf("second coordinator result = %q, want accepted", result)
	}
	allEvents, err := store.ReadEvents()
	if err != nil {
		t.Fatal(err)
	}
	allTelemetry := contextWindowTelemetryEvents(allEvents)
	if len(allTelemetry) <= len(firstTelemetry) {
		t.Fatalf("telemetry events after second stream = %d, want more than first stream's %d", len(allTelemetry), len(firstTelemetry))
	}
	if providerCalls != 2 {
		t.Fatalf("provider calls after second stream = %d, want 2", providerCalls)
	}

	secondTelemetry := allTelemetry[len(firstTelemetry):]
	firstAttempt := firstTelemetry[0].Attempt
	secondAttempt := secondTelemetry[0].Attempt
	if firstAttempt <= 0 || secondAttempt <= 0 {
		t.Fatalf("execution attempts = %d/%d, want positive", firstAttempt, secondAttempt)
	}
	if firstAttempt == secondAttempt {
		t.Fatalf("execution attempts = %d/%d, want distinct coordinator turns", firstAttempt, secondAttempt)
	}
	if firstTelemetry[0].CoordinatorAttemptID == secondTelemetry[0].CoordinatorAttemptID {
		t.Fatalf("coordinator attempt IDs = %q/%q, want distinct coordinator turns", firstTelemetry[0].CoordinatorAttemptID, secondTelemetry[0].CoordinatorAttemptID)
	}
	if firstTelemetry[0].StreamAttemptID == secondTelemetry[0].StreamAttemptID {
		t.Fatalf("stream attempt IDs = %q/%q, want distinct coordinator turns", firstTelemetry[0].StreamAttemptID, secondTelemetry[0].StreamAttemptID)
	}
	if firstTelemetry[0].CoordinatorAttemptID == "" || firstTelemetry[0].CoordinatorAttemptID != firstTelemetry[1].CoordinatorAttemptID {
		t.Fatalf("first stream initial/final coordinator IDs = %q/%q, want equal", firstTelemetry[0].CoordinatorAttemptID, firstTelemetry[1].CoordinatorAttemptID)
	}
	if firstTelemetry[0].StreamAttemptID != firstTelemetry[1].StreamAttemptID {
		t.Fatalf("first stream initial/final stream IDs = %q/%q, want equal", firstTelemetry[0].StreamAttemptID, firstTelemetry[1].StreamAttemptID)
	}
	for i, event := range firstTelemetry {
		if event.Attempt != firstAttempt {
			t.Fatalf("first stream telemetry %d attempt = %d, want %d", i, event.Attempt, firstAttempt)
		}
	}
	for i, event := range secondTelemetry {
		if event.Attempt != secondAttempt {
			t.Fatalf("second stream telemetry %d attempt = %d, want %d", i, event.Attempt, secondAttempt)
		}
	}
	if !hasTelemetryPhase(firstTelemetry, "downshift_candidate") || !hasTelemetryPhase(firstTelemetry, "downshift") {
		t.Fatalf("first stream telemetry phases = %#v, want downshift candidate and downshift", telemetryPhases(firstTelemetry))
	}
}

func contextWindowTelemetryEvents(events []RunEvent) []ContextWindowTelemetryEvent {
	telemetry := make([]ContextWindowTelemetryEvent, 0)
	for _, event := range events {
		switch EventType(event.Type) {
		case EventContextWindowAdmission, EventContextWindowDownshift:
			var decoded ContextWindowTelemetryEvent
			if json.Unmarshal(event.Payload, &decoded) == nil {
				telemetry = append(telemetry, decoded)
			}
		}
	}
	return telemetry
}

func hasTelemetryPhase(events []ContextWindowTelemetryEvent, phase string) bool {
	for _, event := range events {
		if event.Phase == phase {
			return true
		}
	}
	return false
}

func telemetryPhases(events []ContextWindowTelemetryEvent) []string {
	phases := make([]string, 0, len(events))
	for _, event := range events {
		phases = append(phases, event.Phase)
	}
	return phases
}

func TestInitEventStoreHydratesActiveBranchTelemetryWithoutPendingRecovery(t *testing.T) {
	workspace := t.TempDir()
	store, err := NewEventStore(workspace, "run-telemetry-restart", "session-telemetry-restart")
	if err != nil {
		t.Fatal(err)
	}
	store.SetBranchID("main")
	payload, err := json.Marshal(ContextWindowTelemetryEvent{
		SchemaVersion:   contextWindowTelemetrySchemaVersion,
		TelemetryID:     "telemetry-admission-1",
		RunID:           "run-telemetry-restart",
		TeamID:          "restart-team",
		BranchID:        "main",
		Model:           "restart-model",
		RequestedTokens: 80,
		AvailableTokens: 120,
		Decision:        "admitted",
		CompactionCount: 2,
		PolicyDigest:    "policy-digest",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.AppendPersisted(RunEvent{Type: string(EventContextWindowAdmission), Actor: "coordinator", Payload: payload}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	restarted := &Coordinator{
		session:     &TeamSession{Workspace: workspace, Config: agent.TeamConfig{Name: "restart-team"}},
		sessionData: NewSession(),
	}
	restarted.initEventStore()
	if restarted.EventStore() == nil {
		t.Fatal("restart did not initialize event store")
	}
	defer restarted.EventStore().Close()

	got := restarted.Metrics().ContextWindowTelemetry
	if got.AdmissionEvents != 1 || got.Admitted != 1 || got.CannotFit != 0 || got.LastRequestedTokens != 80 || got.LastCompactionCount != 2 {
		t.Fatalf("hydrated telemetry = %#v, want exactly one persisted admission", got)
	}
}
