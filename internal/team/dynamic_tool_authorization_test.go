package team

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"charm.land/fantasy"

	internalmcp "github.com/kjelly/hufu/internal/mcp"
)

type dynamicTestTool struct {
	info fantasy.ToolInfo
	opts fantasy.ProviderOptions
}

func (t dynamicTestTool) Info() fantasy.ToolInfo                           { return t.info }
func (t dynamicTestTool) ProviderOptions() fantasy.ProviderOptions         { return t.opts }
func (t *dynamicTestTool) SetProviderOptions(opts fantasy.ProviderOptions) { t.opts = opts }
func (dynamicTestTool) Run(_ context.Context, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
	return fantasy.NewTextResponse("ok"), nil
}

func TestDynamicToolAuthorizationSnapshotValidationAndClone(t *testing.T) {
	targets := []FrozenDynamicToolTarget{
		{Name: "a__one", DescriptorSHA256: strings.Repeat("a", 64)},
		{Name: "z__two", DescriptorSHA256: strings.Repeat("b", 64)},
	}
	snapshot := &DynamicToolAuthorizationSnapshot{Version: dynamicToolAuthorizationSnapshotVersion, Targets: targets}
	snapshot.FrozenCatalogDigest = frozenDynamicCatalogDigest(targets)
	if err := validateDynamicToolAuthorizationSnapshot(snapshot); err != nil {
		t.Fatalf("validate snapshot: %v", err)
	}
	cloned := cloneDynamicToolAuthorizationSnapshot(snapshot)
	cloned.Targets[0].Name = "changed"
	if snapshot.Targets[0].Name != "a__one" {
		t.Fatal("clone shares target backing array")
	}
	invalid := cloneDynamicToolAuthorizationSnapshot(snapshot)
	invalid.Targets[0], invalid.Targets[1] = invalid.Targets[1], invalid.Targets[0]
	if err := validateDynamicToolAuthorizationSnapshot(invalid); err == nil || !strings.Contains(err.Error(), "strictly ascending") {
		t.Fatalf("unordered snapshot error = %v", err)
	}
}

func TestDynamicToolAuthorizationProjectionRoundTrip(t *testing.T) {
	snapshot := newEmptyDynamicToolAuthorizationSnapshot()
	spec := TodoSpec{Agent: "worker", Desc: "inspect", DynamicToolAuthorization: snapshot}
	item := todoItemFromSpec(spec, "1")
	projection, err := newTaskOccurrenceProjection(item)
	if err != nil {
		t.Fatalf("new occurrence projection: %v", err)
	}
	if projection.DynamicToolAuthorization == nil || projection.DynamicToolAuthorization.FrozenCatalogDigest != snapshot.FrozenCatalogDigest {
		t.Fatalf("projection snapshot = %#v", projection.DynamicToolAuthorization)
	}
	payload, err := json.Marshal(item)
	if err != nil {
		t.Fatalf("marshal item: %v", err)
	}
	replayed := ReduceToTodoList([]RunEvent{{Type: string(EventTaskCreated), TaskID: item.ID, Payload: payload}})
	if len(replayed) != 1 || replayed[0].DynamicToolAuthorization == nil {
		t.Fatalf("replayed snapshot = %#v", replayed)
	}
	replayed[0].DynamicToolAuthorization.Targets = append(replayed[0].DynamicToolAuthorization.Targets, FrozenDynamicToolTarget{Name: "mutated"})
	if len(item.DynamicToolAuthorization.Targets) != 0 {
		t.Fatal("event replay shares snapshot backing array")
	}
}

func TestResolvedWorkerToolsKeepDirectSurfaceAndLogicalDigest(t *testing.T) {
	c := newDirectTypedCoordinator(t, "view", nil, nil)
	todoID := resolverTodoID(t, c, "dynamic-surface")
	item := c.todoItemByID(todoID)
	item.DynamicToolAuthorization = newEmptyDynamicToolAuthorizationSnapshot()
	resolved, err := c.ToolResolver().ResolveTaskTools(t.Context(), c.session.Agents["worker"], WorkerToolResolutionRequest{
		Task: TaskDef{Agent: "worker"}, TodoID: todoID, Mode: WorkerToolResolutionNormal,
	})
	if err != nil {
		t.Fatalf("resolve tools: %v", err)
	}
	if !slices.Equal(resolved.Names, resolved.AuthorizedNames) {
		t.Fatalf("provider names %v != authorized names %v", resolved.Names, resolved.AuthorizedNames)
	}
	if resolved.ProviderSurfaceDigest == "" || resolved.LogicalToolsetDigest == "" {
		t.Fatalf("missing digests: %#v", resolved)
	}
	if resolved.DynamicAuthorization == item.DynamicToolAuthorization {
		t.Fatal("resolved tools share durable snapshot pointer")
	}
}

func TestFrozenDynamicAuthorizationCannotWiden(t *testing.T) {
	descriptor := internalmcp.MCPTool{Name: "server__allowed", ServerName: "server", OrigName: "allowed", InputSchema: map[string]any{"type": "object"}}
	fingerprint, err := internalmcp.MCPToolDescriptorSHA256(descriptor)
	if err != nil {
		t.Fatalf("descriptor hash: %v", err)
	}
	snapshot := &DynamicToolAuthorizationSnapshot{Version: dynamicToolAuthorizationSnapshotVersion, Targets: []FrozenDynamicToolTarget{{Name: descriptor.Name, DescriptorSHA256: fingerprint}}}
	snapshot.FrozenCatalogDigest = frozenDynamicCatalogDigest(snapshot.Targets)
	allowed := &dynamicTestTool{info: fantasy.ToolInfo{Name: descriptor.Name}}
	newer := &dynamicTestTool{info: fantasy.ToolInfo{Name: "server__new"}}
	filtered, unavailable, err := filterManagerToolsThroughSnapshot(snapshot, []internalmcp.MCPTool{descriptor, {Name: "server__new", ServerName: "server", OrigName: "new", InputSchema: map[string]any{"type": "object"}}}, []fantasy.AgentTool{allowed, newer}, nil)
	if err != nil {
		t.Fatalf("filter frozen tools: %v", err)
	}
	if len(filtered) != 1 || filtered[0].Info().Name != descriptor.Name {
		t.Fatalf("filtered tools = %#v", agentToolNames(filtered))
	}
	if len(unavailable) != 0 {
		t.Fatalf("unavailable targets = %#v", unavailable)
	}
}

func TestFrozenDynamicAuthorizationReportsChangedOptionalTarget(t *testing.T) {
	descriptor := internalmcp.MCPTool{Name: "server__tool", ServerName: "server", OrigName: "tool", Description: "old", InputSchema: map[string]any{"type": "object"}}
	fingerprint, err := internalmcp.MCPToolDescriptorSHA256(descriptor)
	if err != nil {
		t.Fatalf("descriptor hash: %v", err)
	}
	snapshot := &DynamicToolAuthorizationSnapshot{Version: dynamicToolAuthorizationSnapshotVersion, Targets: []FrozenDynamicToolTarget{{Name: descriptor.Name, DescriptorSHA256: fingerprint}}}
	snapshot.FrozenCatalogDigest = frozenDynamicCatalogDigest(snapshot.Targets)
	descriptor.Description = "new"
	tool := &dynamicTestTool{info: fantasy.ToolInfo{Name: descriptor.Name}}
	filtered, unavailable, err := filterManagerToolsThroughSnapshot(snapshot, []internalmcp.MCPTool{descriptor}, []fantasy.AgentTool{tool}, nil)
	if err != nil {
		t.Fatalf("filter changed target: %v", err)
	}
	if len(filtered) != 0 || len(unavailable) != 1 || unavailable[0].Reason != "descriptor_changed" {
		t.Fatalf("filtered=%v unavailable=%#v", agentToolNames(filtered), unavailable)
	}
	if _, _, err := filterManagerToolsThroughSnapshot(snapshot, []internalmcp.MCPTool{descriptor}, []fantasy.AgentTool{tool}, []string{descriptor.Name}); err == nil {
		t.Fatal("closed sequence accepted changed target")
	}
}

func TestTaskCacheUsesLogicalToolsetAndResourceScopeDigests(t *testing.T) {
	coordinator := &Coordinator{}
	policy := &defaultPolicyEngine{c: coordinator}
	cache := newDefaultTaskCache(taskCacheDependencies{PolicyEngine: func() PolicyEngine { return policy }, Identity: func(req TaskCacheLookupRequest) CacheIdentity {
		return CacheIdentity{ToolRegistryVersion: req.LogicalToolsetDigest, ResourceScopeDigest: req.ResourceScopeDigest}
	}})
	cache.Store(TaskCacheStoreRequest{
		AgentKey: "worker", Task: "inspect", Output: "cached",
		LogicalToolsetDigest: "logical-a", ResourceScopeDigest: "scope-a",
	})
	base := TaskCacheLookupRequest{
		Scope: TaskCacheLookupExecution, AgentKey: "worker", Task: "inspect",
		LogicalToolsetDigest: "logical-a", ResourceScopeDigest: "scope-a",
	}
	if result, ok := cache.Lookup(t.Context(), base); !ok || result.Output != "cached" {
		t.Fatalf("matching digest lookup = (%#v, %t)", result, ok)
	}
	logicalChanged := base
	logicalChanged.LogicalToolsetDigest = "logical-b"
	if _, ok := cache.Lookup(t.Context(), logicalChanged); ok {
		t.Fatal("logical toolset change reused stale result")
	}
	scopeChanged := base
	scopeChanged.ResourceScopeDigest = "scope-b"
	if _, ok := cache.Lookup(t.Context(), scopeChanged); ok {
		t.Fatal("resource scope change reused stale result")
	}
	legacy := base
	legacy.LogicalToolsetDigest = ""
	legacy.ResourceScopeDigest = ""
	if _, ok := cache.Lookup(t.Context(), legacy); ok {
		t.Fatal("legacy empty digest reused a new cache entry")
	}
}
