package team

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/mcp"
)

var mcpActionTestConfig = agent.ActionProviderConfig{Runtime: "mcp", Server: "diagnostics", Tool: "collect_debug"}

func newTestMCPActionProvider(t *testing.T) *mcpActionProvider {
	t.Helper()
	provider, err := newMCPActionProvider("diagnostics", mcpActionTestConfig, map[string]mcp.MCPServerConfig{
		"diagnostics": {AllowedTools: []string{"collect_debug"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return provider
}

// bindTestMCPAction declares the provider in c's team and binds it to
// manager, the way NewCoordinator does.
func bindTestMCPAction(t *testing.T, c *Coordinator, manager *mcp.MCPToolManager) {
	t.Helper()
	c.session.Config.ActionProviders = map[string]agent.ActionProviderConfig{"diagnostics": mcpActionTestConfig}
	c.mcpManager = manager
	if err := c.bindMCPActionProviders(); err != nil {
		t.Fatal(err)
	}
}

// newMCPCatalogActionRuntime returns a dynamic catalog coordinator whose
// catalog entry runs through an MCP provider served by fake.
func newMCPCatalogActionRuntime(t *testing.T, fake *mcpActionFake, bind bool) (*Coordinator, *EventStore, *mcpActionProvider) {
	t.Helper()
	provider := newTestMCPActionProvider(t)
	c, events := newDynamicCatalogCoordinator(t, dynamicCatalogSession(t, provider, true))
	if bind {
		bindTestMCPAction(t, c, newMCPActionManager(t, fake))
	}
	return c, events, provider
}

func mcpCatalogActionTask(payload string) TaskDef {
	task := catalogActionTask(&CatalogActionBinding{
		ActionID: "collect", EntryHash: "sha256:entry", CatalogHash: "sha256:catalog",
		ArgumentsHash: runInputHash([]byte(payload)), InvocationID: "tai_mcp",
	})
	task.Action.Payload = payload
	return task
}

func runMCPCatalogAction(c *Coordinator, payload string) (TaskDef, error) {
	task, item := addCatalogActionTodo(c, mcpCatalogActionTask(payload))
	_, err := c.executeRuntimeAction(context.Background(), task, item.ID)
	return task, err
}

type denyMCPAuthorizationPolicy struct{ defaultAuthorizationPolicy }

func (denyMCPAuthorizationPolicy) AuthorizeMCPCall(context.Context, MCPAuthorizationRequest) (PolicyDecision, error) {
	return PolicyDecision{Code: DecisionDeny, RuleID: "test.deny", Reason: "denied by test policy"}, nil
}

func policyDecisionEvents(t *testing.T, events *EventStore) []map[string]any {
	t.Helper()
	stored, err := events.ReadEvents()
	if err != nil {
		t.Fatal(err)
	}
	var decisions []map[string]any
	for _, event := range stored {
		if event.Type != "policy_decision" {
			continue
		}
		var payload map[string]any
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			t.Fatal(err)
		}
		decisions = append(decisions, payload)
	}
	return decisions
}

func TestMCPCatalogActionRunsThroughTheActionLifecycle(t *testing.T) {
	fake := newMCPActionFake(collectDebugTool("v1"))
	var sent map[string]any
	fake.setHandler(func(_ context.Context, request mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		sent = request.GetArguments()
		return mcpgo.NewToolResultStructuredOnly(map[string]any{"summary": "ok"}), nil
	})
	c, events, _ := newMCPCatalogActionRuntime(t, fake, true)
	if _, err := runMCPCatalogAction(c, `{"service":"api"}`); err != nil {
		t.Fatalf("MCP catalog action: %v", err)
	}
	if calls := fake.calls.Load(); calls != 1 || sent["service"] != "api" || len(sent) != 1 {
		t.Fatalf("calls = %d, sent arguments = %v", calls, sent)
	}
	got := c.taskTracker.TodoList().Items()[0]
	if got.Status != TaskDone || got.TypedResult == nil || got.TypedResult.RuntimeOutputs["summary"] != "ok" {
		t.Fatalf("task = status %s result %#v", got.Status, got.TypedResult)
	}
	lifecycle := actionLifecycleEvents(t, events)
	if len(lifecycle) != 2 || lifecycle[0].Type != "action_started" || lifecycle[1].Type != "action_completed" {
		t.Fatalf("action events = %v", lifecycle)
	}
	decisions := policyDecisionEvents(t, events)
	if len(decisions) != 1 || decisions[0]["kind"] != "mcp" || decisions[0]["server"] != "diagnostics" || decisions[0]["tool"] != "collect_debug" || decisions[0]["agent"] != "runtime-engineer" {
		t.Fatalf("policy decisions = %v, want one allowed MCP decision", decisions)
	}
}

func TestMCPCatalogActionRejectsBeforeCallingTheTool(t *testing.T) {
	tests := []struct {
		name    string
		bind    bool
		payload string
		prepare func(*Coordinator, *mcpActionProvider)
		want    string
	}{
		{name: "unbound provider", payload: `{"service":"api"}`, want: mcpActionUnbound},
		{name: "arguments miss a required property", bind: true, payload: `{}`, want: mcpActionPayloadInvalid},
		{name: "arguments have the wrong type", bind: true, payload: `{"service":7}`, want: mcpActionPayloadInvalid},
		{name: "payload is not an object", bind: true, payload: `["api"]`, want: mcpActionPayloadInvalid},
		{name: "policy denies", bind: true, payload: `{"service":"api"}`, prepare: func(c *Coordinator, _ *mcpActionProvider) {
			c.SetAuthorizationPolicy(denyMCPAuthorizationPolicy{})
		}, want: mcpActionAuthorizationDenied},
		{name: "descriptor changed after admission", bind: true, payload: `{"service":"api"}`, prepare: func(_ *Coordinator, p *mcpActionProvider) {
			p.mu.Lock()
			p.binding.descriptorSHA256 = strings.Repeat("0", 64)
			p.mu.Unlock()
		}, want: mcpActionDescriptorMismatch},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := newMCPActionFake(collectDebugTool("v1"))
			c, events, provider := newMCPCatalogActionRuntime(t, fake, tt.bind)
			if tt.prepare != nil {
				tt.prepare(c, provider)
			}
			_, err := runMCPCatalogAction(c, tt.payload)
			if err == nil || !strings.Contains(err.Error(), tt.want+":") {
				t.Fatalf("error = %v, want %s", err, tt.want)
			}
			if calls := fake.calls.Load(); calls != 0 {
				t.Fatalf("CallTool count = %d, want 0", calls)
			}
			lifecycle := actionLifecycleEvents(t, events)
			if len(lifecycle) == 0 || lifecycle[len(lifecycle)-1].Type != "action_failed" {
				t.Fatalf("action events = %v, want a final action_failed", lifecycle)
			}
		})
	}
}

func TestMCPActionExecuteRequiresRuntimeIdentityAndAuthorizer(t *testing.T) {
	fake := newMCPActionFake(collectDebugTool("v1"))
	c, _, provider := newMCPCatalogActionRuntime(t, fake, true)
	action := Action{Capability: "diagnostics", Type: "collect", Payload: `{"service":"api"}`}
	identity := ActionEnvironment{Workspace: t.TempDir(), TaskID: "1", Attempt: 1, ActionInvocationID: "action-1"}
	tests := []struct {
		name string
		ctx  context.Context
		want string
	}{
		{name: "no task attempt", ctx: c.withRuntimeActionMCPAuthorization(context.Background(), TaskDef{Agent: "runtime-engineer"}), want: mcpActionIdentityMissing},
		{name: "no authorizer", ctx: WithActionEnvironment(context.Background(), identity), want: mcpActionAuthorizationMissing},
		{name: "a worker authorizer is not the runtime authorizer", ctx: mcp.WithToolAuthorizer(WithActionEnvironment(context.Background(), identity), func(context.Context, string, string, string) error {
			return nil
		}), want: mcpActionAuthorizationMissing},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := provider.Execute(tt.ctx, action); err == nil || !strings.HasPrefix(err.Error(), tt.want+":") {
				t.Fatalf("error = %v, want %s", err, tt.want)
			}
		})
	}
	if calls := fake.calls.Load(); calls != 0 {
		t.Fatalf("CallTool count = %d, want 0", calls)
	}
}

func TestMCPActionTimeoutIsClassifiedAsTimeout(t *testing.T) {
	fake := newMCPActionFake(collectDebugTool("v1"))
	fake.setHandler(func(ctx context.Context, _ mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		select {
		case <-ctx.Done():
		case <-time.After(2 * time.Second):
		}
		return mcpgo.NewToolResultText("late"), nil
	})
	c, _, provider := newMCPCatalogActionRuntime(t, fake, false)
	bindTestMCPAction(t, c, newHTTPMCPActionManager(t, fake))
	provider.timeout = 50 * time.Millisecond
	task, err := runMCPCatalogAction(c, `{"service":"api"}`)
	if err == nil || !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), mcpActionTransportFailed+":") {
		t.Fatalf("error = %v, want a transport deadline", err)
	}
	if category := c.phaseWorkflow.actionExecutionError(task, err).Category; category != CategoryTimeout {
		t.Fatalf("failure category = %s, want %s", category, CategoryTimeout)
	}
	if calls := fake.calls.Load(); calls != 1 {
		t.Fatalf("CallTool count = %d, want 1", calls)
	}
}

func TestMCPStaticWorkflowActionRuns(t *testing.T) {
	fake := newMCPActionFake(collectDebugTool("v1"))
	c, session := newCommandActionCoordinator(t, []string{"/bin/false"}, "run-mcp-static")
	session.Config.Capabilities.Required = []string{"diagnostics"}
	session.ProviderRegistry.Register("diagnostics", newTestMCPActionProvider(t))
	bindTestMCPAction(t, c, newMCPActionManager(t, fake))
	task := TaskDef{Agent: "executor", Goal: "collect", Phase: PhaseExecute, SideEffect: SideEffectNone,
		Action: &Action{Capability: "diagnostics", Type: "collect_debug", Payload: `{"service":"api"}`}}
	item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{PlanTaskID: "collect", Phase: PhaseExecute, ContractID: "execute", Action: task.Action, Agent: task.Agent, Desc: task.Goal, SideEffect: SideEffectNone}})[0]
	if _, err := c.executeRuntimeAction(context.Background(), task, item.ID); err != nil {
		t.Fatalf("static MCP action: %v", err)
	}
	got := c.taskTracker.TodoList().Items()[0]
	if calls := fake.calls.Load(); calls != 1 || got.Status != TaskDone || got.TypedResult == nil || got.TypedResult.RuntimeOutputs["summary"] != "ok" {
		t.Fatalf("calls = %d, task = status %s result %#v", calls, got.Status, got.TypedResult)
	}
}

func TestForceMCPKeepsCommandActionProviders(t *testing.T) {
	c, _ := newCommandActionCoordinator(t, []string{"/bin/sh", "-c", `printf '{"outputs":{"summary":"ok"}}'`}, "run-force-mcp")
	c.forceMCP = true
	task := TaskDef{Agent: "executor", Goal: "apply", Phase: PhaseExecute, Action: &Action{Capability: "structured-actions", Type: "apply"}}
	item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{PlanTaskID: "apply", Phase: PhaseExecute, ContractID: "execute", Action: task.Action, Agent: task.Agent, Desc: task.Goal}})[0]
	if _, err := c.executeRuntimeAction(context.Background(), task, item.ID); err != nil {
		t.Fatalf("command action under --force-mcp: %v", err)
	}
	if got := c.taskTracker.TodoList().Items()[0]; got.Status != TaskDone {
		t.Fatalf("task status = %s", got.Status)
	}
}
