package team

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
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

// TestExecutionEventProjectionOnRealAttempts runs real task attempts against
// a model server and checks what the projection reports for them: the model
// each attempt ran on and the usage the provider reported.
func TestExecutionEventProjectionOnRealAttempts(t *testing.T) {
	tests := []struct {
		name     string
		failures map[string]int
		// terminal maps each terminal row to the model it must report.
		terminal []string
	}{
		{name: "primary succeeds", terminal: []string{"done/1/fb-primary"}},
		{
			name:     "fallback to another backend",
			failures: map[string]int{"fb-primary": http.StatusServiceUnavailable},
			terminal: []string{"error/1/fb-primary", "done/2/remote/fb-remote"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := newIPv4TestServer(t, &usageReportingServer{failures: tt.failures})
			t.Cleanup(server.Close)
			f := newFallbackFixture(t, map[string]string{"ollama": server.URL, "remote": server.URL}, []string{"ollama/fb-primary", "remote/fb-remote"}, allFallbackClasses, SideEffectNone)
			team := f.c.session.Config.Name
			if err := f.c.emitEvent("run_started", "coordinator", "", LifecycleEventPayload{Team: team}); err != nil {
				t.Fatal(err)
			}
			if err := f.run(t); err != nil {
				t.Fatalf("executeTask: %v", err)
			}
			events, err := f.store.ReadEvents()
			if err != nil {
				t.Fatal(err)
			}
			var terminal []string
			var usage ExecutionUsage
			for _, row := range projectEvents(events) {
				if row.TaskID == "" {
					continue
				}
				if row.Team != team {
					t.Fatalf("row %+v has team %q, want %q", row, row.Team, team)
				}
				usage = addExecutionUsage(usage, row.Usage)
				if isTerminalExecutionStatus(row.Status) {
					terminal = append(terminal, fmt.Sprintf("%s/%d/%s", row.Status, row.Attempt, row.Model))
				}
			}
			if !slices.Equal(terminal, tt.terminal) {
				t.Fatalf("terminal rows = %v, want %v", terminal, tt.terminal)
			}
			// The server reports 11 prompt and 7 completion tokens per call.
			if usage.InputTokens != 11 || usage.OutputTokens != 7 || usage.TotalTokens != 18 {
				t.Fatalf("task usage = %+v, want the successful attempt's 11/7/18", usage)
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
	projected := projectEvents(events)
	if len(projected) == 0 {
		t.Fatal("projection is empty")
	}
	last := projected[len(projected)-1]
	if last.Status != "run_finished" || last.Outcome != RunOutcomeCancelled || last.Team != "alpha" || last.TeamRevision != "rev-1" {
		t.Fatalf("last projected row = %+v, want a cancelled run_finished for team alpha at rev-1", last)
	}
}

// projectEvents projects a complete event slice.
func projectEvents(events []RunEvent) []ExecutionEvent {
	projector := NewExecutionEventProjector()
	for _, event := range events {
		projector.Add(event)
	}
	return projector.Rows()
}

// workspaceExecutionRows projects workspace's validated event store.
func workspaceExecutionRows(t *testing.T, workspace string) []ExecutionEvent {
	t.Helper()
	projector := NewExecutionEventProjector()
	if err := StreamValidatedRunEvents(t.Context(), workspace, func(event RunEvent) error {
		projector.Add(event)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return projector.Rows()
}

// assertProjectedRows compares the row fields execution telemetry reads.
func assertProjectedRows(t *testing.T, got, want []ExecutionEvent) {
	t.Helper()
	describe := func(rows []ExecutionEvent) []string {
		out := make([]string, 0, len(rows))
		for _, row := range rows {
			out = append(out, fmt.Sprintf("%s run=%s task=%s agent=%s attempt=%d team=%s outcome=%s acceptance=%s evidence=%s",
				row.Status, row.RunID, row.TaskID, row.Agent, row.Attempt, row.Team, row.Outcome, row.AcceptanceState, row.EvidenceManifestHash))
		}
		return out
	}
	if g, w := describe(got), describe(want); !slices.Equal(g, w) {
		t.Fatalf("projected rows:\n%s\nwant:\n%s", strings.Join(g, "\n"), strings.Join(w, "\n"))
	}
}
