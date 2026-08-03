package team

import (
	"context"
	"testing"

	"charm.land/fantasy"
	"github.com/anomalyco/hufu/internal/agent"
	"github.com/anomalyco/hufu/internal/config"
	"github.com/anomalyco/hufu/internal/tools"
)

type distinctMCPPolicy struct {
	toolCalls int
	mcpCalls  int
}

func (p *distinctMCPPolicy) AuthorizeToolCall(context.Context, ToolAuthorizationRequest) (PolicyDecision, error) {
	p.toolCalls++
	return PolicyDecision{Code: DecisionDeny, RuleID: "test.builtin-deny", Reason: "built-in tools denied"}, nil
}

func (p *distinctMCPPolicy) AuthorizeMCPCall(context.Context, MCPAuthorizationRequest) (PolicyDecision, error) {
	p.mcpCalls++
	return PolicyDecision{Code: DecisionAllow, RuleID: "test.mcp-allow"}, nil
}

func (*distinctMCPPolicy) AuthorizeTransition(context.Context, TaskTransitionRequest) (PolicyDecision, error) {
	return PolicyDecision{Code: DecisionAllow, RuleID: "test.transition"}, nil
}

type agentSpecificStreamTestAgent struct{}

func (agentSpecificStreamTestAgent) Generate(ctx context.Context, call fantasy.AgentCall) (*fantasy.AgentResult, error) {
	return agentSpecificStreamTestAgent{}.Stream(ctx, fantasy.AgentStreamCall{Prompt: call.Prompt, Messages: call.Messages})
}

func (agentSpecificStreamTestAgent) Stream(_ context.Context, call fantasy.AgentStreamCall) (*fantasy.AgentResult, error) {
	if err := call.OnToolCall(fantasy.ToolCallContent{ToolCallID: "mcp-1", ToolName: "run-tests", Input: `{}`}); err != nil {
		return &fantasy.AgentResult{}, err
	}
	return &fantasy.AgentResult{Response: fantasy.Response{Content: fantasy.ResponseContent{fantasy.TextContent{Text: "stream-ok"}}}}, nil
}

func TestAuthorizationPolicyFailsClosedForUnknownTools(t *testing.T) {
	policy := defaultAuthorizationPolicy{}
	decision, err := policy.AuthorizeToolCall(context.Background(), ToolAuthorizationRequest{Tool: "bash", AllowedTools: map[string]bool{"view": true}, FailureMode: PolicyFailClosed})
	if err != nil || decision.Code != DecisionDeny {
		t.Fatalf("unknown tool decision = %#v, err %v", decision, err)
	}
	decision, err = policy.AuthorizeMCPCall(context.Background(), MCPAuthorizationRequest{Server: "fs", Tool: "write", AllowedTools: map[string]bool{"fs:read": true}, FailureMode: PolicyFailClosed})
	if err != nil || decision.Code != DecisionDeny {
		t.Fatalf("unknown MCP tool decision = %#v, err %v", decision, err)
	}
}

func TestAuthorizationPolicyAllowsExplicitToolAndTransition(t *testing.T) {
	policy := defaultAuthorizationPolicy{}
	decision, err := policy.AuthorizeToolCall(context.Background(), ToolAuthorizationRequest{Agent: "worker", Tool: "view", AllowedTools: map[string]bool{"view": true}})
	if err != nil || decision.Code != DecisionAllow {
		t.Fatalf("allowed tool decision = %#v, err %v", decision, err)
	}
	decision, err = policy.AuthorizeTransition(context.Background(), TaskTransitionRequest{TaskID: "1", From: TaskPending, To: TaskInProgress})
	if err != nil || decision.Code != DecisionAllow {
		t.Fatalf("transition decision = %#v, err %v", decision, err)
	}
}

func TestAuthorizeStreamToolRoutesNamespacedMCP(t *testing.T) {
	c := &Coordinator{}
	decision, err := c.authorizeStreamTool(context.Background(), "worker", "filesystem__read", map[string]bool{"filesystem__read": true})
	if err != nil || decision.Code != DecisionAllow {
		t.Fatalf("namespaced MCP decision = %#v, err %v", decision, err)
	}
	decision, err = c.authorizeStreamTool(context.Background(), "worker", "filesystem__write", map[string]bool{"filesystem__read": true})
	if err != nil || decision.Code != DecisionDeny {
		t.Fatalf("unauthorized MCP decision = %#v, err %v", decision, err)
	}
}

func TestRegisterProviderSecretsAddsResolvedSources(t *testing.T) {
	registry := tools.NewSecretRegistry()
	session := &TeamSession{}
	session.Config.Providers = map[string]config.ProviderConfig{
		"openai": {ProviderAPIKey: "provider-key-exact-123"},
	}
	registerProviderSecrets(registry, session, "default-key-exact-456")
	for _, name := range []string{"provider.default.api_key", "provider.openai.api_key"} {
		if value, ok := registry.Resolve(name); !ok || value == "" {
			t.Fatalf("provider secret %q was not registered", name)
		}
	}
}

func TestAgentSpecificMCPStreamUsesMCPPolicyForCasePreservingAgent(t *testing.T) {
	policy := &distinctMCPPolicy{}
	session := &TeamSession{
		Workspace: t.TempDir(),
		Config:    agent.TeamConfig{Name: "team"},
		Agents: map[string]*agent.AgentDef{
			"helper": {Name: "Helper", MCPTools: map[string]agent.MCPToolConfig{"run-tests": {Cmd: "printf ok"}}},
		},
	}
	c := &Coordinator{session: session, taskTracker: NewTaskTracker(), reportStatus: func(StatusEvent) {}}
	c.SetAuthorizationPolicy(policy)
	ctx := c.withEffectiveToolsAllowed(context.Background(), session.Agents["helper"])
	output, _, err := c.runAgentWithStatusAndHistory(ctx, agentSpecificStreamTestAgent{}, "helper", "run", nil, &taskTiming{})
	if err != nil || output != "stream-ok" {
		t.Fatalf("stream output=%q err=%v", output, err)
	}
	if policy.toolCalls != 0 || policy.mcpCalls != 1 {
		t.Fatalf("policy routing tool=%d mcp=%d, want tool=0 mcp=1", policy.toolCalls, policy.mcpCalls)
	}
}
