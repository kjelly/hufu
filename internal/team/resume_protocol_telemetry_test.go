package team

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/config"
)

type resumedProtocolProvider struct {
	mu          sync.Mutex
	chatTools   [][]string
	chatRequest int
	orderError  error
	profiles    []StatusEvent
}

func (p *resumedProtocolProvider) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/api/show":
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"model":"resume-protocol-telemetry-model","parameters":"num_ctx 8192"}`)
		return
	case "/api/ps":
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"models":[{"name":"resume-protocol-telemetry-model","context_length":8192}]}`)
		return
	case "/v1/chat/completions":
		var request struct {
			Tools []struct {
				Function struct {
					Name string `json:"name"`
				} `json:"function"`
			} `json:"tools"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			http.Error(w, fmt.Sprintf("decode provider request: %v", err), http.StatusBadRequest)
			return
		}
		names := make([]string, 0, len(request.Tools))
		for _, tool := range request.Tools {
			names = append(names, tool.Function.Name)
		}

		p.mu.Lock()
		p.chatRequest++
		requestNumber := p.chatRequest
		p.chatTools = append(p.chatTools, names)
		profileCount := len(p.profiles)
		if profileCount != requestNumber {
			p.orderError = fmt.Errorf("profile reports before provider request %d = %d, want %d", requestNumber, profileCount, requestNumber)
		}
		p.mu.Unlock()

		arguments := `{"status":"success"}`
		if requestNumber == 2 {
			arguments = `{"status":"success","summary":"resumed protocol repair succeeded","details":"bounded result-only repair"}`
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprintf(w, "data: {\"id\":\"resume-protocol\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"resume-protocol-telemetry-model\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"tool_calls\":[{\"index\":0,\"id\":\"submit-%d\",\"type\":\"function\",\"function\":{\"name\":\"submit_result\",\"arguments\":%q}}]},\"finish_reason\":null}]}\n\n", requestNumber, arguments)
		_, _ = fmt.Fprint(w, "data: {\"id\":\"resume-protocol\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"resume-protocol-telemetry-model\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n")
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	default:
		http.NotFound(w, r)
	}
}

func (p *resumedProtocolProvider) recordStatus(event StatusEvent) {
	if event.Type != string(EventModelProfileResolved) {
		return
	}
	p.mu.Lock()
	p.profiles = append(p.profiles, event)
	p.mu.Unlock()
}

func (p *resumedProtocolProvider) snapshot() ([][]string, []StatusEvent, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	tools := make([][]string, len(p.chatTools))
	for i := range p.chatTools {
		tools[i] = append([]string(nil), p.chatTools[i]...)
	}
	return tools, append([]StatusEvent(nil), p.profiles...), p.orderError
}

func newResumedProtocolTelemetryCoordinator(t *testing.T) (*Coordinator, *TodoItem, *resumedProtocolProvider, *EventStore) {
	t.Helper()
	const modelID = "resume-protocol-telemetry-model"
	GlobalModelSpecRegistry().RegisterSpec(ModelContextSpec{ModelID: modelID, ContextWindow: 8192, MaxOutputTokens: 128, SafetyMarginTokens: 32})

	provider := new(resumedProtocolProvider)
	server := newIPv4TestServer(t, provider)
	t.Cleanup(server.Close)
	manager, err := agent.NewProviderManager(server.URL+"/v1", "provider-secret", map[string]config.ProviderConfig{
		"ollama": {ProviderURL: server.URL + "/v1"},
	})
	if err != nil {
		t.Fatalf("NewProviderManager: %v", err)
	}
	workspace := t.TempDir()
	runID := "run-resumed-protocol-telemetry"
	store, err := NewEventStore(workspace, runID, "session-resumed-protocol-telemetry")
	if err != nil {
		t.Fatalf("NewEventStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	worker := &agent.AgentDef{
		Name: "worker", Role: "worker", Tools: "view",
		Generation: agent.GenerationParams{Model: modelID, MaxTokens: "128"},
	}
	c := &Coordinator{
		session: &TeamSession{
			Dir: workspace, Workspace: workspace,
			Config: agent.TeamConfig{Name: "resumed-protocol-telemetry"},
			Agents: map[string]*agent.AgentDef{"worker": worker},
		},
		projectDir:              workspace,
		providerManager:         manager,
		modelProfileRuntime:     NewModelProfileRuntime(manager, false),
		coreTools:               agent.BuildAllAgentTools(workspace),
		taskTracker:             NewTaskTracker(),
		sessionData:             NewSession(),
		eventStore:              store,
		executionRunID:          runID,
		reportStatus:            provider.recordStatus,
		providerBoundaryStarted: true,
	}
	item := &TodoItem{
		ID: "1", Agent: "worker", Desc: "resume result-only repair",
		Status:    TaskProtocolIncomplete,
		Output:    "checkpointed evidence contains api_token=resume-secret",
		Execution: ExecutionContract{RequiresResult: true},
	}
	c.taskTracker.TodoList().Restore([]*TodoItem{item})
	return c, item, provider, store
}

func TestResumeProtocolRepairRebindsEachProviderInvocation(t *testing.T) {
	c, _, provider, store := newResumedProtocolTelemetryCoordinator(t)
	workerCalls := 0
	c.workerAgentOverride = &countingTextAgent{calls: &workerCalls, text: "UNSAFE WORKER REPLAY"}

	if _, err := c.ResumeInterruptedTasks(t.Context()); err != nil {
		updated := c.taskTracker.TodoList().Items()[0]
		t.Fatalf("ResumeInterruptedTasks: %v; receipt=%#v", err, updated.ExecutionReceipt)
	}

	requestTools, profiles, orderErr := provider.snapshot()
	if orderErr != nil {
		t.Fatal(orderErr)
	}
	if len(requestTools) != 2 || len(profiles) != 2 {
		t.Fatalf("provider requests/profile reports = %d/%d, want exactly 2/2", len(requestTools), len(profiles))
	}
	for i, names := range requestTools {
		if len(names) != 1 || names[0] != submitResultToolName {
			t.Fatalf("provider request %d tools = %v, want result-only surface", i+1, names)
		}
	}
	if workerCalls != 0 {
		t.Fatalf("worker replay calls = %d, want 0", workerCalls)
	}

	events, err := store.ReadEvents()
	if err != nil {
		t.Fatalf("ReadEvents: %v", err)
	}
	profileEvents := 0
	for _, event := range events {
		if event.Type == string(EventModelProfileResolved) {
			profileEvents++
		}
	}
	if profileEvents != 2 {
		t.Fatalf("durable profile events = %d, want 2", profileEvents)
	}
	loadedProfiles, err := LoadModelProfileTelemetry(c.session.Workspace, c.executionRunID)
	if err != nil {
		t.Fatalf("LoadModelProfileTelemetry: %v", err)
	}
	if len(loadedProfiles) != 2 {
		t.Fatalf("loaded profile events = %d, want 2", len(loadedProfiles))
	}
	jsonl, err := os.ReadFile(filepath.Join(c.session.Workspace, logsDir, eventStoreFile))
	if err != nil {
		t.Fatalf("read event JSONL: %v", err)
	}
	jsonlProfileEvents := 0
	for line := range strings.SplitSeq(strings.TrimSpace(string(jsonl)), "\n") {
		var event RunEvent
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatalf("decode event JSONL line: %v", err)
		}
		if event.Type == string(EventModelProfileResolved) {
			jsonlProfileEvents++
		}
	}
	if jsonlProfileEvents != 2 {
		t.Fatalf("JSONL profile events = %d, want 2", jsonlProfileEvents)
	}
	for _, secret := range []string{"provider-secret", "resume-secret"} {
		if strings.Contains(string(jsonl), secret) {
			t.Fatalf("event JSONL contains secret %q", secret)
		}
	}

	updated := c.taskTracker.TodoList().Items()[0]
	if updated.Status != TaskDone || updated.TypedResult == nil || updated.TypedResult.Source != "submitted" {
		t.Fatalf("resumed task = %#v, want done submitted result", updated)
	}
	provenance := updated.ExecutionReceipt.RepairProvenance
	if provenance == nil || !provenance.Success || provenance.RepairAttempts != 2 || len(provenance.History) != 2 {
		t.Fatalf("repair provenance = %#v, want two-attempt success", provenance)
	}
	if provenance.History[0].FailureReason != RepairFailureInvalidSchema || !provenance.History[1].Success {
		t.Fatalf("repair history = %#v, want invalid_schema then success", provenance.History)
	}
}

func TestResumeProtocolRepairSecondProfileAppendFailureMakesNoProviderRequest(t *testing.T) {
	c, _, provider, store := newResumedProtocolTelemetryCoordinator(t)
	configureEventStoreSyncFailureForEventType(t, store, string(EventModelProfileResolved), 2, errors.New("injected second model profile append failure"))

	if _, err := c.ResumeInterruptedTasks(t.Context()); err == nil {
		t.Fatal("expected second profile append failure")
	}
	requestTools, profiles, orderErr := provider.snapshot()
	if orderErr != nil {
		t.Fatal(orderErr)
	}
	if len(requestTools) != 1 {
		t.Fatalf("provider requests = %d, want only first repair request", len(requestTools))
	}
	if len(profiles) != 1 {
		t.Fatalf("reported profile events = %d, want only first successful append", len(profiles))
	}
	if len(requestTools[0]) != 1 || requestTools[0][0] != submitResultToolName {
		t.Fatalf("first provider tools = %v, want result-only surface", requestTools[0])
	}
}
