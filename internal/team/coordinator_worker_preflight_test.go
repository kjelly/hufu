package team

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"charm.land/fantasy"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/config"
	"github.com/kjelly/hufu/internal/modelprofile"
	"github.com/kjelly/hufu/internal/providerintrospection"
	"github.com/kjelly/hufu/internal/skill"
	"github.com/kjelly/hufu/internal/tools"
)

func TestExecuteTaskWorkerUsesResolvedToolsAfterCoordinatorPreflight(t *testing.T) {
	modelID := "worker-preflight-boundary-test"
	GlobalModelSpecRegistry().RegisterSpec(ModelContextSpec{
		ModelID: modelID, ContextWindow: 8192, MaxOutputTokens: 128, SafetyMarginTokens: 32,
	})

	var (
		mu            sync.Mutex
		providerTools []string
		requestCount  int
	)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Tools []struct {
				Function struct {
					Name string `json:"name"`
				} `json:"function"`
			} `json:"tools"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			http.Error(w, fmt.Sprintf("decode request: %v", err), http.StatusBadRequest)
			return
		}
		names := make([]string, 0, len(request.Tools))
		for _, tool := range request.Tools {
			names = append(names, tool.Function.Name)
		}
		mu.Lock()
		providerTools = append([]string(nil), names...)
		requestCount++
		callNumber := requestCount
		mu.Unlock()

		w.Header().Set("Content-Type", "text/event-stream")
		if callNumber == 1 {
			arguments := `{"status":"success","summary":"review completed"}`
			fmt.Fprintf(w, "data: {\"id\":\"worker-preflight\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":%q,\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"tool_calls\":[{\"index\":0,\"id\":\"submit-1\",\"type\":\"function\",\"function\":{\"name\":\"submit_result\",\"arguments\":%q}}]},\"finish_reason\":null}]}\n\n", modelID, arguments)
			fmt.Fprint(w, "data: {\"id\":\"worker-preflight\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"worker\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n")
		} else {
			fmt.Fprint(w, "data: {\"id\":\"worker-preflight\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"worker\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"done\"},\"finish_reason\":\"stop\"}]}\n\n")
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	})
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Skipf("network listener unavailable in this environment: %v", err)
	}
	provider := httptest.NewUnstartedServer(handler)
	provider.Listener = listener
	provider.Start()
	defer provider.Close()

	workspace := t.TempDir()
	const runID = "worker-preflight-run"
	store, err := NewEventStore(workspace, runID, "session-worker-preflight")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	worker := &agent.AgentDef{
		Name: "reviewer", Role: "worker", Tools: "view,grep,glob,ls", Skills: "large", MaxRetries: 0,
		Timeout: 10, Generation: agent.GenerationParams{Model: modelID, MaxTokens: "128"},
	}
	session := &TeamSession{
		Dir: workspace, Workspace: workspace,
		Config: agent.TeamConfig{
			Name: "worker-preflight", Timeout: 10, MaxRetries: 0,
			Generation: agent.GenerationParams{Model: modelID, MaxTokens: "128"},
		},
		Agents: map[string]*agent.AgentDef{"reviewer": worker},
	}
	providerManager, err := agent.NewProviderManager(provider.URL, "", map[string]config.ProviderConfig{
		"ollama": {ProviderURL: provider.URL},
	})
	if err != nil {
		t.Fatal(err)
	}
	c := &Coordinator{
		session:         session,
		projectDir:      workspace,
		skills:          []*skill.SkillDef{{Name: "large", Description: "large workflow", Path: "skills/large/SKILL.md", Content: "FULL REQUIRED INSTRUCTIONS"}},
		providerManager: providerManager,
		modelProfileRuntime: &ModelProfileRuntime{
			manager: providerManager,
			resolver: modelprofile.NewRuntimeResolver(func(providerintrospection.ProviderRef) providerintrospection.ModelIntrospector {
				return auxiliaryProfileIntrospector{}
			}, modelprofile.ProfileCacheOptions{}),
		},
		coreTools:      agent.BuildAllAgentTools(workspace, tools.WithAllowedPaths([]string{workspace})),
		taskTracker:    NewTaskTracker(),
		sessionData:    NewSession(),
		sessionTime:    time.Now(),
		eventStore:     store,
		executionRunID: runID,
		reportStatus:   func(StatusEvent) {},
	}
	item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "reviewer", Desc: "review the bounded workset", Execution: ExecutionContract{RequiresResult: true}}})[0]
	task := TaskDef{Agent: "reviewer", Goal: "review the bounded workset", Execution: ExecutionContract{RequiresResult: true}}

	resolved, err := c.ToolResolver().ResolveTaskTools(t.Context(), worker, WorkerToolResolutionRequest{
		Task: task, TodoID: item.ID, Mode: WorkerToolResolutionNormal,
	})
	if err != nil {
		t.Fatalf("ResolveTaskTools: %v", err)
	}
	coordinatorAgent := fantasy.NewAgentTool("agent", strings.Repeat("coordinator-only delegation schema ", 800), func(context.Context, map[string]any, fantasy.ToolCall) (fantasy.ToolResponse, error) {
		return fantasy.NewTextResponse("unused"), nil
	})
	coordinatorFinish := fantasy.NewAgentTool("finish", strings.Repeat("coordinator-only completion schema ", 800), func(context.Context, map[string]any, fantasy.ToolCall) (fantasy.ToolResponse, error) {
		return fantasy.NewTextResponse("unused"), nil
	})
	preflight := newCoordinatorRequestPreflight(
		modelID,
		"coordinator request",
		strings.Repeat("oversized coordinator-only instructions ", 800),
		[]fantasy.AgentTool{coordinatorAgent, coordinatorFinish},
	)
	parentCtx := withCoordinatorRequestPreflight(t.Context(), preflight)
	if _, err := c.executeTask(parentCtx, task, item.ID); err != nil {
		t.Fatalf("executeTask: %v", err)
	}

	mu.Lock()
	gotProviderTools := append([]string(nil), providerTools...)
	gotRequests := requestCount
	mu.Unlock()
	if gotRequests == 0 {
		t.Fatal("worker made no provider request")
	}
	if !slices.Equal(gotProviderTools, resolved.Names) {
		t.Fatalf("provider-visible tools = %v, resolver result = %v", gotProviderTools, resolved.Names)
	}
	if !slices.Contains(gotProviderTools, "submit_result") {
		t.Fatalf("provider-visible tools = %v, missing submit_result", gotProviderTools)
	}
	if slices.Contains(gotProviderTools, "load_skill") {
		t.Fatalf("provider-visible tools = %v, contains ungranted load_skill", gotProviderTools)
	}
	for _, forbidden := range []string{"agent", "finish", "approve_plan"} {
		if slices.Contains(gotProviderTools, forbidden) {
			t.Fatalf("provider-visible tools = %v, contains coordinator-only tool %q", gotProviderTools, forbidden)
		}
	}
	stored := c.GetTaskResult(item.ID)
	if stored == nil || stored.Status != TaskResultStatusSuccess || stored.Summary != "review completed" {
		t.Fatalf("stored typed result = %#v, want successful submit_result", stored)
	}
	if len(item.InjectedSkills) != 0 {
		t.Fatalf("full skill disclosure left mandatory-load state: %v", item.InjectedSkills)
	}
}

type protocolRepairProviderCapture struct {
	mu        sync.Mutex
	requests  [][]string
	submitted bool
}

func (p *protocolRepairProviderCapture) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Tools []struct {
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		} `json:"tools"`
	}
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		http.Error(w, fmt.Sprintf("decode request: %v", err), http.StatusBadRequest)
		return
	}
	names := make([]string, 0, len(request.Tools))
	for _, tool := range request.Tools {
		names = append(names, tool.Function.Name)
	}
	p.mu.Lock()
	p.requests = append(p.requests, slices.Clone(names))
	shouldSubmit := len(names) == 1 && names[0] == submitResultToolName && !p.submitted
	if shouldSubmit {
		p.submitted = true
	}
	p.mu.Unlock()

	w.Header().Set("Content-Type", "text/event-stream")
	if shouldSubmit {
		arguments := `{"status":"success","summary":"protocol repair completed"}`
		fmt.Fprintf(w, "data: {\"id\":\"protocol-repair\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"protocol-repair\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"tool_calls\":[{\"index\":0,\"id\":\"submit-1\",\"type\":\"function\",\"function\":{\"name\":\"submit_result\",\"arguments\":%q}}]},\"finish_reason\":null}]}\n\n", arguments)
		fmt.Fprint(w, "data: {\"id\":\"protocol-repair\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"protocol-repair\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n")
	} else {
		fmt.Fprint(w, "data: {\"id\":\"protocol-repair\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"protocol-repair\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"execution completed\"},\"finish_reason\":\"stop\"}]}\n\n")
	}
	fmt.Fprint(w, "data: [DONE]\n\n")
}

func (p *protocolRepairProviderCapture) providerRequests() [][]string {
	p.mu.Lock()
	defer p.mu.Unlock()
	requests := make([][]string, len(p.requests))
	for i, request := range p.requests {
		requests[i] = slices.Clone(request)
	}
	return requests
}

func newProtocolRepairProviderCoordinator(t *testing.T) (*Coordinator, *TodoItem, *protocolRepairProviderCapture) {
	t.Helper()
	modelID := "protocol-repair-preflight-boundary-test"
	GlobalModelSpecRegistry().RegisterSpec(ModelContextSpec{
		ModelID: modelID, ContextWindow: 8192, MaxOutputTokens: 128, SafetyMarginTokens: 32,
	})

	capture := new(protocolRepairProviderCapture)
	provider := newIPv4TestServer(t, capture)
	t.Cleanup(provider.Close)

	workspace := t.TempDir()
	const runID = "protocol-repair-preflight-run"
	store, err := NewEventStore(workspace, runID, "session-protocol-repair-preflight")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	worker := &agent.AgentDef{
		Name: "reviewer", Role: "worker", Tools: "view,grep,glob,ls", MaxRetries: 0,
		Timeout: 10, Generation: agent.GenerationParams{Model: modelID, MaxTokens: "128"},
	}
	session := &TeamSession{
		Dir: workspace, Workspace: workspace,
		Config: agent.TeamConfig{
			Name: "protocol-repair-preflight", Timeout: 10, MaxRetries: 0,
			Generation: agent.GenerationParams{Model: modelID, MaxTokens: "128"},
		},
		Agents: map[string]*agent.AgentDef{"reviewer": worker},
	}
	providerManager, err := agent.NewProviderManager(provider.URL, "", map[string]config.ProviderConfig{
		"ollama": {ProviderURL: provider.URL},
	})
	if err != nil {
		t.Fatal(err)
	}
	c := &Coordinator{
		session:         session,
		projectDir:      workspace,
		providerManager: providerManager,
		modelProfileRuntime: &ModelProfileRuntime{
			manager: providerManager,
			resolver: modelprofile.NewRuntimeResolver(func(providerintrospection.ProviderRef) providerintrospection.ModelIntrospector {
				return auxiliaryProfileIntrospector{}
			}, modelprofile.ProfileCacheOptions{}),
		},
		coreTools:      agent.BuildAllAgentTools(workspace, tools.WithAllowedPaths([]string{workspace})),
		taskTracker:    NewTaskTracker(),
		sessionData:    NewSession(),
		sessionTime:    time.Now(),
		eventStore:     store,
		executionRunID: runID,
		reportStatus:   func(StatusEvent) {},
	}
	item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "reviewer", Desc: "repair the bounded result", Execution: ExecutionContract{RequiresResult: true}}})[0]
	return c, item, capture
}

func oversizedCoordinatorPreflightContext(t *testing.T, modelID string) context.Context {
	t.Helper()
	coordinatorAgent := fantasy.NewAgentTool("agent", strings.Repeat("coordinator-only delegation schema ", 800), func(context.Context, map[string]any, fantasy.ToolCall) (fantasy.ToolResponse, error) {
		return fantasy.NewTextResponse("unused"), nil
	})
	coordinatorFinish := fantasy.NewAgentTool("finish", strings.Repeat("coordinator-only completion schema ", 800), func(context.Context, map[string]any, fantasy.ToolCall) (fantasy.ToolResponse, error) {
		return fantasy.NewTextResponse("unused"), nil
	})
	preflight := newCoordinatorRequestPreflight(
		modelID,
		"coordinator request",
		strings.Repeat("oversized coordinator-only instructions ", 800),
		[]fantasy.AgentTool{coordinatorAgent, coordinatorFinish},
	)
	return withCoordinatorRequestPreflight(t.Context(), preflight)
}

func assertProtocolRepairProviderSurface(t *testing.T, requests [][]string, wantWorkerSurface []string, wantWorkerRequest bool) {
	t.Helper()
	if len(requests) == 0 {
		t.Fatal("provider made no request")
	}
	if wantWorkerRequest {
		if len(requests) < 2 {
			t.Fatalf("provider requests = %v, want normal worker requests followed by result-only repair", requests)
		}
		for _, request := range requests[:len(requests)-1] {
			if !slices.Equal(request, wantWorkerSurface) {
				t.Fatalf("worker provider-visible tools = %v, want normal surface %v before final repair", request, wantWorkerSurface)
			}
		}
	}
	for _, request := range requests[len(requests)-1:] {
		if !slices.Equal(request, []string{submitResultToolName}) {
			t.Fatalf("protocol-repair provider-visible tools = %v, want [%s]", request, submitResultToolName)
		}
		for _, forbidden := range []string{"agent", "finish", "approve_plan"} {
			if slices.Contains(request, forbidden) {
				t.Fatalf("protocol-repair provider-visible tools = %v, contains coordinator-only tool %q", request, forbidden)
			}
		}
	}
}

func TestResultOnlyRepairUsesSubmitResultAfterInheritedCoordinatorPreflight(t *testing.T) {
	c, item, capture := newProtocolRepairProviderCoordinator(t)
	worker := c.session.Agents["reviewer"]
	task := TaskDef{Agent: "reviewer", Goal: "repair the bounded result", Execution: ExecutionContract{RequiresResult: true}}
	normalResolved, err := c.ToolResolver().ResolveTaskTools(t.Context(), worker, WorkerToolResolutionRequest{
		Task: task, TodoID: item.ID, Mode: WorkerToolResolutionNormal,
	})
	if err != nil {
		t.Fatalf("ResolveTaskTools: %v", err)
	}
	resultRepairResolved, err := c.ToolResolver().ResolveTaskTools(t.Context(), worker, WorkerToolResolutionRequest{
		Task: task, TodoID: item.ID, Mode: WorkerToolResolutionResultRepair,
	})
	if err != nil {
		t.Fatalf("ResolveTaskTools result repair: %v", err)
	}

	if _, err := c.executeTask(oversizedCoordinatorPreflightContext(t, worker.Generation.Model), task, item.ID); err != nil {
		t.Fatalf("executeTask: %v", err)
	}
	assertProtocolRepairProviderSurface(t, capture.providerRequests(), normalResolved.Names, true)
	if !slices.Equal(resultRepairResolved.Names, []string{submitResultToolName}) {
		t.Fatalf("result-repair provider tools = %v, want [%s]", resultRepairResolved.Names, submitResultToolName)
	}
	stored := c.GetTaskResult(item.ID)
	if stored == nil || stored.Status != TaskResultStatusSuccess || stored.Summary != "protocol repair completed" {
		t.Fatalf("stored typed result = %#v, want successful protocol repair result", stored)
	}
}

func TestResumedProtocolRepairUsesSubmitResultAfterInheritedCoordinatorPreflight(t *testing.T) {
	c, item, capture := newProtocolRepairProviderCoordinator(t)
	if err := c.taskTracker.TodoList().TryUpdateStatusAndOutput(item.ID, TaskInProgress, "worker started", "checkpointed execution evidence"); err != nil {
		t.Fatalf("mark task in progress: %v", err)
	}
	if err := c.taskTracker.TodoList().TryUpdateStatusAndOutput(item.ID, TaskProtocolIncomplete, "worker omitted result", "checkpointed execution evidence"); err != nil {
		t.Fatalf("checkpoint protocol-incomplete task: %v", err)
	}
	task := TaskDef{Agent: "reviewer", Goal: "repair the bounded result", Execution: ExecutionContract{RequiresResult: true}}

	if _, err := c.executeTask(oversizedCoordinatorPreflightContext(t, c.session.Config.Generation.Model), task, item.ID); err != nil {
		t.Fatalf("executeTask: %v", err)
	}
	assertProtocolRepairProviderSurface(t, capture.providerRequests(), nil, false)
	stored := c.GetTaskResult(item.ID)
	if stored == nil || stored.Status != TaskResultStatusSuccess || stored.Summary != "protocol repair completed" {
		t.Fatalf("stored typed result = %#v, want successful resumed protocol repair result", stored)
	}
}
