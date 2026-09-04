package team

import (
	"context"
	"fmt"

	"charm.land/fantasy"

	"github.com/kjelly/hufu/internal/agent"
)

// providerRequestAdmission is the team-owned policy adapter for the agent
// package's provider wrapper. It applies one capacity rule to workers,
// direct agents, repairs, extra models, and sidecars.
type providerRequestAdmission struct {
	c *Coordinator
}

func (c *Coordinator) providerAdmission() agent.RequestAdmission {
	if c == nil {
		return nil
	}
	return providerRequestAdmission{c: c}
}

func (a providerRequestAdmission) AdmitProviderRequest(ctx context.Context, request agent.ProviderRequest) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if bound := request.AdmissionContext; bound.IsBound() && bound.ModelID != "" && bound.ModelID != request.ModelID {
		return fmt.Errorf("provider admission context model %q does not match request model %q", bound.ModelID, request.ModelID)
	}
	spec := modelContextSpecForProviderRequest(request)
	if spec.IsEstimated && spec.ContextWindow <= 0 {
		return &ContextWindowMetadataUnavailableError{ModelID: request.ModelID}
	}
	if spec.ContextWindow <= 0 {
		return &ContextWindowMetadataUnavailableError{ModelID: request.ModelID}
	}

	requestTokens, err := defaultCounter.CountProviderRequest(ctx, request.ModelID, request)
	if err != nil {
		return fmt.Errorf("count provider request for %q: %w", request.ModelID, err)
	}
	reserve := spec.MaxOutputTokens
	if request.Call != nil && request.Call.MaxOutputTokens != nil && *request.Call.MaxOutputTokens > 0 {
		reserve = int(*request.Call.MaxOutputTokens)
	}
	if request.ObjectCall != nil && request.ObjectCall.MaxOutputTokens != nil && *request.ObjectCall.MaxOutputTokens > 0 {
		reserve = int(*request.ObjectCall.MaxOutputTokens)
	}
	margin := spec.SafetyMarginTokens
	available := spec.ContextWindow - reserve - margin
	if available < 0 {
		available = 0
	}
	if requestTokens > available {
		return &CannotFitError{
			ModelID:       request.ModelID,
			RequestTokens: requestTokens,
			Available:     available,
			ProvenNoSend:  true,
		}
	}
	return nil
}

// AcquireProviderInvocation implements the optional Coordinator-owned runtime
// boundary used by the agent package's admitted language-model wrapper. The
// wrapper calls this only after request admission and releases the returned
// slot when the underlying provider call returns.
func (a providerRequestAdmission) AcquireProviderInvocation(ctx context.Context, modelID string) (func(), error) {
	if a.c == nil {
		return nil, nil
	}
	slot, err := acquireSem(ctx, a.c.providerSemaphore(modelID))
	if err != nil {
		return nil, err
	}
	return slot.release, nil
}

func modelContextSpecForProviderRequest(request agent.ProviderRequest) ModelContextSpec {
	bound := request.AdmissionContext
	if bound.IsBound() {
		return ModelContextSpec{
			ModelID:             request.ModelID,
			ContextWindow:       bound.ContextWindow,
			ContextWindowSource: bound.ContextWindowSource,
			MaxOutputTokens:     bound.MaxOutputTokens,
			SafetyMarginTokens:  bound.SafetyMarginTokens,
			Estimator:           bound.Estimator,
			IsEstimated:         bound.IsEstimated,
		}
	}
	return globalRegistry.GetSpec(request.ModelID)
}

// providerCallFromContextRequest builds the exact input shape used by the
// shared admission counter for coordinator preflight and stream admission.
func providerCallFromContextRequest(modelID, system, prompt string, messages []fantasy.Message, tools []fantasy.AgentTool) agent.ProviderRequest {
	request := agent.ProviderRequest{ModelID: modelID, Messages: cloneMessages(messages), Tools: tools}
	if system != "" {
		request.Messages = replaceOrPrependSystem(request.Messages, system)
	}
	if prompt != "" && !hasExactUserMessage(request.Messages, prompt) {
		request.Messages = append(request.Messages, fantasy.NewUserMessage(prompt))
	}
	return request
}

func replaceOrPrependSystem(messages []fantasy.Message, system string) []fantasy.Message {
	for i := range messages {
		if messages[i].Role == fantasy.MessageRoleSystem {
			updated := cloneMessages(messages)
			updated[i] = fantasy.NewSystemMessage(system)
			return updated
		}
	}
	return append([]fantasy.Message{fantasy.NewSystemMessage(system)}, messages...)
}
