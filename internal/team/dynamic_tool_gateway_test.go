package team

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"

	"charm.land/fantasy"

	"github.com/kjelly/hufu/internal/agent"
	internalmcp "github.com/kjelly/hufu/internal/mcp"
	"github.com/kjelly/hufu/internal/tools"
)

type gatewayTestExecutor struct {
	calls int
	name  string
	input string
	text  string
	err   error
	isErr bool
}

func (e *gatewayTestExecutor) ExecuteAuthorizedTool(_ context.Context, name, _ string, input string) (string, bool, error) {
	e.calls++
	e.name, e.input = name, input
	return e.text, e.isErr, e.err
}

func gatewayTestTarget() DynamicToolTarget {
	return DynamicToolTarget{
		Name: "github__get_issue", Kind: "mcp", Server: "github", NativeName: "get_issue",
		Description: "Get one GitHub pull request issue", DescriptorSHA256: strings.Repeat("a", 64),
		Required: []string{"owner", "issue_number", "labels", "metadata"},
		InputSchema: map[string]any{
			"type": "object", "additionalProperties": false,
			"required": []any{"owner", "issue_number", "labels", "metadata"},
			"properties": map[string]any{
				"owner":        map[string]any{"type": "string", "enum": []any{"kjelly"}},
				"issue_number": map[string]any{"type": "integer", "const": json.Number("1.0")},
				"labels":       map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
				"metadata": map[string]any{"type": "object", "additionalProperties": false, "required": []any{"open"},
					"properties": map[string]any{"open": map[string]any{"type": "boolean"}}},
			},
		},
	}
}

func gatewayTestContext(t *testing.T, c *Coordinator, names ...string) context.Context {
	t.Helper()
	ctx := context.WithValue(t.Context(), tools.UnattendedKey, true)
	ctx = context.WithValue(ctx, tools.AgentNameKey, "worker")
	ctx = context.WithValue(ctx, tools.AgentToolsAllowedKey, names)
	return ctx
}

func TestDynamicToolGatewaySchemaIsInventoryIndependent(t *testing.T) {
	left := newDynamicToolGateway(nil, &gatewayTestExecutor{}, []DynamicToolTarget{gatewayTestTarget()})
	rightTarget := gatewayTestTarget()
	rightTarget.Name = "k8s__pods"
	right := newDynamicToolGateway(nil, &gatewayTestExecutor{}, []DynamicToolTarget{gatewayTestTarget(), rightTarget})
	leftHash, err := providerToolSchemaSHA256(left.Info())
	if err != nil {
		t.Fatal(err)
	}
	rightHash, err := providerToolSchemaSHA256(right.Info())
	if err != nil || leftHash != rightHash {
		t.Fatalf("gateway schema hashes = %q, %q, err=%v", leftHash, rightHash, err)
	}
	if !slices.Equal(left.Info().Required, []string{"action"}) || left.Info().Name != dynamicToolGatewayName {
		t.Fatalf("gateway info = %#v", left.Info())
	}
}

func TestDynamicToolGatewaySearchAndInspectExposeOnlyAttemptCatalog(t *testing.T) {
	target := gatewayTestTarget()
	gateway := newDynamicToolGateway(nil, &gatewayTestExecutor{}, []DynamicToolTarget{target})
	search, err := gateway.Run(t.Context(), fantasy.ToolCall{Input: `{"action":"search","query":"pull request"}`})
	if err != nil || search.IsError || !strings.Contains(search.Content, target.Name) || strings.Contains(search.Content, "input_schema") {
		t.Fatalf("search = %#v, %v", search, err)
	}
	inspect, err := gateway.Run(t.Context(), fantasy.ToolCall{Input: `{"action":"inspect","target":"github__get_issue"}`})
	if err != nil || inspect.IsError || !strings.Contains(inspect.Content, `"input_schema"`) || !strings.Contains(inspect.Content, target.DescriptorSHA256) {
		t.Fatalf("inspect = %#v, %v", inspect, err)
	}
	denied, _ := gateway.Run(t.Context(), fantasy.ToolCall{Input: `{"action":"inspect","target":"hidden__tool"}`})
	if !denied.IsError || !strings.Contains(denied.Content, "dynamic_target_not_authorized") {
		t.Fatalf("unauthorized inspect = %#v", denied)
	}
}

func TestDynamicToolGatewayValidatesBeforePolicyAndTransport(t *testing.T) {
	c := newDirectTypedCoordinator(t, "view", nil, nil)
	executor := &gatewayTestExecutor{text: "ok"}
	target := gatewayTestTarget()
	gateway := newDynamicToolGateway(c, executor, []DynamicToolTarget{target})
	ctx := gatewayTestContext(t, c, dynamicToolGatewayName, target.Name)

	invalidInputs := []string{
		`{"action":"call","target":"github__get_issue","arguments":[]} `,
		`{"action":"call","target":"github__get_issue","arguments":{"owner":"other","issue_number":1,"labels":[],"metadata":{"open":true}}}`,
		`{"action":"call","target":"github__get_issue","arguments":{"owner":"kjelly","issue_number":1.5,"labels":[],"metadata":{"open":true}}}`,
		`{"action":"call","target":"github__get_issue","arguments":{"owner":"kjelly","issue_number":1,"labels":[1],"metadata":{"open":true}}}`,
		`{"action":"call","target":"github__get_issue","arguments":{"owner":"kjelly","issue_number":1,"labels":[],"metadata":{"extra":true}}}`,
		`{"action":"call","target":"github__get_issue","arguments":{}} trailing`,
	}
	for _, input := range invalidInputs {
		response, err := gateway.Run(ctx, fantasy.ToolCall{ID: "call-invalid", Input: input})
		if err != nil || !response.IsError {
			t.Fatalf("invalid response = %#v, %v", response, err)
		}
	}
	if executor.calls != 0 {
		t.Fatalf("invalid calls reached transport %d time(s)", executor.calls)
	}

	valid := `{"action":"call","target":"github__get_issue","arguments":{"metadata":{"open":true},"labels":["bug"],"issue_number":1,"owner":"kjelly"}}`
	response, err := gateway.Run(ctx, fantasy.ToolCall{ID: "call-valid", Input: valid})
	if err != nil || response.IsError || response.Content != "ok" || executor.calls != 1 {
		t.Fatalf("valid response = %#v, err=%v calls=%d", response, err, executor.calls)
	}
	if executor.name != target.Name || executor.input != `{"issue_number":1,"labels":["bug"],"metadata":{"open":true},"owner":"kjelly"}` {
		t.Fatalf("transport received name=%q input=%s", executor.name, executor.input)
	}
}

func TestDynamicToolGatewayCallAppliesLogicalPermissionAndReadOnlyPolicy(t *testing.T) {
	c := newDirectTypedCoordinator(t, "view", nil, nil)
	target := gatewayTestTarget()
	valid := `{"action":"call","target":"github__get_issue","arguments":{"owner":"kjelly","issue_number":1,"labels":[],"metadata":{"open":true}}}`

	permissionExecutor := &gatewayTestExecutor{text: "should not run"}
	gateway := newDynamicToolGateway(c, permissionExecutor, []DynamicToolTarget{target})
	denied, _ := gateway.Run(gatewayTestContext(t, c, dynamicToolGatewayName), fantasy.ToolCall{ID: "permission", Input: valid})
	if !denied.IsError || !strings.Contains(denied.Content, "dynamic_policy_denied") || permissionExecutor.calls != 0 {
		t.Fatalf("permission denial = %#v, calls=%d", denied, permissionExecutor.calls)
	}

	readOnlyExecutor := &gatewayTestExecutor{text: "should not run"}
	gateway = newDynamicToolGateway(c, readOnlyExecutor, []DynamicToolTarget{target})
	readOnlyCtx := context.WithValue(gatewayTestContext(t, c, dynamicToolGatewayName, target.Name), tools.AgentReadOnlyExecutionKey, true)
	search, _ := gateway.Run(readOnlyCtx, fantasy.ToolCall{Input: `{"action":"search"}`})
	denied, _ = gateway.Run(readOnlyCtx, fantasy.ToolCall{ID: "readonly", Input: valid})
	if search.IsError || !denied.IsError || !strings.Contains(denied.Content, "side_effect:none") || readOnlyExecutor.calls != 0 {
		t.Fatalf("read-only search=%#v call=%#v calls=%d", search, denied, readOnlyExecutor.calls)
	}

	forceExecutor := &gatewayTestExecutor{text: "ok"}
	gateway = newDynamicToolGateway(c, forceExecutor, []DynamicToolTarget{target})
	forceCtx := context.WithValue(gatewayTestContext(t, c, dynamicToolGatewayName, target.Name), tools.AgentForceMCPKey, true)
	allowed, err := gateway.Run(forceCtx, fantasy.ToolCall{ID: "force-mcp", Input: valid})
	if err != nil || allowed.IsError || forceExecutor.calls != 1 {
		t.Fatalf("force-MCP response=%#v err=%v calls=%d", allowed, err, forceExecutor.calls)
	}
}

func TestDynamicToolGatewayInvocationReceiptAndUTF8Truncation(t *testing.T) {
	c := newDirectTypedCoordinator(t, "view", nil, nil)
	todoID := resolverTodoID(t, c, "dynamic-receipt")
	target := gatewayTestTarget()
	executor := &gatewayTestExecutor{text: strings.Repeat("界", maxDynamicGatewayOutputBytes)}
	gateway := newDynamicToolGateway(c, executor, []DynamicToolTarget{target})
	accumulator := newDynamicInvocationAccumulator(1)
	evidence := new(toolCallEvidence)
	transcript, err := newTaskTranscriptForAttempt(t.TempDir(), todoID, "run", 1)
	if err != nil {
		t.Fatal(err)
	}
	var events []StatusEvent
	c.SetStatusReporter(func(event StatusEvent) { events = append(events, event) })
	ctx := gatewayTestContext(t, c, dynamicToolGatewayName, target.Name)
	ctx = withDynamicToolInvocationSink(ctx, accumulator.record, dynamicInvocationDiagnosticsReporter(c, transcript, evidence, "worker", todoID))
	valid := `{"action":"call","target":"github__get_issue","arguments":{"owner":"kjelly","issue_number":1,"labels":[],"metadata":{"open":true}}}`
	response, err := gateway.Run(ctx, fantasy.ToolCall{ID: "logical-call", Input: valid})
	if err != nil || response.IsError || len(response.Content) > maxDynamicGatewayOutputBytes || !strings.Contains(response.Content, "truncated by hufu") {
		t.Fatalf("bounded response bytes=%d error=%v responseError=%v", len(response.Content), err, response.IsError)
	}
	if err := transcript.Close(); err != nil {
		t.Fatal(err)
	}
	receipts, truncated := accumulator.snapshot()
	if truncated != 0 || len(receipts) != 1 || receipts[0].LogicalTool != target.Name || receipts[0].GatewayTool != dynamicToolGatewayName || !receipts[0].Truncated {
		t.Fatalf("logical receipts=%#v truncated=%d", receipts, truncated)
	}
	if evidence.toolName != target.Name || evidence.toolInput == "" || evidence.resultText == "" || evidence.resultErr {
		t.Fatalf("retry evidence=%#v", evidence)
	}
	if item := c.todoItemByID(todoID); item == nil || item.LastOperation != target.Name {
		t.Fatalf("last operation item=%#v", item)
	}
	if len(events) != 2 || events[0].Type != "tool_call" || events[0].ToolName != target.Name || events[0].ToolArgs == "" || events[1].Type != "tool_result" || events[1].ToolName != target.Name || events[1].ToolResult == "" {
		t.Fatalf("logical status events=%#v", events)
	}
	transcriptData, err := os.ReadFile(transcript.path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(transcriptData), `"tool":"`+target.Name+`"`) || !strings.Contains(string(transcriptData), `"tool_call_id":"logical-call"`) {
		t.Fatalf("logical transcript=%s", transcriptData)
	}

	receipt := &ExecutionReceipt{ToolInvocations: receipts, ToolInvocationsTruncated: truncated}
	cloned := cloneExecutionReceipt(receipt)
	cloned.ToolInvocations[0].LogicalTool = "mutated"
	if receipt.ToolInvocations[0].LogicalTool != target.Name {
		t.Fatal("receipt clone shares logical invocation backing array")
	}
	canonical := toCanonicalReceipts([]ExecutionReceipt{*receipt}, nil)
	if len(canonical) != 1 || len(canonical[0].ToolInvocations) != 1 || canonical[0].ToolInvocations[0].LogicalTool != target.Name {
		t.Fatalf("canonical receipt projection=%#v", canonical)
	}
}

func TestDynamicSchemaUnsupportedKeywordIsIneligible(t *testing.T) {
	schema := map[string]any{"type": "object", "properties": map[string]any{"name": map[string]any{"type": "string", "pattern": "x"}}}
	if dynamicSchemaEligible(schema) {
		t.Fatal("unsupported pattern schema was gateway eligible")
	}
}

func gatewayProjectionFixture(t *testing.T, count int) ([]fantasy.AgentTool, []internalmcp.MCPTool, *DynamicToolAuthorizationSnapshot) {
	t.Helper()
	concrete := make([]fantasy.AgentTool, 0, count)
	descriptors := make([]internalmcp.MCPTool, 0, count)
	targets := make([]FrozenDynamicToolTarget, 0, count)
	for i := range count {
		name := fmt.Sprintf("server__tool_%02d", i)
		descriptor := internalmcp.MCPTool{
			Name: name, ServerName: "server", OrigName: fmt.Sprintf("tool_%02d", i), Description: "dynamic tool",
			InputSchema: map[string]any{"type": "object", "properties": map[string]any{"value": map[string]any{"type": "string"}}},
		}
		fingerprint, err := internalmcp.MCPToolDescriptorSHA256(descriptor)
		if err != nil {
			t.Fatal(err)
		}
		concrete = append(concrete, &dynamicTestTool{info: fantasy.ToolInfo{Name: name}})
		descriptors = append(descriptors, descriptor)
		targets = append(targets, FrozenDynamicToolTarget{Name: name, DescriptorSHA256: fingerprint})
	}
	snapshot := &DynamicToolAuthorizationSnapshot{Version: dynamicToolAuthorizationSnapshotVersion, Targets: targets}
	snapshot.FrozenCatalogDigest = frozenDynamicCatalogDigest(targets)
	return concrete, descriptors, snapshot
}

func TestProjectDynamicToolGatewayCollapsesManyTargetsAndPreservesCompatibilityModes(t *testing.T) {
	c := newDirectTypedCoordinator(t, "view", nil, nil)
	concrete, descriptors, snapshot := gatewayProjectionFixture(t, 50)
	projected, targets, err := projectDynamicToolGateway(c, concrete, nil, descriptors, snapshot, WorkerToolResolutionNormal, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(projected) != 1 || projected[0].Info().Name != dynamicToolGatewayName || len(targets) != 50 {
		t.Fatalf("projected names=%v target count=%d", agentToolNames(projected), len(targets))
	}

	unchanged, noTargets, err := projectDynamicToolGateway(c, concrete, nil, descriptors, newEmptyDynamicToolAuthorizationSnapshot(), WorkerToolResolutionNormal, nil)
	if err != nil || !slices.Equal(agentToolNames(unchanged), agentToolNames(concrete)) || len(noTargets) != 0 {
		t.Fatalf("empty snapshot projected names=%v targets=%d err=%v", agentToolNames(unchanged), len(noTargets), err)
	}
	closed, noTargets, err := projectDynamicToolGateway(c, concrete, nil, descriptors, snapshot, WorkerToolResolutionNormal, []string{descriptors[0].Name})
	if err != nil || !slices.Equal(agentToolNames(closed), agentToolNames(concrete)) || len(noTargets) != 0 {
		t.Fatalf("closed sequence projected names=%v targets=%d err=%v", agentToolNames(closed), len(noTargets), err)
	}
}

func TestProjectDynamicToolGatewayOrdersSurfaceAndFailsOnCollision(t *testing.T) {
	c := newDirectTypedCoordinator(t, "view", nil, nil)
	managerTools, descriptors, snapshot := gatewayProjectionFixture(t, 1)
	base := []fantasy.AgentTool{
		&dynamicTestTool{info: fantasy.ToolInfo{Name: "base_b"}},
		&dynamicTestTool{info: fantasy.ToolInfo{Name: "base_a"}},
	}
	ineligible := internalmcp.MCPTool{
		Name: "server__direct", ServerName: "server", OrigName: "direct",
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{"value": map[string]any{"type": "string", "pattern": "x"}}},
	}
	ineligibleFingerprint, err := internalmcp.MCPToolDescriptorSHA256(ineligible)
	if err != nil {
		t.Fatal(err)
	}
	snapshot.Targets = append(snapshot.Targets, FrozenDynamicToolTarget{Name: ineligible.Name, DescriptorSHA256: ineligibleFingerprint})
	slices.SortFunc(snapshot.Targets, func(a, b FrozenDynamicToolTarget) int { return strings.Compare(a.Name, b.Name) })
	snapshot.FrozenCatalogDigest = frozenDynamicCatalogDigest(snapshot.Targets)
	tools := append(slices.Clone(base), managerTools...)
	tools = append(tools,
		&dynamicTestTool{info: fantasy.ToolInfo{Name: "z_custom"}},
		&dynamicTestTool{info: fantasy.ToolInfo{Name: ineligible.Name}},
		&dynamicTestTool{info: fantasy.ToolInfo{Name: submitResultToolName}},
	)
	descriptors = append(descriptors, ineligible)
	projected, targets, err := projectDynamicToolGateway(c, tools, base, descriptors, snapshot, WorkerToolResolutionNormal, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"base_b", "base_a", "server__direct", "z_custom", dynamicToolGatewayName, submitResultToolName}
	if got := agentToolNames(projected); !slices.Equal(got, want) || len(targets) != 1 {
		t.Fatalf("projected names=%v targets=%d, want %v/1", got, len(targets), want)
	}

	collision := append(slices.Clone(tools), &dynamicTestTool{info: fantasy.ToolInfo{Name: dynamicToolGatewayName}})
	if _, _, err := projectDynamicToolGateway(c, collision, base, descriptors, snapshot, WorkerToolResolutionNormal, nil); err == nil || !strings.Contains(err.Error(), "tool_name_collision") {
		t.Fatalf("collision error=%v", err)
	}
}

func TestDynamicGatewayKeepsProviderDigestStableAndLogicalDigestDistinct(t *testing.T) {
	c := newDirectTypedCoordinator(t, "view", nil, nil)
	def := &agent.AgentDef{Name: "worker"}
	oneTools, oneDescriptors, oneSnapshot := gatewayProjectionFixture(t, 1)
	twoTools, twoDescriptors, twoSnapshot := gatewayProjectionFixture(t, 2)
	oneSurface, oneTargets, err := projectDynamicToolGateway(c, oneTools, nil, oneDescriptors, oneSnapshot, WorkerToolResolutionNormal, nil)
	if err != nil {
		t.Fatal(err)
	}
	twoSurface, twoTargets, err := projectDynamicToolGateway(c, twoTools, nil, twoDescriptors, twoSnapshot, WorkerToolResolutionNormal, nil)
	if err != nil {
		t.Fatal(err)
	}
	oneProvider, oneLogical, err := workerToolDigests(c, def, TaskDef{}, WorkerToolResolutionNormal, PhaseExecute, oneSurface, oneTargets, oneSnapshot, nil)
	if err != nil {
		t.Fatal(err)
	}
	twoProvider, twoLogical, err := workerToolDigests(c, def, TaskDef{}, WorkerToolResolutionNormal, PhaseExecute, twoSurface, twoTargets, twoSnapshot, nil)
	if err != nil {
		t.Fatal(err)
	}
	if oneProvider != twoProvider || oneLogical == twoLogical {
		t.Fatalf("provider digests %q/%q logical digests %q/%q", oneProvider, twoProvider, oneLogical, twoLogical)
	}
}
