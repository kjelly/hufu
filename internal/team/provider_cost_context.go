package team

import (
	"context"
	"fmt"
	"strings"

	"github.com/kjelly/hufu/internal/cost"
	"github.com/kjelly/hufu/internal/tools"
)

type providerCostContext struct {
	Agent   string
	Role    string
	Purpose cost.Purpose
}

type providerCostContextKey struct{}

func withProviderCostContext(ctx context.Context, identity providerCostContext) context.Context {
	return context.WithValue(ctx, providerCostContextKey{}, identity)
}

func (c *Coordinator) providerCostIdentity(ctx context.Context, invocationID string) (cost.InvocationIdentity, error) {
	identity := cost.InvocationIdentity{
		ProviderInvocationID: invocationID,
		RunID:                strings.TrimSpace(c.executionRunID),
		Purpose:              cost.PurposeWorker,
	}
	metadata, _ := invocationMetadataFromContext(ctx)
	identity.TaskID = strings.TrimSpace(metadata.TaskID)
	identity.OccurrenceAttempt = metadata.Attempt
	identity.Agent = strings.TrimSpace(metadata.AgentName)
	identity.Role = strings.TrimSpace(metadata.AgentRole)
	identity.Purpose = providerCostPurpose(metadata.Purpose)

	if todoID, _ := ctx.Value(todoIDKey{}).(string); identity.TaskID == "" {
		identity.TaskID = strings.TrimSpace(todoID)
	}
	if attempt, _ := ctx.Value(executionAttemptKey{}).(int); identity.OccurrenceAttempt < 1 {
		identity.OccurrenceAttempt = attempt
	}
	if agentName, _ := ctx.Value(tools.AgentNameKey).(string); identity.Agent == "" {
		identity.Agent = strings.TrimSpace(agentName)
	}
	if explicit, ok := ctx.Value(providerCostContextKey{}).(providerCostContext); ok {
		if strings.TrimSpace(explicit.Agent) != "" {
			identity.Agent = strings.TrimSpace(explicit.Agent)
		}
		if strings.TrimSpace(explicit.Role) != "" {
			identity.Role = strings.TrimSpace(explicit.Role)
		}
		if explicit.Purpose != "" {
			identity.Purpose = explicit.Purpose
		}
	}
	if protocolRepairExecution(ctx) {
		identity.Purpose = cost.PurposeRepair
	}
	if identity.Agent == "" {
		identity.Agent = string(identity.Purpose)
	}
	if identity.Role == "" {
		identity.Role = c.providerCostAgentRole(identity.Agent, identity.Purpose)
	}
	if identity.TaskID != "" && identity.OccurrenceAttempt < 1 {
		identity.OccurrenceAttempt = 1
	}
	if identity.RunID == "" {
		return cost.InvocationIdentity{}, fmt.Errorf("cost reservation run identity is unavailable")
	}
	return identity, nil
}

func (c *Coordinator) providerCostAgentRole(agentName string, purpose cost.Purpose) string {
	if purpose == cost.PurposeCoordinator {
		return "coordinator"
	}
	if isAuxiliaryCostPurpose(purpose) {
		return "auxiliary"
	}
	if c != nil && c.session != nil {
		if def := c.session.Agents[strings.ToLower(strings.TrimSpace(agentName))]; def != nil && strings.TrimSpace(def.Role) != "" {
			return strings.TrimSpace(def.Role)
		}
	}
	return "worker"
}

func providerCostPurpose(purpose string) cost.Purpose {
	switch strings.ToLower(strings.TrimSpace(purpose)) {
	case "coordinator", "orchestrator":
		return cost.PurposeCoordinator
	case "direct_agent":
		return cost.PurposeDirectAgent
	case "judge":
		return cost.PurposeJudge
	case "skeptic":
		return cost.PurposeSkeptic
	case "guard", "guard_reviewer", "path_reviewer":
		return cost.PurposeGuard
	case "plan_reviewer":
		return cost.PurposePlanReviewer
	case "protocol_repair":
		return cost.PurposeRepair
	case "external_agent":
		return cost.PurposeExternal
	case "", "task_execution", "task_retry", "recovery":
		return cost.PurposeWorker
	default:
		return cost.PurposeSidecar
	}
}

func isAuxiliaryCostPurpose(purpose cost.Purpose) bool {
	switch purpose {
	case cost.PurposeSidecar, cost.PurposeGuard, cost.PurposeJudge, cost.PurposeSkeptic, cost.PurposePlanReviewer:
		return true
	default:
		return false
	}
}
