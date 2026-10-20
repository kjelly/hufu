package team

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"charm.land/fantasy"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/tools"
)

// workflowMCPToolAllowed keeps observation phases useful without granting
// arbitrary MCP execution. Reuse the call-time observation boundary, including
// live adapter authenticity, explicit agent grants and network restrictions.
func (c *Coordinator) workflowMCPToolAllowed(ctx context.Context, def *agent.AgentDef, name string, inner fantasy.AgentTool) bool {
	if c == nil || c.phaseWorkflow == nil || !c.phaseWorkflow.Enabled() || c.phaseWorkflow.State() == PhaseExecute {
		return true
	}
	if def == nil {
		return false
	}
	ctx = context.WithValue(ctx, tools.AgentNameKey, def.Name)
	return c.workerMCPObservationGrant(ctx, name, inner)
}

func (c *Coordinator) filterWorkflowMCPTools(ctx context.Context, def *agent.AgentDef, candidates []fantasy.AgentTool) []fantasy.AgentTool {
	filtered := make([]fantasy.AgentTool, 0, len(candidates))
	for _, tool := range candidates {
		if c.workflowMCPToolAllowed(ctx, def, tool.Info().Name, tool) {
			filtered = append(filtered, tool)
		}
	}
	return filtered
}

// workerMCPObservationGrant requires both an explicit agent grant and an
// authentic, live manager policy. Server annotations and lookalike handlers
// cannot earn the exception. Bound worksets retain centralized path enforcement.
func (c *Coordinator) workerMCPObservationGrant(ctx context.Context, name string, inner fantasy.AgentTool) bool {
	if c == nil || c.session == nil || c.mcpManager == nil {
		return false
	}
	agentName, _ := ctx.Value(tools.AgentNameKey).(string)
	def := c.session.Agents[strings.ToLower(agentName)]
	noNet, _ := ctx.Value(tools.AgentNetworkBlockKey).(bool)
	if def == nil || c.toolDeniedByTeam(name) || c.noNet || def.NoNet || noNet || !explicitlyDeclaresTool(def.Tools, name) {
		return false
	}
	if inner != nil && !c.mcpManager.OwnsAgentTool(inner) {
		return false
	}
	return c.declaredMCPObservationTool(name)
}

// declaredMCPObservationTool reports whether the live manager holds a
// maintainer-owned worker policy for name that declares no side effect and no
// filesystem access. It classifies the tool only; whether an agent may call it
// is workerMCPObservationGrant's decision.
func (c *Coordinator) declaredMCPObservationTool(name string) bool {
	if c == nil || c.mcpManager == nil {
		return false
	}
	policy, ok := c.mcpManager.WorkerPolicy(name)
	return ok && policy.SideEffect == "none" && policy.Filesystem == "none"
}

// readOnlyToolCall extends isReadOnlyToolCall to MCP observation tools for
// recovery decisions. MCP calls usually reach a worker through the dynamic
// gateway: its search and inspect actions read only the frozen catalog, and a
// call is read-only only when its target carries a declared observation
// policy. Server annotations, model claims and malformed gateway input never
// qualify, so an unclassified call keeps the fail-closed result.
func (c *Coordinator) readOnlyToolCall(toolName, input string) bool {
	if isReadOnlyToolCall(toolName, input) {
		return true
	}
	name := strings.TrimSpace(toolName)
	if name == dynamicToolGatewayName {
		request, _, err := parseDynamicGatewayRequest(input)
		if err != nil {
			return false
		}
		if request.Action != "call" {
			return true
		}
		name = request.Target
	}
	return c.declaredMCPObservationTool(name)
}

func (c *Coordinator) workerArtifactToolDenial(ctx context.Context, name string, inner fantasy.AgentTool) string {
	policy, _ := ctx.Value(tools.ArtifactPathPolicyKey).(tools.ArtifactPathPolicy)
	granted := c.workerMCPObservationGrant(ctx, name, inner)
	// A missing artifact/read-only marker must not create authority for a
	// policy-enabled tool (including coordinator or future execution paths).
	if c != nil && c.mcpManager != nil {
		if _, declared := c.mcpManager.WorkerPolicy(name); declared && !granted {
			return fmt.Sprintf("MCP tool %q requires an authentic adapter, an explicit agent grant and permitted network access", name)
		}
	}
	if !policy.FailClosedForUnsupported && granted {
		return ""
	}
	return artifactScopeToolDenial(ctx, name, inner)
}

func (c *Coordinator) workerReadOnlyToolMutation(ctx context.Context, name, input string, inner fantasy.AgentTool) bool {
	if c.workerMCPObservationGrant(ctx, name, inner) {
		return false
	}
	return readOnlyToolMutation(name, input)
}

func (c *Coordinator) workerMCPArgumentDenial(name, input string) string {
	if c == nil || c.mcpManager == nil {
		return ""
	}
	if _, ok := c.mcpManager.WorkerPolicy(name); ok {
		if err := c.mcpManager.ValidateWorkerToolArguments(name, input); err != nil {
			return err.Error()
		}
	}
	return ""
}

// bindMCPWorkerPolicies refuses startup when an explicitly trusted tool did not
// load, instead of asking a model to discover a static missing prerequisite.
func (c *Coordinator) bindMCPWorkerPolicies() error {
	if c == nil || c.session == nil {
		return nil
	}
	for server, config := range c.session.MCPServers {
		for native, policy := range config.ToolPolicies {
			if err := c.mcpManager.BindWorkerToolPolicy(server+"__"+native, policy); err != nil {
				return err
			}
		}
	}
	return nil
}

// Only digests enter durable state. Server launch/endpoint identity and every
// declaration are included, without storing commands, schema constants or keys.
type ExecutionMCPWorkerPolicySnapshot struct {
	Server string `json:"server"`
	Hash   string `json:"hash"`
}

func executionPolicyMCPWorkerPolicies(session *TeamSession) []ExecutionMCPWorkerPolicySnapshot {
	if session == nil {
		return nil
	}
	var result []ExecutionMCPWorkerPolicySnapshot
	for server, config := range session.MCPServers {
		if len(config.ToolPolicies) > 0 {
			result = append(result, ExecutionMCPWorkerPolicySnapshot{Server: server, Hash: mcpServerConfigHash(config)})
		}
	}
	slices.SortFunc(result, func(a, b ExecutionMCPWorkerPolicySnapshot) int { return strings.Compare(a.Server, b.Server) })
	return result
}

func validateExecutionMCPWorkerPolicies(entries []ExecutionMCPWorkerPolicySnapshot) error {
	for i, entry := range entries {
		if strings.TrimSpace(entry.Server) == "" || !strings.HasPrefix(entry.Hash, "sha256:") || !decisionDigestPattern.MatchString(strings.TrimPrefix(entry.Hash, "sha256:")) || (i > 0 && entries[i-1].Server >= entry.Server) {
			return fmt.Errorf("execution policy snapshot MCP worker policies are invalid or unordered")
		}
	}
	return nil
}
