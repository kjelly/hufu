package team

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/cost"
)

// AgentExecutionCostReservation is the durable identity committed before an
// external execution backend may prepare its execution world or start a
// provider process.
type AgentExecutionCostReservation struct {
	RunID                string
	ProviderInvocationID string
}

// AgentExecutionCostBoundary accounts for external execution backends that
// bypass Fantasy's admitted language-model wrapper.
type AgentExecutionCostBoundary interface {
	ReserveAgentExecution(context.Context, AttemptRequest) (AgentExecutionCostReservation, error)
	SettleAgentExecution(context.Context, AgentExecutionCostReservation, AttemptResult, error)
}

type coordinatorAgentExecutionCost struct {
	c *Coordinator
}

var _ AgentExecutionCostBoundary = coordinatorAgentExecutionCost{}

func (a coordinatorAgentExecutionCost) ReserveAgentExecution(ctx context.Context, request AttemptRequest) (AgentExecutionCostReservation, error) {
	if a.c == nil || a.c.costManager == nil {
		return AgentExecutionCostReservation{}, nil
	}
	runID := strings.TrimSpace(a.c.executionRunID)
	if runID == "" {
		return AgentExecutionCostReservation{}, fmt.Errorf("external cost reservation run identity is unavailable")
	}
	if requestRunID := strings.TrimSpace(request.RunID); requestRunID != "" && requestRunID != runID {
		return AgentExecutionCostReservation{}, fmt.Errorf("external cost reservation run %q does not match active run %q", requestRunID, runID)
	}
	invocationID, err := agent.NewProviderInvocationID()
	if err != nil {
		return AgentExecutionCostReservation{}, err
	}
	agentName, role := externalAttemptActor(request)
	identity := cost.InvocationIdentity{
		ProviderInvocationID: invocationID,
		RunID:                runID,
		TaskID:               strings.TrimSpace(request.TaskID),
		OccurrenceAttempt:    request.Attempt,
		Agent:                agentName,
		Role:                 role,
		Purpose:              cost.PurposeExternal,
	}
	if identity.TaskID == "" {
		identity.TaskID = strings.TrimSpace(request.Task.ID)
	}
	if identity.TaskID != "" && identity.OccurrenceAttempt < 1 {
		identity.OccurrenceAttempt = 1
	}
	if _, err := a.c.costManager.Reserve(ctx, a.c.EventJournal(), CostReservationRequest{
		Identity: identity, ExecutionTarget: request.ExecutionTarget.String(), Opaque: true,
	}); err != nil {
		return AgentExecutionCostReservation{}, err
	}
	return AgentExecutionCostReservation{RunID: runID, ProviderInvocationID: invocationID}, nil
}

func (a coordinatorAgentExecutionCost) SettleAgentExecution(ctx context.Context, reservation AgentExecutionCostReservation, result AttemptResult, attemptErr error) {
	if a.c == nil || a.c.costManager == nil || reservation.ProviderInvocationID == "" {
		return
	}
	_, _ = a.c.costManager.Settle(ctx, a.c.EventJournal(), CostSettlementRequest{
		RunID: reservation.RunID, ProviderInvocationID: reservation.ProviderInvocationID,
		Usage: externalAttemptCostUsage(result.Usage), Outcome: externalAttemptOutcome(ctx, attemptErr),
	})
}

func externalAttemptActor(request AttemptRequest) (string, string) {
	agentName := strings.TrimSpace(request.Task.Agent)
	role := ""
	if request.Agent != nil {
		if name := strings.TrimSpace(request.Agent.Name); name != "" {
			agentName = name
		}
		role = strings.TrimSpace(request.Agent.Role)
	}
	if agentName == "" {
		agentName = "external_agent"
	}
	if role == "" {
		role = "worker"
	}
	return agentName, role
}

func externalAttemptCostUsage(usage ExecutionUsage) *cost.TokenUsage {
	observed := cost.TokenUsage{
		InputTokens: int64(usage.InputTokens), CacheReadTokens: int64(usage.CacheReadTokens),
		CacheCreationTokens: int64(usage.CacheCreationTokens), OutputTokens: int64(usage.OutputTokens),
		TotalTokens: int64(usage.TotalTokens),
	}
	if usage.TotalTokens <= 0 || !completeCostUsage(observed) {
		return nil
	}
	return &observed
}

func externalAttemptOutcome(ctx context.Context, err error) cost.Outcome {
	if err == nil {
		return cost.OutcomeSuccess
	}
	if ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return cost.OutcomeCancelled
	}
	return cost.OutcomeProviderError
}
