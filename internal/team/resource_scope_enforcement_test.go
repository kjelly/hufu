package team

import (
	"path/filepath"
	"slices"
	"testing"

	"charm.land/fantasy"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/execution"
	internaltools "github.com/kjelly/hufu/internal/tools"
)

func scopedResolutionFor(tool fantasy.AgentTool) ResolvedWorkerTools {
	name := tool.Info().Name
	return ResolvedWorkerTools{
		Names: []string{name}, AuthorizedNames: []string{name},
		WorkspaceScopeDescriptors: map[string]internaltools.ToolWorkspaceScopeDescriptor{
			name: internaltools.DescribeToolWorkspaceScope(tool),
		},
	}
}

func TestResolvedWorkerSurfaceCarriesScopeDescriptors(t *testing.T) {
	c, root := scopedResourceCoordinator(t)
	c.coreTools = agent.BuildAllAgentTools(root, internaltools.WithAllowedPaths([]string{root}))
	c.coreTools = append(c.coreTools,
		&requestAgentTool{coordinator: c},
		&todoTool{coordinator: c},
		&canonicalMemoryQueryTool{coordinator: c},
		&teamInfoTool{coordinator: c},
	)
	todoID := resolverTodoID(t, c, "scoped-surface")
	task := TaskDef{
		Agent: "worker", SideEffect: SideEffectWorkspaceWrite,
		Execution:               ExecutionContract{RequiresResult: true},
		WorksetBinding:          &WorksetBinding{TouchedPaths: []string{"docs/a.md"}},
		ResolvedExecutionTarget: execution.ExecutionTarget{Backend: "local", Model: "test"},
	}
	resolved, err := c.ToolResolver().ResolveTaskTools(t.Context(), c.session.Agents["worker"], WorkerToolResolutionRequest{
		Task: task, TodoID: todoID, Mode: WorkerToolResolutionNormal,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range resolved.AuthorizedNames {
		if _, ok := resolved.WorkspaceScopeDescriptors[name]; !ok {
			t.Fatalf("resolved tool %q has no scope descriptor", name)
		}
	}
	for _, name := range []string{submitResultToolName} {
		descriptor, ok := resolved.WorkspaceScopeDescriptors[name]
		if !ok || descriptor.MayReadWorkspace || descriptor.MayWriteWorkspace {
			t.Fatalf("protocol/observation descriptor %q = %#v (present=%v, surface=%v)", name, descriptor, ok, resolved.Names)
		}
	}
	for name, tool := range map[string]fantasy.AgentTool{
		"request_agent": &requestAgentTool{coordinator: c},
		"todo":          &todoTool{coordinator: c},
		"memory_query":  &canonicalMemoryQueryTool{coordinator: c},
		"team_info":     &teamInfoTool{coordinator: c},
	} {
		descriptor := internaltools.DescribeToolWorkspaceScope(tool)
		if descriptor.MayReadWorkspace || descriptor.MayWriteWorkspace {
			t.Fatalf("protocol/observation descriptor %q = %#v", name, descriptor)
		}
	}
	item := &TodoItem{ID: todoID, Agent: "worker", SideEffect: task.SideEffect, WorksetBinding: task.WorksetBinding}
	snapshot, err := c.resolveNewTaskResourceScope(task, item, resolved)
	if err != nil {
		t.Fatal(err)
	}
	if !snapshot.BoundedReadScope || !snapshot.BoundedWriteScope {
		t.Fatalf("real resolved worker surface did not enable bounded scope: %#v", snapshot)
	}
}

func scopedResourceCoordinator(t *testing.T) (*Coordinator, string) {
	t.Helper()
	c := newDirectTypedCoordinator(t, "view,write", nil, nil)
	root := c.projectDir
	c.allowedPaths = []string{root}
	registry := NewExecutionRegistry()
	if err := registry.Register(fakeLanguageModelBackend{fakeExecutionBackend{name: "local", kind: execution.BackendKindLLM}}); err != nil {
		t.Fatal(err)
	}
	if err := registry.Register(fakeExecutionBackend{name: "external", kind: execution.BackendKindAgent}); err != nil {
		t.Fatal(err)
	}
	c.SetExecutionRegistry(registry)
	return c, root
}

func TestResolveNewTaskResourceScopeEnablesOnlyEnforcedLocalSurface(t *testing.T) {
	c, root := scopedResourceCoordinator(t)
	binding := &WorksetBinding{TouchedPaths: []string{"docs/a.md"}}
	todo := &TodoItem{ID: "1", Agent: "worker", SideEffect: SideEffectWorkspaceWrite, WorksetBinding: binding}
	task := TaskDef{
		Agent: "worker", SideEffect: SideEffectWorkspaceWrite, WorksetBinding: binding,
		ResolvedExecutionTarget: execution.ExecutionTarget{Backend: "local", Model: "test"},
	}
	write := internaltools.NewWriteTool(internaltools.WithWorkDir(root), internaltools.WithAllowedPaths([]string{root}))
	snapshot, err := c.resolveNewTaskResourceScope(task, todo, scopedResolutionFor(write))
	if err != nil {
		t.Fatal(err)
	}
	if !snapshot.BoundedReadScope || !snapshot.BoundedWriteScope || snapshot.Source != resourceScopeSourceAuthoredWorkset || !slices.Equal(snapshot.WritePaths, []string{"docs/a.md"}) {
		t.Fatalf("bounded snapshot = %#v", snapshot)
	}
	want, _ := NewWorkspacePathResourceClaim("docs/a.md", ResourceWrite)
	if !slices.Contains(snapshot.Claims, want) {
		t.Fatalf("claims = %#v, want %#v", snapshot.Claims, want)
	}

	unsupported := scopedResolutionFor(internaltools.NewBashTool())
	fallback, err := c.resolveNewTaskResourceScope(task, todo, unsupported)
	if err != nil {
		t.Fatal(err)
	}
	rootClaim, _ := NewWorkspacePathResourceClaim(".", ResourceExclusive)
	if fallback.BoundedWriteScope || fallback.Source != resourceScopeSourceRuntimeFallback || !slices.Contains(fallback.Claims, rootClaim) {
		t.Fatalf("unsupported fallback = %#v", fallback)
	}

	externalTask := task
	externalTask.ResolvedExecutionTarget = execution.ExecutionTarget{Backend: "external", Model: "test"}
	external, err := c.resolveNewTaskResourceScope(externalTask, todo, scopedResolutionFor(write))
	if err != nil {
		t.Fatal(err)
	}
	if external.BoundedWriteScope || !slices.Contains(external.Claims, rootClaim) {
		t.Fatalf("external fallback = %#v", external)
	}
}

func TestResolveNewTaskResourceScopeHonorsConfiguredCeiling(t *testing.T) {
	c, root := scopedResourceCoordinator(t)
	c.allowedPaths = []string{filepath.Join(root, "internal")}
	binding := &WorksetBinding{TouchedPaths: []string{"docs/a.md"}}
	todo := &TodoItem{ID: "1", Agent: "worker", SideEffect: SideEffectNone, WorksetBinding: binding}
	task := TaskDef{Agent: "worker", SideEffect: SideEffectNone, WorksetBinding: binding, ResolvedExecutionTarget: execution.ExecutionTarget{Backend: "local", Model: "test"}}
	view := internaltools.NewViewTool(internaltools.WithWorkDir(root), internaltools.WithAllowedPaths(c.allowedPaths))
	if _, err := c.resolveNewTaskResourceScope(task, todo, scopedResolutionFor(view)); err == nil {
		t.Fatal("disjoint configured ceiling accepted")
	}
}

func TestDerivedDisjointWriterScopesRemainConcurrent(t *testing.T) {
	c, root := scopedResourceCoordinator(t)
	write := scopedResolutionFor(internaltools.NewWriteTool(internaltools.WithWorkDir(root), internaltools.WithAllowedPaths([]string{root})))
	tasks := []TaskDef{
		{Agent: "worker", SideEffect: SideEffectWorkspaceWrite, WorksetBinding: &WorksetBinding{TouchedPaths: []string{"docs/"}}, ResolvedExecutionTarget: execution.ExecutionTarget{Backend: "local", Model: "test"}},
		{Agent: "worker", SideEffect: SideEffectWorkspaceWrite, WorksetBinding: &WorksetBinding{TouchedPaths: []string{"internal/"}}, ResolvedExecutionTarget: execution.ExecutionTarget{Backend: "local", Model: "test"}},
	}
	envelopes := make([]TaskExecutionEnvelope, len(tasks))
	for i := range tasks {
		item := &TodoItem{ID: string(rune('1' + i)), Agent: "worker", SideEffect: tasks[i].SideEffect, WorksetBinding: tasks[i].WorksetBinding}
		snapshot, err := c.resolveNewTaskResourceScope(tasks[i], item, write)
		if err != nil {
			t.Fatal(err)
		}
		effective, err := effectiveScopeFromSnapshot(snapshot, root)
		if err != nil {
			t.Fatal(err)
		}
		envelopes[i] = TaskExecutionEnvelope{ResourceScope: effective}
	}
	normalized := serializeConflictingMutationTasks(tasks, envelopes)
	if len(normalized[1].DependsOn) != 0 {
		t.Fatalf("disjoint bounded writers were serialized: %v", normalized[1].DependsOn)
	}
}

func TestInstallTaskExecutionPathScopeUsesDetachedEnvelope(t *testing.T) {
	root := t.TempDir()
	envelope := TaskExecutionEnvelope{
		ResourceScopeDigest: "scope-digest",
		ResourceScope: EffectiveTaskResourceScope{
			AllowedReadPaths: []string{filepath.Join(root, "docs") + string(filepath.Separator)},
			BoundedReadScope: true,
		},
	}
	ctx, err := installTaskExecutionPathScope(withTaskExecutionEnvelope(t.Context(), envelope))
	if err != nil {
		t.Fatal(err)
	}
	scope, ok := ctx.Value(internaltools.AgentTaskPathScopeKey).(internaltools.AgentTaskPathScope)
	if !ok || scope.Digest != envelope.ResourceScopeDigest || !scope.ReadBounded {
		t.Fatalf("installed scope = %#v", scope)
	}
	scope.ReadPaths[0] = "changed"
	bound, _ := taskExecutionEnvelopeFromContext(ctx)
	if bound.ResourceScope.AllowedReadPaths[0] == "changed" {
		t.Fatal("installed path scope aliases envelope")
	}
}
