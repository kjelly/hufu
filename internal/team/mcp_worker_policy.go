package team

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"charm.land/fantasy"

	"github.com/kjelly/hufu/internal/tools"
)

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
	policy, ok := c.mcpManager.WorkerPolicy(name)
	return ok && policy.SideEffect == "none" && policy.Filesystem == "none"
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
