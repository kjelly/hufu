package team

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"charm.land/fantasy"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/modelprofile"
	"github.com/kjelly/hufu/internal/providerintrospection"
	"github.com/kjelly/hufu/internal/sidecar"
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
	const modelID = "qwen3:8b"
	provider := newIPv4TestServer(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/v1/chat/completions" {
			http.NotFound(writer, request)
			return
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(writer, `{"id":"profile-invocation","object":"chat.completion","created":1,"model":"qwen3:8b","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
	}))
	defer provider.Close()

	workspace := t.TempDir()
	store, err := NewEventStore(workspace, "run-bound-profile", "session-bound-profile")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	GlobalModelSpecRegistry().RegisterSpec(ModelContextSpec{ModelID: modelID, ContextWindow: 8_192, MaxOutputTokens: 512})
	manager, err := agent.NewProviderManager(provider.URL+"/v1", "", nil)
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
	if ctx == nil || !bound.AdmissionContext.IsBound() || bound.AdmissionContext.ProfileTelemetryJSON == "" {
		t.Fatalf("bound context = %#v", bound)
	}
	events, err := store.ReadEvents()
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 0 {
		t.Fatalf("resolution events = %#v, want no event before provider entry", events)
	}

	languageModel, err := manager.GetProvider(modelID).LanguageModel(ctx, modelID)
	if err != nil {
		t.Fatal(err)
	}
	wrapped := agent.NewAdmittedLanguageModelWithContext(modelID, languageModel, c.providerAdmission(), bound.AdmissionContext)
	if _, err := wrapped.Generate(ctx, fantasy.Call{Prompt: fantasy.Prompt{fantasy.NewUserMessage("invoke")}}); err != nil {
		t.Fatalf("provider-bound invocation: %v", err)
	}
	if reported.Type != string(EventModelProfileResolved) || reported.ModelProfile == nil || reported.ModelProfile.InvocationID == "" {
		t.Fatalf("post-invocation status = %#v", reported)
	}
	events, err = store.ReadEvents()
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Type != string(EventModelProfileResolved) {
		t.Fatalf("events = %#v", events)
	}
	var persisted modelprofile.TelemetryProjection
	if err := json.Unmarshal(events[0].Payload, &persisted); err != nil {
		t.Fatal(err)
	}
	if persisted.InvocationID != reported.ModelProfile.InvocationID {
		t.Fatalf("persisted invocation ID = %q, status ID = %q", persisted.InvocationID, reported.ModelProfile.InvocationID)
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
	ctx, invocation, err := c.resolveProviderBoundInvocationContext(t.Context(), modelID, nil)
	if err != nil {
		t.Fatalf("resolver error = %v, want provider-bound snapshot", err)
	}
	if !invocation.AdmissionContext.IsBound() || invocation.AdmissionContext.ProfileTelemetryJSON == "" {
		t.Fatalf("resolver returned unusable invocation: %#v", invocation)
	}
	languageModel, err := manager.GetProvider(modelID).LanguageModel(ctx, modelID)
	if err != nil {
		t.Fatal(err)
	}
	wrapped := agent.NewAdmittedLanguageModelWithContext(modelID, languageModel, c.providerAdmission(), invocation.AdmissionContext)
	if _, err := wrapped.Generate(ctx, fantasy.Call{Prompt: fantasy.Prompt{fantasy.NewUserMessage("must not reach provider")}}); err == nil || !strings.Contains(err.Error(), "persist model profile resolved") {
		t.Fatalf("provider invocation error = %v, want durable profile append failure", err)
	}
	if chatRequests.Load() != 0 {
		t.Fatalf("provider chat requests = %d, want 0 after append failure", chatRequests.Load())
	}
	if len(statusEvents) != 0 {
		t.Fatalf("status events = %#v, want no profile status after append failure", statusEvents)
	}
}

func TestAuxiliarySidecarsReportOneProfilePerProviderInvocation(t *testing.T) {
	models := map[string]int{
		"aux-sidecar-model": 512,
		"aux-guard-model":   512,
		"aux-judge-model":   1024,
	}
	var statusMu sync.Mutex
	statusEvents := make([]StatusEvent, 0, 4)
	requestCounts := make(map[string]int, len(models))
	var handlerErr error
	provider := newIPv4TestServer(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/v1/chat/completions" {
			http.NotFound(writer, request)
			return
		}
		var body struct {
			Model string `json:"model"`
		}
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			statusMu.Lock()
			handlerErr = fmt.Errorf("decode provider request: %w", err)
			statusMu.Unlock()
			http.Error(writer, handlerErr.Error(), http.StatusBadRequest)
			return
		}
		statusMu.Lock()
		wantOutput, ok := models[body.Model]
		if !ok {
			handlerErr = fmt.Errorf("unexpected auxiliary model %q", body.Model)
		} else {
			profiles := 0
			for _, event := range statusEvents {
				if event.Type == string(EventModelProfileResolved) && event.ModelProfile != nil && event.ModelProfile.ModelID == body.Model {
					profiles++
					if event.ModelProfile.MaxOutput.Value != wantOutput {
						handlerErr = fmt.Errorf("%s max output = %d, want reservation %d", body.Model, event.ModelProfile.MaxOutput.Value, wantOutput)
					}
				}
			}
			if profiles != requestCounts[body.Model]+1 {
				handlerErr = fmt.Errorf("%s profile events before request = %d, want %d", body.Model, profiles, requestCounts[body.Model]+1)
			}
			requestCounts[body.Model]++
		}
		statusMu.Unlock()

		writer.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(writer, `{"id":"auxiliary","object":"chat.completion","created":1,"model":%q,"choices":[{"index":0,"message":{"role":"assistant","content":"{\"approved\":true}"},"finish_reason":"stop"}]}`, body.Model)
	}))
	defer provider.Close()

	manager, err := agent.NewProviderManager(provider.URL+"/v1", "auxiliary-secret", nil)
	if err != nil {
		t.Fatal(err)
	}
	runtime := &ModelProfileRuntime{
		manager: manager,
		resolver: modelprofile.NewRuntimeResolver(func(providerintrospection.ProviderRef) providerintrospection.ModelIntrospector {
			return auxiliaryProfileIntrospector{}
		}, modelprofile.ProfileCacheOptions{}),
	}
	workspace := t.TempDir()
	store, err := NewEventStore(workspace, "run-auxiliary-profile", "session-auxiliary-profile")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	c := &Coordinator{
		providerManager:         manager,
		modelProfileRuntime:     runtime,
		session:                 &TeamSession{Workspace: workspace, Config: agent.TeamConfig{Generation: agent.GenerationParams{MaxTokens: "4096"}}},
		eventStore:              store,
		executionRunID:          "run-auxiliary-profile",
		providerBoundaryStarted: true,
		sidecarModel:            "aux-sidecar-model",
		guardModel:              "aux-guard-model",
		judgeModel:              "aux-judge-model",
		reportStatus: func(event StatusEvent) {
			statusMu.Lock()
			statusEvents = append(statusEvents, event)
			statusMu.Unlock()
		},
	}

	s := c.Sidecar()
	g := c.GuardSidecar()
	j := c.JudgeSidecar()
	if s == nil || g == nil || j == nil {
		t.Fatalf("auxiliary construction returned sidecar=%v guard=%v judge=%v", s != nil, g != nil, j != nil)
	}
	if events, err := store.ReadEvents(); err != nil {
		t.Fatal(err)
	} else if len(events) != 0 {
		t.Fatalf("construction telemetry events = %d, want zero", len(events))
	}

	if _, err := s.Execute(t.Context(), "return a short response"); err != nil {
		t.Fatalf("sidecar invocation: %v", err)
	}
	if _, err := s.Execute(t.Context(), "return a second short response"); err != nil {
		t.Fatalf("second sidecar invocation: %v", err)
	}
	if result, err := g.ReviewToolCall(t.Context(), "worker", "view", "{}", []string{"allow view"}); err != nil || !result.Approved {
		t.Fatalf("guard invocation result=%#v err=%v, want approved", result, err)
	}
	if _, err := j.ExecuteProfile(t.Context(), "return the best candidate", sidecar.JudgeProfile); err != nil {
		t.Fatalf("judge invocation: %v", err)
	}

	statusMu.Lock()
	err = handlerErr
	statusMu.Unlock()
	if err != nil {
		t.Fatal(err)
	}

	events, err := store.ReadEvents()
	if err != nil {
		t.Fatal(err)
	}
	profileCounts := make(map[string]int, len(models))
	for _, event := range events {
		if event.Type != string(EventModelProfileResolved) {
			continue
		}
		var projection modelprofile.TelemetryProjection
		if err := json.Unmarshal(event.Payload, &projection); err != nil {
			t.Fatal(err)
		}
		profileCounts[projection.ModelID]++
		for _, forbidden := range []string{"http://", "https://", "auxiliary-secret", "Bearer"} {
			if strings.Contains(string(event.Payload), forbidden) {
				t.Fatalf("durable profile payload contains %q: %s", forbidden, event.Payload)
			}
		}
	}
	for modelID, want := range map[string]int{"aux-sidecar-model": 2, "aux-guard-model": 1, "aux-judge-model": 1} {
		if profileCounts[modelID] != want {
			t.Fatalf("durable %s profile events = %d, want %d", modelID, profileCounts[modelID], want)
		}
	}
	profiles, err := LoadModelProfileTelemetry(workspace, "run-auxiliary-profile")
	if err != nil {
		t.Fatal(err)
	}
	if len(profiles) != 4 {
		t.Fatalf("loaded auxiliary profiles = %d, want four actual invocations", len(profiles))
	}
}

func TestAuxiliarySidecarBinderFailsClosedOnProfileTelemetryAppendFailure(t *testing.T) {
	const modelID = "aux-binder-append-failure"
	var chatRequests atomic.Int32
	provider := newIPv4TestServer(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/v1/chat/completions" {
			chatRequests.Add(1)
		}
		writer.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(writer, "data: {\"id\":\"unexpected\",\"object\":\"chat.completion.chunk\",\"created\":1,\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\n")
		_, _ = fmt.Fprint(writer, "data: [DONE]\n\n")
	}))
	defer provider.Close()
	manager, err := agent.NewProviderManager(provider.URL+"/v1", "binder-secret", nil)
	if err != nil {
		t.Fatal(err)
	}
	runtime := &ModelProfileRuntime{
		manager: manager,
		resolver: modelprofile.NewRuntimeResolver(func(providerintrospection.ProviderRef) providerintrospection.ModelIntrospector {
			return auxiliaryProfileIntrospector{}
		}, modelprofile.ProfileCacheOptions{}),
	}
	workspace := t.TempDir()
	store, err := NewEventStore(workspace, "run-aux-binder-failure", "session-aux-binder-failure")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	var statusEvents []StatusEvent
	c := &Coordinator{
		providerManager:         manager,
		modelProfileRuntime:     runtime,
		session:                 &TeamSession{Workspace: workspace},
		eventStore:              store,
		executionRunID:          "run-aux-binder-failure",
		providerBoundaryStarted: true,
		sidecarModel:            modelID,
		reportStatus:            func(event StatusEvent) { statusEvents = append(statusEvents, event) },
	}
	s := c.Sidecar()
	if s == nil {
		t.Fatal("sidecar construction failed before injected append failure")
	}
	configureEventStoreSyncFailureForEventType(t, store, string(EventModelProfileResolved), 1, errors.New("injected binder profile append failure"))
	if _, err := s.Execute(t.Context(), "must not reach provider"); err == nil || !strings.Contains(err.Error(), "persist model profile resolved") {
		t.Fatalf("sidecar error = %v, want binder append failure", err)
	}
	if chatRequests.Load() != 0 {
		t.Fatalf("provider chat requests = %d, want zero after binder append failure", chatRequests.Load())
	}
	for _, event := range statusEvents {
		if event.Type == string(EventModelProfileResolved) {
			t.Fatalf("profile status event = %#v, want none after binder append failure", event)
		}
	}
}
