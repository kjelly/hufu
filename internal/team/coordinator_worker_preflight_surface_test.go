package team

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
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
	"github.com/kjelly/hufu/internal/tools"
)

type workerProviderSurfaceCapture struct {
	mu           sync.Mutex
	requests     [][]string
	mode         string
	submitted    bool
	planReviewed bool
}

func (p *workerProviderSurfaceCapture) ServeHTTP(w http.ResponseWriter, r *http.Request) {
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
	mode := p.mode
	shouldSubmit := (mode == "direct" || mode == "sub-agent") && slices.Contains(names, submitResultToolName) && !p.submitted
	if shouldSubmit {
		p.submitted = true
	}
	shouldRejectPlan := mode == "plan-reviewer" && slices.Contains(names, "reject_plan") && !p.planReviewed
	if shouldRejectPlan {
		p.planReviewed = true
	}
	p.mu.Unlock()

	w.Header().Set("Content-Type", "text/event-stream")
	switch {
	case shouldSubmit:
		arguments := `{"status":"success","summary":"direct provider result"}`
		fmt.Fprintf(w, "data: {\"id\":\"direct\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"worker\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"tool_calls\":[{\"index\":0,\"id\":\"submit-1\",\"type\":\"function\",\"function\":{\"name\":\"submit_result\",\"arguments\":%q}}]},\"finish_reason\":null}]}\n\n", arguments)
		fmt.Fprint(w, "data: {\"id\":\"direct\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"worker\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n")
	case shouldRejectPlan:
		arguments := `{"todo_id":"plan-reviewer-todo","reason":"fixture plan rejection"}`
		fmt.Fprintf(w, "data: {\"id\":\"plan-reviewer\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"worker\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"tool_calls\":[{\"index\":0,\"id\":\"reject-1\",\"type\":\"function\",\"function\":{\"name\":\"reject_plan\",\"arguments\":%q}}]},\"finish_reason\":null}]}\n\n", arguments)
		fmt.Fprint(w, "data: {\"id\":\"plan-reviewer\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"worker\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n")
	default:
		fmt.Fprint(w, "data: {\"id\":\"sub-agent\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"worker\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"sub-agent provider result\"},\"finish_reason\":\"stop\"}]}\n\n")
	}
	fmt.Fprint(w, "data: [DONE]\n\n")
}

func (p *workerProviderSurfaceCapture) providerRequests() [][]string {
	p.mu.Lock()
	defer p.mu.Unlock()
	requests := make([][]string, len(p.requests))
	for i, request := range p.requests {
		requests[i] = slices.Clone(request)
	}
	return requests
}

func newWorkerProviderSurfaceCoordinator(t *testing.T, modelID, mode string) (*Coordinator, *agent.AgentDef, *workerProviderSurfaceCapture) {
	t.Helper()
	GlobalModelSpecRegistry().RegisterSpec(ModelContextSpec{ModelID: modelID, ContextWindow: 8192, MaxOutputTokens: 128, SafetyMarginTokens: 32})
	capture := &workerProviderSurfaceCapture{mode: mode}
	provider := newIPv4TestServer(t, capture)
	t.Cleanup(provider.Close)
	workspace := t.TempDir()
	def := &agent.AgentDef{Name: "worker", Role: "worker", Tools: "view,grep,glob,ls", MaxRetries: 0, Timeout: 10, Generation: agent.GenerationParams{Model: modelID, MaxTokens: "128"}}
	session := &TeamSession{Dir: workspace, Workspace: workspace, Config: agent.TeamConfig{Name: "surface", Timeout: 10, MaxRetries: 0, Generation: agent.GenerationParams{Model: modelID, MaxTokens: "128"}}, Agents: map[string]*agent.AgentDef{"worker": def}}
	providerManager, err := agent.NewProviderManager(provider.URL, "", map[string]config.ProviderConfig{"ollama": {ProviderURL: provider.URL}})
	if err != nil {
		t.Fatal(err)
	}
	const runID = "surface-run"
	store, err := NewEventStore(workspace, runID, "surface-session")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	c := &Coordinator{
		session: session, projectDir: workspace, providerManager: providerManager,
		modelProfileRuntime: &ModelProfileRuntime{
			manager: providerManager,
			resolver: modelprofile.NewRuntimeResolver(func(providerintrospection.ProviderRef) providerintrospection.ModelIntrospector {
				return auxiliaryProfileIntrospector{}
			}, modelprofile.ProfileCacheOptions{}),
		},
		coreTools:   agent.BuildAllAgentTools(workspace, tools.WithAllowedPaths([]string{workspace})),
		taskTracker: NewTaskTracker(), sessionData: NewSession(), sessionTime: time.Now(), eventStore: store, executionRunID: runID, reportStatus: func(StatusEvent) {},
		providerBoundaryStart: func(context.Context, string) error { return nil },
	}
	return c, def, capture
}

func TestRunDirectAgentUsesWorkerSurfaceAfterInheritedCoordinatorPreflight(t *testing.T) {
	modelID := "direct-worker-surface-test"
	c, def, capture := newWorkerProviderSurfaceCoordinator(t, modelID, "direct")
	result, err := c.RunDirectAgent(oversizedCoordinatorPreflightContext(t, modelID), "worker", "perform direct work")
	if err != nil || result == nil {
		t.Fatalf("RunDirectAgent result=%#v err=%v", result, err)
	}
	requests := capture.providerRequests()
	if len(requests) == 0 {
		t.Fatal("direct agent made no provider request")
	}
	items := c.taskTracker.TodoList().Items()
	if len(items) != 1 {
		t.Fatalf("direct task items=%#v, want one task", items)
	}
	if !items[0].Execution.RequiresResult {
		t.Fatalf("direct task execution contract=%#v, want RequiresResult=true for resume", items[0].Execution)
	}
	resolved, err := c.ToolResolver().ResolveTaskTools(t.Context(), def, WorkerToolResolutionRequest{
		Task: TaskDef{Agent: "worker", Execution: ExecutionContract{RequiresResult: true}}, TodoID: items[0].ID, Mode: WorkerToolResolutionNormal,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(requests[0], resolved.Names) {
		t.Fatalf("direct provider tools=%v, want=%v", requests[0], resolved.Names)
	}
	for _, forbidden := range []string{"agent", "finish", "request_agent", "approve_plan"} {
		if slices.Contains(requests[0], forbidden) {
			t.Fatalf("direct provider tools=%v contains coordinator-only tool %q", requests[0], forbidden)
		}
	}
	if items[0].Status != TaskDone {
		t.Fatalf("direct task status=%s, want done after typed submit", items[0].Status)
	}
	if typed := c.GetTaskResult(items[0].ID); typed == nil || typed.Source != "submitted" || validateCompletedTaskResult(typed) != nil {
		t.Fatalf("direct typed result missing: items=%#v result=%#v", items, c.GetTaskResult(items[0].ID))
	}
}

func TestHufuLocalSubagentUsesWorkerSurfaceAfterInheritedCoordinatorPreflight(t *testing.T) {
	modelID := "sub-agent-worker-surface-test"
	c, def, capture := newWorkerProviderSurfaceCoordinator(t, modelID, "sub-agent")
	item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: def.Name, Desc: "perform delegated work", Execution: ExecutionContract{RequiresResult: true}}})[0]
	identity := submitResultRuntimeIdentity{RunID: c.executionRunID, TaskID: item.ID, Attempt: 1, Agent: def.Name}
	c.openTaskOccurrence(identity)
	ctx := context.WithValue(oversizedCoordinatorPreflightContext(t, modelID), todoIDKey{}, item.ID)
	ctx = withSubmitResultRuntimeIdentity(ctx, identity)
	if err := c.startProviderExecutionBoundary(ctx); err != nil {
		t.Fatal(err)
	}
	defer c.stopProviderExecutionBoundary()
	task := TaskDef{Agent: def.Name, Goal: "perform delegated work", Execution: ExecutionContract{RequiresResult: true}}
	resolved, err := c.ToolResolver().ResolveTaskTools(t.Context(), def, WorkerToolResolutionRequest{
		Task: task, TodoID: item.ID, Mode: WorkerToolResolutionNormal,
	})
	if err != nil {
		t.Fatalf("ResolveTaskTools: %v", err)
	}
	result, err := NewHufuLocalSubagentProvider(c).RunAttempt(ctx, AttemptRequest{
		RunID: c.executionRunID, TaskID: item.ID, Attempt: 1, Agent: def, Task: task,
		Prompt: task.Goal, ModelID: modelID, MaxSteps: agent.DefaultMaxSteps, Tools: resolved,
	})
	if err != nil {
		t.Fatalf("RunAttempt: %v", err)
	}
	requests := capture.providerRequests()
	if len(requests) == 0 {
		t.Fatal("sub-agent made no provider request")
	}
	want := resolved.Names
	if !slices.Equal(requests[0], want) {
		t.Fatalf("sub-agent provider tools=%v, want=%v", requests[0], want)
	}
	if result.TypedResult == nil || result.TypedResult.Status != TaskResultStatusSuccess {
		t.Fatalf("sub-agent typed result=%#v, want successful submit_result", result.TypedResult)
	}
	for _, forbidden := range []string{"agent", "finish", "request_agent", "approve_plan"} {
		if slices.Contains(requests[0], forbidden) {
			t.Fatalf("sub-agent provider tools=%v contains forbidden tool %q", requests[0], forbidden)
		}
	}
}

func TestHufuLocalSubagentRejectsCallerSurfaceBeforeProviderCall(t *testing.T) {
	modelID := "sub-agent-caller-surface-test"
	c, def, capture := newWorkerProviderSurfaceCoordinator(t, modelID, "sub-agent")
	item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: def.Name, Desc: "perform delegated work", Execution: ExecutionContract{RequiresResult: true}}})[0]
	task := TaskDef{Agent: def.Name, Goal: "perform delegated work", Execution: ExecutionContract{RequiresResult: true}}
	view := tools.NewViewTool(tools.WithAllowedPaths([]string{c.projectDir}))
	_, err := NewHufuLocalSubagentProvider(c).RunAttempt(t.Context(), AttemptRequest{
		RunID: c.executionRunID, TaskID: item.ID, Attempt: 1, Agent: def, Task: task,
		Prompt: task.Goal, ModelID: modelID, MaxSteps: agent.DefaultMaxSteps,
		Tools: ResolvedWorkerTools{Tools: []fantasy.AgentTool{view}, Names: []string{"view"}},
	})
	if err == nil || !strings.Contains(err.Error(), "does not match canonical surface") {
		t.Fatalf("RunAttempt error = %v, want caller-surface denial", err)
	}
	if requests := capture.providerRequests(); len(requests) != 0 {
		t.Fatalf("provider requests = %v, want no model call after boundary denial", requests)
	}
}

func TestPlanReviewerUsesReviewerSurfaceAfterInheritedCoordinatorPreflight(t *testing.T) {
	modelID := "plan-reviewer-surface-test"
	c, _, capture := newWorkerProviderSurfaceCoordinator(t, modelID, "plan-reviewer")
	c.planReviewerModel = modelID
	todoID := "plan-reviewer-todo"
	c.pendingPlans = map[string]*PlanEntry{
		todoID: {TodoID: todoID, Agent: "worker", Goal: "review the plan", PlanText: "1. inspect the change", Status: "submitted"},
	}

	parentCtx := oversizedCoordinatorPreflightContext(t, modelID)
	reviewer, err := c.getPlanReviewer(parentCtx, todoID)
	if err != nil {
		t.Fatalf("getPlanReviewer: %v", err)
	}
	if !reviewer.providerBoundInvocationContext.AdmissionContext.IsBound() {
		t.Fatalf("plan reviewer provider context = %#v, want bound context", reviewer.providerBoundInvocationContext)
	}
	if _, _, execErr, reviewErr := reviewer.review(parentCtx, "1. inspect the change"); execErr != nil || reviewErr != nil {
		t.Fatalf("plan reviewer review: execution error=%v review error=%v", execErr, reviewErr)
	}

	requests := capture.providerRequests()
	if len(requests) != 1 {
		t.Fatalf("plan reviewer provider requests = %v, want exactly one request", requests)
	}
	want := []string{"approve_plan", "reject_plan"}
	if !slices.Equal(requests[0], want) {
		t.Fatalf("plan reviewer provider tools = %v, want %v", requests[0], want)
	}
	for _, forbidden := range []string{"agent", "finish", "load_skill", "submit_result"} {
		if slices.Contains(requests[0], forbidden) {
			t.Fatalf("plan reviewer provider tools=%v contains forbidden tool %q", requests[0], forbidden)
		}
	}
}

func TestHufuLocalRejectsForgedCanonicalAssertionsBeforeProviderCall(t *testing.T) {
	modelID := "sub-agent-canonical-assertion-test"
	c, def, capture := newWorkerProviderSurfaceCoordinator(t, modelID, "sub-agent")

	t.Run("agent definition", func(t *testing.T) {
		item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: def.Name, Desc: "agent assertion"}})[0]
		forged := *def
		forged.Tools = "bash"
		_, err := NewHufuLocalSubagentProvider(c).RunAttempt(t.Context(), AttemptRequest{
			TaskID: item.ID, Attempt: 1, Agent: &forged, Task: taskDefFromTodoItem(item), ModelID: modelID,
		})
		if err == nil || !strings.Contains(err.Error(), "agent assertion") {
			t.Fatalf("RunAttempt error = %v, want canonical agent assertion denial", err)
		}
	})

	base := TodoSpec{
		Agent: def.Name, Desc: "task assertion", PlanFirst: false,
		Execution:      ExecutionContract{RequiresResult: true, ToolSequence: []string{"view", submitResultToolName}},
		WorksetBinding: &WorksetBinding{WorksetID: "canonical-workset", ItemKey: "one"},
	}
	for _, test := range []struct {
		name string
		edit func(*TaskDef)
	}{
		{name: "requires result", edit: func(task *TaskDef) { task.Execution.RequiresResult = false }},
		{name: "sequence", edit: func(task *TaskDef) { task.Execution.ToolSequence = []string{"submit_result"} }},
		{name: "workset", edit: func(task *TaskDef) {
			task.WorksetBinding = &WorksetBinding{WorksetID: "forged-workset", ItemKey: "one"}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			item := c.taskTracker.TodoList().AddBatch([]TodoSpec{base})[0]
			forged := taskDefFromTodoItem(item)
			test.edit(&forged)
			_, err := NewHufuLocalSubagentProvider(c).RunAttempt(t.Context(), AttemptRequest{
				TaskID: item.ID, Attempt: 1, Agent: def, Task: forged, ModelID: modelID,
			})
			if err == nil || !strings.Contains(err.Error(), "task assertion") {
				t.Fatalf("RunAttempt error = %v, want canonical task assertion denial", err)
			}
		})
	}

	if requests := capture.providerRequests(); len(requests) != 0 {
		t.Fatalf("provider requests = %v, want no provider calls", requests)
	}
}

func TestPlanLifecyclePersistsThroughTaskReplay(t *testing.T) {
	initial := todoItemFromSpec(TodoSpec{
		PlanTaskID: "plan-task", PlanFirst: true, Agent: "worker", Desc: "planned work",
	}, "7")
	created, err := json.Marshal(taskTransitionPayload(initial))
	if err != nil {
		t.Fatal(err)
	}
	approved := *initial
	approved.Status = TaskPlanned
	approved.PlanID = initial.ID
	planned, err := json.Marshal(taskTransitionPayload(&approved))
	if err != nil {
		t.Fatal(err)
	}
	replayed := ReduceToTodoList([]RunEvent{
		{Type: "task_created", TaskID: initial.ID, Payload: created},
		{Type: "task_planned", TaskID: initial.ID, Payload: planned},
	})
	if len(replayed) != 1 || !replayed[0].PlanFirst || replayed[0].PlanID != initial.ID {
		t.Fatalf("replayed plan lifecycle = %#v, want plan_first=true plan_id=%q", replayed, initial.ID)
	}
	recovered := taskDefFromTodoItem(replayed[0])
	if !recovered.PlanFirst || recovered.PlanID != initial.ID {
		t.Fatalf("recovered task lifecycle = %#v, want approved plan lifecycle", recovered)
	}

	cleared := *initial
	cleared.Status = TaskPlanned
	cleared.PlanFirst = false
	cleared.PlanID = ""
	clearedPayload, err := json.Marshal(taskTransitionPayload(&cleared))
	if err != nil {
		t.Fatal(err)
	}
	replayed = ReduceToTodoList([]RunEvent{
		{Type: "task_created", TaskID: initial.ID, Payload: created},
		{Type: "task_planned", TaskID: initial.ID, Payload: planned},
		{Type: "task_planned", TaskID: initial.ID, Payload: clearedPayload},
	})
	if len(replayed) != 1 || replayed[0].PlanFirst || replayed[0].PlanID != "" {
		t.Fatalf("replayed cleared plan lifecycle = %#v, want plan_first=false plan_id empty", replayed)
	}
	recovered = taskDefFromTodoItem(replayed[0])
	if recovered.PlanFirst || recovered.PlanID != "" {
		t.Fatalf("recovered cleared task lifecycle = %#v, want cleared plan lifecycle", recovered)
	}

	legacyOmitted := []byte(fmt.Sprintf(`{"id":%q,"status":"planned","plan_first":true,"plan_id":%q}`, initial.ID, initial.ID))
	legacyReplay := ReduceToTodoList([]RunEvent{
		{Type: "task_created", TaskID: initial.ID, Payload: created},
		{Type: "task_planned", TaskID: initial.ID, Payload: legacyOmitted},
		{Type: "task_planned", TaskID: initial.ID, Payload: []byte(fmt.Sprintf(`{"id":%q,"status":"planned"}`, initial.ID))},
	})
	if len(legacyReplay) != 1 || !legacyReplay[0].PlanFirst || legacyReplay[0].PlanID != initial.ID {
		t.Fatalf("legacy omitted plan lifecycle changed state = %#v, want prior lifecycle preserved", legacyReplay)
	}
}
