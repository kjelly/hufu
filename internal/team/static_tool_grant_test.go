package team

import (
	"context"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"charm.land/fantasy"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/tools"
)

// staticGrantFixture freezes a worker occurrence under creationTools, then
// lets the test change the live agent definition.
type staticGrantFixture struct {
	c      *Coordinator
	def    *agent.AgentDef
	todoID string
	task   TaskDef
	events *[]StatusEvent
}

func newStaticGrantFixture(t *testing.T, creationTools string, denied []string) staticGrantFixture {
	t.Helper()
	c := newDirectTypedCoordinator(t, creationTools, nil, denied)
	def := c.agentPool.(*mockAgentPool).resolveDef
	var mu sync.Mutex
	events := &[]StatusEvent{}
	c.SetStatusReporter(func(event StatusEvent) {
		mu.Lock()
		defer mu.Unlock()
		*events = append(*events, event)
	})
	task := TaskDef{Agent: "worker", Execution: ExecutionContract{RequiresResult: true}}
	f := staticGrantFixture{c: c, def: def, task: task, events: events}
	f.todoID = f.freezeTodo(t)
	return f
}

// freezeTodo creates a todo whose snapshot is frozen from the current live
// configuration, as task creation does.
func (f staticGrantFixture) freezeTodo(t *testing.T) string {
	t.Helper()
	snapshot, err := f.c.resolveNewTaskToolAuthorization(t.Context(), f.task, f.def)
	if err != nil {
		t.Fatal(err)
	}
	return f.c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "static-grant", DynamicToolAuthorization: snapshot}})[0].ID
}

func (f staticGrantFixture) resolve(t *testing.T, mode WorkerToolResolutionMode) (ResolvedWorkerTools, error) {
	t.Helper()
	return f.c.ToolResolver().ResolveTaskTools(t.Context(), f.def, WorkerToolResolutionRequest{Task: f.task, TodoID: f.todoID, Mode: mode})
}

func (f staticGrantFixture) narrowingMessages() []string {
	var messages []string
	for _, event := range *f.events {
		if event.Type == string(EventStaticToolGrantNarrowed) {
			messages = append(messages, event.Message)
		}
	}
	return messages
}

func TestWorkerGrantIsIndependentOfCoordinatorSurface(t *testing.T) {
	f := newStaticGrantFixture(t, "view,bash", nil)
	if coordinatorCoreToolNames["bash"] {
		t.Fatal("the coordinator surface unexpectedly includes bash")
	}
	resolved, err := f.resolve(t, WorkerToolResolutionNormal)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(resolved.Names, "bash") || !slices.Contains(resolved.AuthorizedNames, "bash") {
		t.Fatalf("worker lost its own bash grant: %v", resolved.Names)
	}
	if ceiling := f.c.todoItemByID(f.todoID).DynamicToolAuthorization.StaticToolCeiling; !slices.Contains(ceiling, "bash") || slices.Contains(ceiling, submitResultToolName) {
		t.Fatalf("ceiling = %v", ceiling)
	}
}

func TestStaticToolCeilingNarrowsButNeverWidens(t *testing.T) {
	cases := []struct {
		name            string
		creationTools   string
		creationDenied  []string
		liveTools       string
		liveDenied      []string
		wantAbsent      string
		wantPresent     string
		wantIgnored     string
		wantUnavailable string
	}{
		{name: "tool added after creation", creationTools: "view", liveTools: "view,bash", wantAbsent: "bash", wantPresent: "view", wantIgnored: "bash"},
		{name: "tool removed after creation", creationTools: "view,write", liveTools: "view", wantAbsent: "write", wantPresent: "view", wantUnavailable: "write"},
		{name: "denial added after creation", creationTools: "view,bash", liveTools: "view,bash", liveDenied: []string{"bash"}, wantAbsent: "bash", wantPresent: "view", wantUnavailable: "bash"},
		{name: "denial removed after creation", creationTools: "view,bash", creationDenied: []string{"bash"}, liveTools: "view,bash", wantAbsent: "bash", wantPresent: "view", wantIgnored: "bash"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newStaticGrantFixture(t, tc.creationTools, tc.creationDenied)
			f.def.Tools = tc.liveTools
			f.c.session.Config.ToolsDenied = tc.liveDenied
			for range 2 {
				resolved, err := f.resolve(t, WorkerToolResolutionNormal)
				if err != nil {
					t.Fatal(err)
				}
				if slices.Contains(resolved.Names, tc.wantAbsent) || !slices.Contains(resolved.Names, tc.wantPresent) {
					t.Fatalf("surface = %v, want %q absent and %q present", resolved.Names, tc.wantAbsent, tc.wantPresent)
				}
			}
			messages := f.narrowingMessages()
			if len(messages) != 1 {
				t.Fatalf("narrowing status lines = %d, want 1: %v", len(messages), messages)
			}
			if !strings.Contains(messages[0], "ignored=["+tc.wantIgnored+"]") || !strings.Contains(messages[0], "unavailable=["+tc.wantUnavailable+"]") {
				t.Fatalf("narrowing message = %q", messages[0])
			}
		})
	}
}

func TestStaticToolCeilingIsPhaseFree(t *testing.T) {
	f := newStaticGrantFixture(t, "view,bash", nil)
	// Rebuild the ceiling while the workflow is outside EXECUTE.
	f.c.phaseWorkflow = &runtimeWorkflow{enabled: true, state: PhasePrepare, workspace: RuntimeWorkspace{Root: filepath.Join(f.c.session.Workspace, "runtime")}}
	f.todoID = f.freezeTodo(t)
	if ceiling := f.c.todoItemByID(f.todoID).DynamicToolAuthorization.StaticToolCeiling; !slices.Contains(ceiling, "bash") {
		t.Fatalf("ceiling built outside EXECUTE dropped bash: %v", ceiling)
	}
	planning, err := f.resolve(t, WorkerToolResolutionNormal)
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(planning.Names, "bash") || len(f.narrowingMessages()) != 0 {
		t.Fatalf("outside EXECUTE: surface=%v narrowing=%v", planning.Names, f.narrowingMessages())
	}
	f.c.phaseWorkflow.state = PhaseExecute
	executing, err := f.resolve(t, WorkerToolResolutionNormal)
	if err != nil || !slices.Contains(executing.Names, "bash") {
		t.Fatalf("EXECUTE surface=%v err=%v", executing.Names, err)
	}
}

func TestStaticToolCeilingLifecycleModes(t *testing.T) {
	f := newStaticGrantFixture(t, "view", nil)
	f.def.Tools = "view,bash"
	repair, err := f.resolve(t, WorkerToolResolutionResultRepair)
	if err != nil || !slices.Equal(repair.Names, []string{submitResultToolName}) {
		t.Fatalf("result repair surface=%v err=%v", repair.Names, err)
	}
	prospective := cloneTodoItem(f.c.todoItemByID(f.todoID))
	if _, err := f.c.ToolResolver().ResolveTaskTools(t.Context(), f.def, WorkerToolResolutionRequest{Task: f.task, TodoID: f.todoID, Mode: WorkerToolResolutionNormal, ProspectiveTodo: prospective}); err != nil {
		t.Fatal(err)
	}
	if len(f.narrowingMessages()) != 0 {
		t.Fatalf("result repair or prospective resolution recorded narrowing: %v", f.narrowingMessages())
	}

	sequenced := f.task
	sequenced.Execution.ToolSequence = []string{"bash", submitResultToolName}
	if _, err := f.c.ToolResolver().ResolveTaskTools(t.Context(), f.def, WorkerToolResolutionRequest{Task: sequenced, TodoID: f.todoID, Mode: WorkerToolResolutionNormal}); err == nil {
		t.Fatal("a closed sequence needing a tool outside the ceiling resolved")
	}
}

func TestStaticToolGrantNamesFiltersAgentMCPKeys(t *testing.T) {
	c := newDirectTypedCoordinator(t, "view", nil, []string{"blocked"})
	def := &agent.AgentDef{Name: "worker", Role: "worker", Tools: "view", MCPTools: map[string]agent.MCPToolConfig{
		"deploy": {}, "blocked": {}, "finish": {},
	}}
	names := c.staticToolGrantNames(def, TaskDef{})
	if !slices.Contains(names, "deploy") || slices.Contains(names, "blocked") || slices.Contains(names, "finish") || !slices.IsSorted(names) {
		t.Fatalf("static grant names = %v", names)
	}
	snapshot := &DynamicToolAuthorizationSnapshot{StaticToolCeiling: []string{"deploy"}}
	kept := filterToolsByStaticCeiling([]fantasy.AgentTool{&dynamicTestTool{info: fantasy.ToolInfo{Name: "deploy"}}, &dynamicTestTool{info: fantasy.ToolInfo{Name: "later"}}}, snapshot)
	if len(kept) != 1 || kept[0].Info().Name != "deploy" {
		t.Fatalf("agent MCP tools after ceiling = %v", agentToolNames(kept))
	}
}

func TestStaticToolCeilingSnapshotRules(t *testing.T) {
	empty := newEmptyDynamicToolAuthorizationSnapshot()
	if err := validateDynamicToolAuthorizationSnapshot(empty); err != nil {
		t.Fatalf("empty snapshot invalid: %v", err)
	}
	// Adding the ceiling did not change catalog digests (values from bba44b6).
	if empty.FrozenCatalogDigest != "6c2207c16164ee645c66b8241ab21032ad5214488290662cfc54aeae6973da82" ||
		frozenDynamicCatalogDigest([]FrozenDynamicToolTarget{{Name: "server__tool", DescriptorSHA256: "aa"}}) != "ff029d608c91184a473d5f0b5c949e5592ac11398d9806c8e6683a1f5e756e52" {
		t.Fatal("frozen catalog digest changed")
	}
	for name, mutate := range map[string]func(*DynamicToolAuthorizationSnapshot){
		"version 1":       func(s *DynamicToolAuthorizationSnapshot) { s.Version = 1 },
		"missing ceiling": func(s *DynamicToolAuthorizationSnapshot) { s.StaticToolCeiling = nil },
		"unsorted":        func(s *DynamicToolAuthorizationSnapshot) { s.StaticToolCeiling = []string{"view", "bash"} },
		"duplicate":       func(s *DynamicToolAuthorizationSnapshot) { s.StaticToolCeiling = []string{"bash", "bash"} },
	} {
		t.Run(name, func(t *testing.T) {
			snapshot := cloneDynamicToolAuthorizationSnapshot(empty)
			mutate(snapshot)
			if err := validateDynamicToolAuthorizationSnapshot(snapshot); err == nil || !strings.Contains(err.Error(), "dynamic_snapshot_invalid") {
				t.Fatalf("error = %v", err)
			}
		})
	}
	withCeiling := cloneDynamicToolAuthorizationSnapshot(empty)
	withCeiling.StaticToolCeiling = []string{"bash"}
	clone := cloneDynamicToolAuthorizationSnapshot(withCeiling)
	clone.StaticToolCeiling[0] = "write"
	if withCeiling.StaticToolCeiling[0] != "bash" {
		t.Fatal("clone shares the ceiling backing array")
	}

	item := &TodoItem{ID: "digest", Agent: "worker", Desc: "digest", DynamicToolAuthorization: withCeiling}
	first, err := newTaskOccurrenceProjection(item)
	if err != nil {
		t.Fatal(err)
	}
	item.DynamicToolAuthorization = clone
	second, err := newTaskOccurrenceProjection(item)
	if err != nil {
		t.Fatal(err)
	}
	firstDigest, _ := decisionOccurrenceInputDigest(first)
	secondDigest, _ := decisionOccurrenceInputDigest(second)
	if firstDigest == secondDigest {
		t.Fatal("a different ceiling did not change the occurrence digest")
	}
}

func TestDeclaredStepObeysStaticToolCeiling(t *testing.T) {
	f := newStaticGrantFixture(t, "view", nil)
	f.def.Tools = "view,bash"
	runner := &coordinatorDeclaredToolRunner{c: f.c}
	_, err := runner.RunStructuredStep(t.Context(), StructuredStepRequest{
		TaskID: f.todoID, Attempt: 1,
		Step:          ExecutionStep{ID: "probe", Tool: "bash", Effect: ExecutionEffectRead},
		ResolvedInput: map[string]any{"command": "true"},
	})
	if err == nil || !strings.Contains(err.Error(), `requires unavailable tool "bash"`) {
		t.Fatalf("declared step error = %v, want the frozen ceiling to exclude bash", err)
	}
}

type toolSurfaceCaptureAgent struct {
	directTerminationAgent
	mu   *sync.Mutex
	seen *[][]string
}

func (a toolSurfaceCaptureAgent) Stream(ctx context.Context, call fantasy.AgentStreamCall) (*fantasy.AgentResult, error) {
	a.mu.Lock()
	*a.seen = append(*a.seen, slices.Clone(tools.GetToolsAllowed(ctx)))
	a.mu.Unlock()
	return a.directTerminationAgent.Stream(ctx, call)
}

func TestRetryTaskKeepsFrozenStaticToolCeiling(t *testing.T) {
	var mu sync.Mutex
	var seen [][]string
	c := newDirectTerminationCoordinator(t, directTerminationAgent{})
	c.workerAgentOverride = directTerminationAgentWithResult{worker: toolSurfaceCaptureAgent{mu: &mu, seen: &seen}, coordinator: c}
	c.coreTools = workerInvariantCoreTools(t)
	def := c.agentPool.(*mockAgentPool).resolveDef
	def.Tools = "view"
	task := TaskDef{Agent: "worker", Goal: "retry with a frozen grant"}
	snapshot, err := c.resolveNewTaskToolAuthorization(t.Context(), task, def)
	if err != nil {
		t.Fatal(err)
	}
	c.taskTracker.TodoList().Restore([]*TodoItem{{
		ID: "77", Agent: "worker", Desc: task.Goal, Goal: task.Goal,
		Status: TaskError, Recovery: RecoveryRetry, DynamicToolAuthorization: snapshot,
	}})
	def.Tools = "view,bash"

	if _, err := c.RetryTask(context.Background(), "77"); err != nil {
		t.Fatalf("RetryTask: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) == 0 {
		t.Fatal("the retried worker never ran")
	}
	for _, allowed := range seen {
		if slices.Contains(allowed, "bash") || !slices.Contains(allowed, "view") {
			t.Fatalf("retried attempt allowlist = %v, want the frozen ceiling without bash", allowed)
		}
	}
}
