package team

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/modelprofile"
)

func TestModelProfileTelemetryIsReportedAndDurableWithoutSecrets(t *testing.T) {
	workspace := t.TempDir()
	store, err := NewEventStore(workspace, "run-profile", "session-profile")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	var reported StatusEvent
	c := &Coordinator{eventStore: store, executionRunID: "run-profile", reportStatus: func(event StatusEvent) { reported = event }}
	projection := modelprofile.TelemetryProjection{SchemaVersion: 1, ModelID: "qwen3:8b", Provider: "ollama", Effective: modelprofile.TelemetryValue[int]{Value: 32_768, Source: modelprofile.SourceProviderRuntime}}
	c.reportModelProfileResolved(projection)
	if reported.Type != string(EventModelProfileResolved) || reported.ModelProfile == nil {
		t.Fatalf("reported status = %#v", reported)
	}
	events, err := store.ReadEvents()
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Type != string(EventModelProfileResolved) {
		t.Fatalf("durable events = %#v", events)
	}
	encoded, err := json.Marshal(events[0].Payload)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"http://", "Bearer", "api-key", "provider-secret"} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("durable telemetry contains %q: %s", forbidden, encoded)
		}
	}
	profiles, err := LoadModelProfileTelemetry(workspace, "run-profile")
	if err != nil {
		t.Fatal(err)
	}
	if len(profiles) != 1 || profiles[0].Effective.Value != 32_768 {
		t.Fatalf("loaded profiles = %#v", profiles)
	}
}

func TestProviderBoundInvocationEmitsModelProfileTelemetry(t *testing.T) {
	workspace := t.TempDir()
	store, err := NewEventStore(workspace, "run-bound-profile", "session-bound-profile")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	modelID := "qwen3:8b"
	GlobalModelSpecRegistry().RegisterSpec(ModelContextSpec{ModelID: modelID, ContextWindow: 8_192, MaxOutputTokens: 512})
	manager, err := agent.NewProviderManager("http://127.0.0.1:11434/v1", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	var reported StatusEvent
	c := &Coordinator{
		modelProfileRuntime: NewModelProfileRuntime(manager, true),
		eventStore:          store,
		executionRunID:      "run-bound-profile",
		reportStatus:        func(event StatusEvent) { reported = event },
	}
	ctx, bound, err := c.resolveProviderBoundInvocationContext(context.Background(), modelID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if ctx == nil || !bound.AdmissionContext.IsBound() || reported.Type != string(EventModelProfileResolved) {
		t.Fatalf("bound context/status = %#v / %#v", bound, reported)
	}
	events, err := store.ReadEvents()
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Type != string(EventModelProfileResolved) {
		t.Fatalf("events = %#v", events)
	}
}
