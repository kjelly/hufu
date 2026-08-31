package team

import (
	"context"
	"strconv"
	"strings"

	"charm.land/fantasy"

	"github.com/kjelly/hufu/internal/agent"
)

// prepareAgentModelRequest is installed by createGatedAgent, making Fantasy's
// Generate and Stream paths cross the same pre-provider admission boundary.
// It intentionally does not retry or repair an overflow after provider entry.
func (c *Coordinator) prepareAgentModelRequest(cfg agent.AgentConfig, agentTools []fantasy.AgentTool) fantasy.PrepareStepFunction {
	if c == nil || cfg.Def == nil || cfg.TeamConfig == nil {
		return nil
	}
	manager := NewContextWindowManager(defaultCounter, nil)
	system := cfg.Def.System
	reserved := effectiveAgentMaxOutput(cfg)
	return func(ctx context.Context, options fantasy.PrepareStepFunctionOptions) (context.Context, fantasy.PrepareStepResult, error) {
		modelID := ""
		if options.Model != nil {
			modelID = options.Model.Model()
		}
		if modelID == "" {
			modelID = cfg.Def.Generation.Model
		}
		if modelID == "" {
			modelID = cfg.TeamConfig.Generation.Model
		}
		taskID, _ := ctx.Value(todoIDKey{}).(string)
		attempt, _ := ctx.Value(executionAttemptKey{}).(int)
		admission, err := c.admitCoordinatorContext(ctx, manager, ContextWindowRequest{
			ModelID: modelID, System: system, Tools: agentTools, Messages: options.Messages,
			ReservedOutputTokens: reserved, StepNumber: options.StepNumber,
		}, "agent", taskID, attempt)
		if err != nil {
			return ctx, fantasy.PrepareStepResult{}, err
		}
		if admission.Decision == ContextWindowCannotFit {
			return ctx, fantasy.PrepareStepResult{}, &CannotFitError{
				ModelID: modelID, RequestTokens: admission.RequestTokens,
				Available: admission.Budget.Available, ProvenNoSend: true,
			}
		}
		return ctx, fantasy.PrepareStepResult{Messages: admission.Messages}, nil
	}
}

func effectiveAgentMaxOutput(cfg agent.AgentConfig) int {
	if cfg.Def != nil {
		if value, err := strconv.Atoi(strings.TrimSpace(cfg.Def.Generation.MaxTokens)); err == nil && value > 0 {
			return value
		}
	}
	if cfg.TeamConfig != nil {
		if value, err := strconv.Atoi(strings.TrimSpace(cfg.TeamConfig.Generation.MaxTokens)); err == nil && value > 0 {
			return value
		}
	}
	return 0
}
