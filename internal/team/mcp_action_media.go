package team

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"strings"

	"charm.land/fantasy"
	"github.com/kjelly/hufu/internal/mcp"
	"github.com/kjelly/hufu/internal/media"
)

// pendingMCPActionMedia exists only between the trusted MCP adapter and the
// coordinator's post-Outputs validation publication boundary. Exported JSON
// action providers cannot manufacture it or copy server-provided provenance.
type pendingMCPActionMedia struct {
	tool       mcp.MCPTool
	result     mcp.RuntimeToolResult
	capability string
	actionID   string
}

func verifyMCPActionMediaSource(event RunEvent, p ToolMediaPublishedPayload, item *TodoItem, policy ExecutionPolicySnapshot, prior []RunEvent) error {
	capability := normalizeCapability(item.Action.Capability)
	bound := false
	for _, provider := range policy.MCPActionProviders {
		if provider.Capability == capability && provider.Server+"__"+provider.Tool == p.ToolName && provider.DescriptorSHA256 == p.DescriptorSHA256 {
			bound = true
			break
		}
	}
	if !bound || p.ToolCallID != p.ActionInvocationID {
		return media.Reject(media.AccessDenied, "native image source differs from the frozen MCP action provider")
	}
	started := false
	for _, source := range prior {
		if source.Type != "action_started" || source.RunID != event.RunID || source.TaskID != event.TaskID {
			continue
		}
		var payload LifecycleEventPayload
		if err := json.Unmarshal(source.Payload, &payload); err != nil {
			return err
		}
		started = payload.ActionID == p.ActionInvocationID && payload.Capability == capability && payload.ToolName == item.Action.Type
	}
	if !started {
		return media.Reject(media.AccessDenied, "native image source has no current action invocation")
	}
	return nil
}

type mcpActionMediaGrantKey struct{}

// This grant is constructed only by the coordinator after native Outputs
// validation. It is not the provider-controlled ActionEnvironment context key.
type mcpActionMediaGrant struct {
	environment ActionEnvironment
	identity    submitResultRuntimeIdentity
	capability  string
	descriptor  string
}

func (c *Coordinator) publishPendingMCPActionMedia(ctx context.Context, env ActionEnvironment, pending *pendingMCPActionMedia) (*RuntimeMediaProjection, error) {
	if pending == nil {
		return nil, nil
	}
	item := c.todoItemByID(env.TaskID)
	if item == nil || item.Action == nil || c.phaseWorkflow == nil || pending.actionID == "" || pending.actionID != env.ActionInvocationID || pending.capability != normalizeCapability(item.Action.Capability) {
		return nil, media.Reject(media.AccessDenied, "native image result has no bound action invocation")
	}
	descriptor, err := mcp.MCPToolDescriptorSHA256(pending.tool)
	if err != nil || descriptor != c.phaseWorkflow.providerDescriptorDigest(pending.capability) {
		return nil, media.Reject(media.AccessDenied, "native image result descriptor changed after execution")
	}
	identity := submitResultRuntimeIdentity{RunID: env.RunID, TaskID: env.TaskID, Attempt: env.Attempt, Agent: strings.ToLower(item.Agent),
		OccurrenceRevision: item.OccurrenceRevision, DispatchID: item.DispatchID}
	ctx = context.WithValue(ctx, mcpActionMediaGrantKey{}, mcpActionMediaGrant{environment: env, identity: identity, capability: pending.capability, descriptor: descriptor})
	if _, err := c.publishToolMediaResult(ctx, pending.tool, fantasy.ToolCall{ID: env.ActionInvocationID, Name: pending.tool.Name}, pending.result); err != nil {
		return nil, fmt.Errorf("%s: %w", mcpActionResultInvalid, err)
	}
	projection, err := c.runtimeMediaForOccurrence(ctx, identity, env.ActionInvocationID)
	if err != nil {
		return nil, err
	}
	if projection == nil {
		return nil, media.Reject(media.DurabilityRequired, "native image result lost its canonical publication")
	}
	return projection, nil
}

func (c *Coordinator) mediaPublicationOccurrence(ctx context.Context) (submitResultRuntimeIdentity, *TodoItem, error) {
	grant, action := ctx.Value(mcpActionMediaGrantKey{}).(mcpActionMediaGrant)
	if !action {
		return c.toolMediaOccurrence(ctx)
	}
	identity, env := grant.identity, grant.environment
	item := c.todoItemByID(identity.TaskID)
	if ctx.Err() != nil {
		return identity, nil, ctx.Err()
	}
	if item == nil || item.Action == nil || item.Status != TaskInProgress || identity.RunID != coordinatorRuntimeRunID(c) ||
		identity.TaskID != env.TaskID || identity.RunID != env.RunID || identity.Attempt != env.Attempt || identity.Attempt < 1 || identity.DispatchID == "" ||
		identity.OccurrenceRevision != item.OccurrenceRevision || identity.DispatchID != item.DispatchID || !strings.EqualFold(identity.Agent, item.Agent) ||
		env.ActionInvocationID == "" || normalizeCapability(item.Action.Capability) != grant.capability || c.phaseWorkflow == nil || c.phaseWorkflow.providerDescriptorDigest(grant.capability) != grant.descriptor {
		return identity, nil, media.Reject(media.AccessDenied, "native image publisher has no current action occurrence")
	}
	return identity, item, nil
}
