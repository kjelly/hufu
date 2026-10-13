package team

import (
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"sync"
	"testing"
)

// usageReportingServer answers like fallbackTestServer but reports token
// usage, so attempt receipts carry real provider usage.
type usageReportingServer struct {
	mu       sync.Mutex
	failures map[string]int
}

func (s *usageReportingServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Model string `json:"model"`
	}
	_ = json.NewDecoder(r.Body).Decode(&request)
	s.mu.Lock()
	status, fails := s.failures[request.Model]
	s.mu.Unlock()
	if fails {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = fmt.Fprintf(w, `{"error":{"message":"injected %d for %s","type":"test_error"}}`, status, request.Model)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	_, _ = fmt.Fprintf(w, "data: {\"id\":\"u\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":%q,\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"tool_calls\":[{\"index\":0,\"id\":\"call-submit\",\"type\":\"function\",\"function\":{\"name\":%q,\"arguments\":%q}}]},\"finish_reason\":null}]}\n\n", request.Model, submitResultToolName, `{"status":"success","summary":"done"}`)
	_, _ = fmt.Fprint(w, "data: {\"id\":\"u\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"worker\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n")
	_, _ = fmt.Fprint(w, "data: {\"id\":\"u\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"worker\",\"choices\":[],\"usage\":{\"prompt_tokens\":11,\"completion_tokens\":7,\"total_tokens\":18}}\n\n")
	_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
}

// TestExecutionEventProjectionMatchesLegacyOnRealAttempts runs real task
// attempts against a model server and requires the event-store projection to
// match the legacy stream on every field telemetry aggregates.
func TestExecutionEventProjectionMatchesLegacyOnRealAttempts(t *testing.T) {
	tests := []struct {
		name       string
		failures   map[string]int
		candidates []string
	}{
		{name: "primary succeeds", candidates: []string{"ollama/fb-primary", "remote/fb-remote"}},
		{name: "fallback to another backend", failures: map[string]int{"fb-primary": http.StatusServiceUnavailable}, candidates: []string{"ollama/fb-primary", "remote/fb-remote"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := newIPv4TestServer(t, &usageReportingServer{failures: tt.failures})
			t.Cleanup(server.Close)
			f := newFallbackFixture(t, map[string]string{"ollama": server.URL, "remote": server.URL}, tt.candidates, allFallbackClasses, SideEffectNone)
			logger, err := newExecutionEventLogger(f.c.session.Workspace)
			if err != nil {
				t.Fatal(err)
			}
			f.c.executionEvents = logger
			// beginExecutionRun opens every run on both streams.
			team := f.c.session.Config.Name
			if err := f.c.emitEvent("run_started", "coordinator", "", LifecycleEventPayload{Team: team}); err != nil {
				t.Fatal(err)
			}
			if err := logger.append(ExecutionEvent{Status: "run_started", RunID: f.c.executionRunID, Team: team}); err != nil {
				t.Fatal(err)
			}
			if err := f.run(t); err != nil {
				t.Fatalf("executeTask: %v", err)
			}
			logger.close()

			legacy, err := ReadExecutionEvents(filepath.Join(f.c.session.Workspace, logsDir, executionEventsFile))
			if err != nil {
				t.Fatal(err)
			}
			events, err := f.store.ReadEvents()
			if err != nil {
				t.Fatal(err)
			}
			projected := projectedExecutionEvents(events)
			if err := CompareExecutionEventsParity(legacy, projected); err != nil {
				t.Fatalf("projection differs from the legacy stream: %v\nlegacy=%+v\nprojected=%+v", err, legacy, projected)
			}
			// Guard against a vacuous comparison: the attempts must have
			// reported usage and a model for the projection to match.
			var usage ExecutionUsage
			model := ""
			for _, row := range projected {
				usage = addExecutionUsage(usage, row.Usage)
				if row.Model != "" {
					model = row.Model
				}
			}
			if usage.TotalTokens == 0 || model == "" {
				t.Fatalf("projection has no usage or model to compare: usage=%+v model=%q", usage, model)
			}
		})
	}
}

func TestEmergencyFinalizedRunProjectsCancelledFinish(t *testing.T) {
	workspace := t.TempDir()
	const runID = "run-emergency-projection"
	store, err := NewEventStore(workspace, runID, "session-emergency-projection")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	c := &Coordinator{
		eventStore: store, executionRunID: runID, terminalLifecycleRunID: runID,
		session: &TeamSession{Workspace: workspace}, sessionData: NewSession(), taskTracker: NewTaskTracker(),
	}
	if err := c.emitEvent("run_started", "coordinator", "", LifecycleEventPayload{Team: "alpha", TeamRevision: "rev-1"}); err != nil {
		t.Fatal(err)
	}
	// The interrupt path finalizes and then exits without running the
	// invocation's deferred close, which is where the legacy stream wrote
	// its run_finished line.
	if err := c.EmergencyFinalizeRun(t.Context()); err != nil {
		t.Fatal(err)
	}
	events, err := store.ReadEvents()
	if err != nil {
		t.Fatal(err)
	}
	projected := projectedExecutionEvents(events)
	if len(projected) == 0 {
		t.Fatal("projection is empty")
	}
	last := projected[len(projected)-1]
	if last.Status != "run_finished" || last.Outcome != RunOutcomeCancelled || last.Team != "alpha" || last.TeamRevision != "rev-1" {
		t.Fatalf("last projected row = %+v, want a cancelled run_finished for team alpha at rev-1", last)
	}
}
