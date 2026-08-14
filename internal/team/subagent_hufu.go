package team

import (
	"context"
	"fmt"

	"github.com/kjelly/hufu/internal/agent"
)

// HufuLocalSubagentProvider adapts the existing Fantasy agent runtime. It is
// Hufu-owned and always uses createGatedAgent, so provider migration cannot
// bypass the central tool policy gate.
type HufuLocalSubagentProvider struct{ coordinator *Coordinator }

func NewHufuLocalSubagentProvider(c *Coordinator) *HufuLocalSubagentProvider {
	return &HufuLocalSubagentProvider{coordinator: c}
}

func (p *HufuLocalSubagentProvider) Name() string { return localSubagentProviderName }

func (p *HufuLocalSubagentProvider) Capabilities() SubagentCapabilities {
	return SubagentCapabilities{SupportsHufuTools: true, SupportsTypedResult: true, SupportsActivities: true}
}

func (p *HufuLocalSubagentProvider) RunAttempt(ctx context.Context, request AttemptRequest) (AttemptResult, error) {
	if p == nil || p.coordinator == nil || request.Agent == nil {
		return AttemptResult{}, fmt.Errorf("hufu-local attempt requires a coordinator and agent")
	}
	if request.TaskID == "" || request.Attempt < 1 || request.ModelID == "" {
		return AttemptResult{}, fmt.Errorf("hufu-local attempt has an incomplete contract")
	}
	if len(request.Tools.Tools) == 0 {
		return AttemptResult{}, fmt.Errorf("hufu-local attempt has no authorized tools")
	}
	provider, err := p.coordinator.ModelRuntime().ProviderFor(request.ModelID)
	if err != nil {
		return AttemptResult{}, err
	}
	def := request.Agent
	def = p.coordinator.injectWorkerContext(ctx, def)
	ag, err := p.coordinator.createGatedAgent(ctx, provider, agent.AgentConfig{
		Def:        def,
		TeamConfig: &p.coordinator.session.Config,
		WorkDir:    p.coordinator.projectDir,
		MaxSteps:   request.MaxSteps,
	}, request.Tools.Tools)
	if err != nil {
		return AttemptResult{}, err
	}
	if request.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, request.Timeout)
		defer cancel()
	}
	timing := request.timing
	if timing == nil {
		timing = &taskTiming{}
		timing.reset()
	}
	output, steps, runErr := p.coordinator.runAgentWithStatusAndHistory(ctx, ag, request.Agent.Name, request.Prompt, request.History, timing)
	result := AttemptResult{Output: output, StepsUsed: len(steps), steps: steps, agent: ag}
	if typed := p.coordinator.GetTaskResult(request.TaskID); typed != nil {
		copy := *typed
		result.TypedResult = &copy
	}
	for _, step := range steps {
		result.Usage.InputTokens += int(step.Usage.InputTokens)
		result.Usage.OutputTokens += int(step.Usage.OutputTokens)
		result.Usage.TotalTokens += int(step.Usage.TotalTokens)
	}
	if runErr != nil {
		return result, runErr
	}
	return result, nil
}

var _ SubagentProvider = (*HufuLocalSubagentProvider)(nil)
