package team

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
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

func TestProviderBoundInvocationFailsClosedOnProfileTelemetryAppendFailure(t *testing.T) {
	const modelID = "profile-telemetry-append-failure"
	var chatRequests atomic.Int32
	provider := newIPv4TestServer(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/v1/chat/completions" {
			chatRequests.Add(1)
		}
		writer.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/api/show":
			_, _ = fmt.Fprintf(writer, `{"model":%q,"parameters":"num_ctx 8192"}`, modelID)
		case "/api/ps":
			_, _ = fmt.Fprintf(writer, `{"models":[{"name":%q,"context_length":8192}]}`, modelID)
		default:
			http.NotFound(writer, request)
		}
	}))
	defer provider.Close()

	manager, err := agent.NewProviderManager(provider.URL+"/v1", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	workspace := t.TempDir()
	store, err := NewEventStore(workspace, "run-profile-append-failure", "session-profile-append-failure")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	store.syncFile = func() error { return errors.New("injected model profile append failure") }

	var statusEvents []StatusEvent
	c := &Coordinator{
		modelProfileRuntime: NewModelProfileRuntime(manager, false),
		eventStore:          store,
		executionRunID:      "run-profile-append-failure",
		reportStatus:        func(event StatusEvent) { statusEvents = append(statusEvents, event) },
	}
	_, invocation, err := c.resolveProviderBoundInvocationContext(t.Context(), modelID, nil)
	if err == nil || !strings.Contains(err.Error(), "persist model profile resolved") {
		t.Fatalf("resolver error = %v, want durable profile append failure", err)
	}
	if invocation.AdmissionContext.IsBound() {
		t.Fatalf("resolver returned usable invocation after append failure: %#v", invocation)
	}
	if chatRequests.Load() != 0 {
		t.Fatalf("provider chat requests = %d, want 0 after append failure", chatRequests.Load())
	}
	if len(statusEvents) != 0 {
		t.Fatalf("status events = %#v, want no profile status after append failure", statusEvents)
	}
}
