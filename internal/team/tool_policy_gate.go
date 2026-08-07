package team

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"charm.land/fantasy"

	"github.com/anomalyco/hufu/internal/agent"
	"github.com/anomalyco/hufu/internal/tools"
)

// The authorization boundary for agent tool calls lives here.
//
// It used to live in the stream's OnToolCall callback, which can only return an
// error — and an error there aborts the entire model round. A single call to an
// ungranted tool therefore destroyed the whole attempt, discarding every tool
// call the worker had already completed and burning a retry, before the call was
// even recorded as evidence. Denials are ordinary, recoverable conditions: the
// model should be told and given the chance to finish with the tools it has.
//
// Enforcing in a tool wrapper also unifies the decision with the tool adapter in
// internal/tools, which already surfaces its own denials as tool errors, so the
// two gates no longer disagree about the same tool.

// policyGatedTool wraps an agent tool so an authorization denial reaches the
// model as a tool error result it can adapt to.
type policyGatedTool struct {
	inner       fantasy.AgentTool
	coordinator *Coordinator
}

func (t *policyGatedTool) Info() fantasy.ToolInfo { return t.inner.Info() }

func (t *policyGatedTool) ProviderOptions() fantasy.ProviderOptions {
	return t.inner.ProviderOptions()
}

func (t *policyGatedTool) SetProviderOptions(opts fantasy.ProviderOptions) {
	t.inner.SetProviderOptions(opts)
}

func (t *policyGatedTool) Run(ctx context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
	agentName, _ := ctx.Value(tools.AgentNameKey).(string)
	denial, fatal := t.coordinator.authorizeToolInvocation(ctx, agentName, t.Info().Name)
	if fatal != nil {
		return fantasy.ToolResponse{}, fatal
	}
	if denial != "" {
		if t.coordinator != nil {
			t.coordinator.report(t.coordinator.newEvent("step").withAgent(agentName).
				withMessage(fmt.Sprintf("tool %q denied by policy; continuing with remaining tools", t.Info().Name)))
		}
		return fantasy.NewTextErrorResponse(denial), nil
	}
	if sequenceDenial := taskToolSequenceFromContext(ctx).reserve(t.Info().Name); sequenceDenial != "" {
		if t.coordinator != nil {
			t.coordinator.report(t.coordinator.newEvent("step").withAgent(agentName).
				withMessage(fmt.Sprintf("tool %q denied by closed task sequence", t.Info().Name)))
		}
		return fantasy.NewTextErrorResponse(sequenceDenial), nil
	}
	return t.inner.Run(ctx, call)
}

// gatePolicyTools wraps every tool in agentTools with the recoverable policy
// gate. Tools already wrapped are returned as-is so repeated application (for
// example an escalated retry reusing a prepared set) stays idempotent.
func (c *Coordinator) gatePolicyTools(agentTools []fantasy.AgentTool) []fantasy.AgentTool {
	if c == nil || len(agentTools) == 0 {
		return agentTools
	}
	gated := make([]fantasy.AgentTool, 0, len(agentTools))
	for _, t := range agentTools {
		if t == nil {
			continue
		}
		if _, already := t.(*policyGatedTool); already {
			gated = append(gated, t)
			continue
		}
		gated = append(gated, &policyGatedTool{inner: t, coordinator: c})
	}
	return gated
}

// createGatedAgent is the only agent constructor this package uses. Funnelling
// every agent through one call is what makes the recoverable gate complete: a
// tool set assembled somewhere that skipped gatePolicyTools would have no
// authorization boundary at all now that OnToolCall no longer aborts.
// TestAgentsAreCreatedThroughTheGatedConstructor enforces the funnel.
func (c *Coordinator) createGatedAgent(ctx context.Context, provider *agent.OllamaProvider, cfg agent.AgentConfig, agentTools []fantasy.AgentTool) (fantasy.Agent, error) {
	return agent.CreateAgent(ctx, provider, cfg, c.gatePolicyTools(agentTools))
}

// authorizeToolInvocation resolves whether agentName may call toolName under the
// allowlist attached to ctx.
//
// It returns ("", nil) to allow the call, a non-empty denial message the model
// should see when the call is refused but the run can continue, and a non-nil
// error only when the failure is not something the model can work around (a
// cancelled context, for instance).
func (c *Coordinator) authorizeToolInvocation(ctx context.Context, agentName, toolName string) (string, error) {
	configured := tools.GetToolsAllowed(ctx)
	if configured == nil {
		// No allowlist attached: the tool adapter in internal/tools remains the
		// source of truth, as it is for legacy session permissions and
		// deterministic test agents.
		return "", nil
	}
	// An operator's explicit "always allow"/"always deny" from this session is a
	// decision about this exact tool. The tool adapter honours it; this gate must
	// too, or the same tool is permitted by one boundary and refused by the other.
	if perms, ok := ctx.Value(tools.AgentToolsSessionPermissionsKey).(map[string]bool); ok {
		if allowed, decided := perms[toolName]; decided {
			if allowed {
				return "", nil
			}
			return fmt.Sprintf("tool %q was denied earlier in this session and stays denied. Do not call it again; finish with the tools you have and report what you could not do.", toolName), nil
		}
	}
	allowed := make(map[string]bool, len(configured))
	for _, name := range configured {
		allowed[strings.TrimSpace(name)] = true
	}
	decision, err := c.authorizeStreamTool(ctx, agentName, toolName, allowed)
	if err != nil {
		return "", fmt.Errorf("tool authorization failed for %q: %w", toolName, err)
	}
	if decision.Code == DecisionAllow {
		return "", nil
	}
	reason := strings.TrimSpace(decision.Reason)
	if reason == "" {
		reason = "not authorized"
	}
	return fmt.Sprintf("tool %q is not available to you: %s. Do not call it again — achieve the goal with the tools you do have, and state in your result what you could not do without it.", toolName, reason), nil
}

// unauthorizedExposedTools returns the exposed tool names that the allowlist does
// not cover. An empty allowlist means no policy is attached, so nothing is
// unauthorized.
//
// This is the invariant whose absence deadlocked every worker task: submit_result
// was handed to the model on every task and granted on none.
func unauthorizedExposedTools(exposed, allowed []string) []string {
	if len(allowed) == 0 {
		return nil
	}
	granted := toolNameSet(allowed)
	var missing []string
	seen := map[string]bool{}
	for _, name := range exposed {
		name = strings.TrimSpace(name)
		if name == "" || granted[name] || seen[name] {
			continue
		}
		seen[name] = true
		missing = append(missing, name)
	}
	return missing
}

// validateToolGrants checks the statically constructible worker slices before a
// run starts. Per-task MCP and protocol additions are checked from their actual
// final slices at construction time, because predicting them here would merely
// recreate the drift this guard exists to prevent.
func (c *Coordinator) validateToolGrants() error {
	if c == nil || c.session == nil {
		return nil
	}
	names := make([]string, 0, len(c.session.Agents))
	for name := range c.session.Agents {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		def := c.session.Agents[name]
		if def == nil || strings.EqualFold(def.Role, "coordinator") {
			continue // orchestrator grants are asserted by coordinatorAllowedToolNames
		}
		exposed := agentToolNames(c.selectWorkerTools(def))
		allowed := tools.GetToolsAllowed(c.withEffectiveToolsAllowed(context.Background(), def, exposed))
		if missing := unauthorizedExposedTools(exposed, allowed); len(missing) > 0 {
			return fmt.Errorf("agent %q would be shown tools it is not authorized to call: %s — every tool handed to a model must be in its runtime allowlist", name, strings.Join(missing, ", "))
		}
	}
	return nil
}
