package team

import (
	"context"
	"fmt"
	"slices"
	"strings"

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
	if p == nil || p.coordinator == nil {
		return AttemptResult{}, fmt.Errorf("hufu-local attempt requires a coordinator")
	}
	// RunAttempt is a public provider entrypoint. Keep it behind the same
	// event-first admission boundary as coordinator dispatch and other
	// providers, before any request validation can progress to an LLM call.
	if err := p.coordinator.AdmitExecutionPolicy(); err != nil {
		return AttemptResult{}, fmt.Errorf("admit execution policy before hufu-local attempt: %w", err)
	}
	ctx = withoutCoordinatorRequestPreflight(ctx)
	if request.TaskID == "" || request.Attempt < 1 || request.ModelID == "" {
		return AttemptResult{}, fmt.Errorf("hufu-local attempt has an incomplete contract")
	}
	canonical, err := p.coordinator.canonicalWorkerAttemptContextForID(request.TaskID)
	if err != nil {
		return AttemptResult{}, err
	}
	expectedModelID := canonical.Task.Model
	if !canonical.Task.ResolvedExecutionTarget.IsZero() {
		expectedModelID = p.coordinator.executionModelIDForTarget(canonical.Task.ResolvedExecutionTarget, expectedModelID)
	}
	if strings.TrimSpace(expectedModelID) != "" && strings.TrimSpace(request.ModelID) != strings.TrimSpace(expectedModelID) {
		return AttemptResult{}, fmt.Errorf("hufu-local attempt model assertion does not match canonical model %q for Todo %q", expectedModelID, request.TaskID)
	}
	if !workerAgentResolutionAssertionMatches(request.Agent, canonical.Agent) {
		return AttemptResult{}, fmt.Errorf("hufu-local attempt agent assertion does not match canonical agent for Todo %q", request.TaskID)
	}
	if !workerToolResolutionTaskMatches(request.Task, canonical.Task) {
		return AttemptResult{}, fmt.Errorf("hufu-local attempt task assertion does not match canonical task for Todo %q", request.TaskID)
	}
	if workerToolResolutionModeForTask(request.Task) != canonical.Mode {
		return AttemptResult{}, fmt.Errorf("hufu-local attempt lifecycle assertion does not match canonical lifecycle for Todo %q", request.TaskID)
	}
	// The caller's resolved surface is an assertion only. Resolve and construct
	// the exact final worker surface from the canonical Todo and loaded agent.
	verified, err := p.coordinator.ToolResolver().ResolveTaskTools(ctx, canonical.Agent, WorkerToolResolutionRequest{
		Task: canonical.Task, TodoID: canonical.Todo.ID, Mode: canonical.Mode,
	})
	if err != nil {
		return AttemptResult{}, fmt.Errorf("hufu-local attempt tool resolution: %w", err)
	}
	if len(request.Tools.Names) != len(request.Tools.Tools) {
		return AttemptResult{}, fmt.Errorf("hufu-local attempt tool surface names do not match concrete tools")
	}
	for i, tool := range request.Tools.Tools {
		if tool == nil {
			return AttemptResult{}, fmt.Errorf("hufu-local attempt tool surface contains nil tool at index %d", i)
		}
		if request.Tools.Names[i] != strings.TrimSpace(tool.Info().Name) {
			return AttemptResult{}, fmt.Errorf("hufu-local attempt tool surface name %q does not match concrete tool %q", request.Tools.Names[i], tool.Info().Name)
		}
	}
	if !slices.Equal(request.Tools.Names, verified.Names) {
		return AttemptResult{}, fmt.Errorf("hufu-local attempt tool surface %v does not match canonical surface %v", request.Tools.Names, verified.Names)
	}
	if len(verified.Tools) == 0 {
		return AttemptResult{}, fmt.Errorf("hufu-local attempt has no authorized tools")
	}
	def := canonical.Agent
	def = p.coordinator.injectWorkerContext(ctx, def)
	gatedTools := p.coordinator.gatePolicyTools(verified.Tools)
	ctx, invocation, err := p.coordinator.resolveProviderBoundInvocationContext(ctx, request.ModelID, def)
	if err != nil {
		return AttemptResult{}, err
	}
	ctx = withContextWindowRequestDescriptor(ctx, p.coordinator.newContextWindowRequestDescriptorWithContext(ctx, request.ModelID, def, gatedTools, canonical.Agent.Name, "subagent"))
	// Install authorization from the verified concrete surface before the
	// gated constructor runs. This also preserves agent-specific MCP aliases.
	ctx = p.coordinator.withEffectiveToolsAllowedForTask(ctx, def, verified.Names, canonical.Task)
	// The local Fantasy adapter is still a compatibility implementation of the
	// LLM execution backend, but it must not select a provider through the
	// retired ModelRuntime seam. Prefer the immutable target carried by the
	// attempt; targetless direct adapter fixtures are resolved through the same
	// registry using the request's model for legacy compatibility.
	var provider *agent.OpenAICompatibleProvider
	if !request.ExecutionTarget.IsZero() {
		if !canonical.Task.ResolvedExecutionTarget.IsZero() && request.ExecutionTarget != canonical.Task.ResolvedExecutionTarget {
			return AttemptResult{}, fmt.Errorf("hufu-local attempt execution target does not match canonical target for Todo %q", request.TaskID)
		}
		gatedBackend, target, targetErr := p.coordinator.gatedAgentBackendForTarget(request.ExecutionTarget)
		if targetErr != nil {
			return AttemptResult{}, targetErr
		}
		provider, targetErr = gatedBackend.AgentProvider(ctx, target)
		if targetErr != nil {
			return AttemptResult{}, targetErr
		}
	} else {
		gatedBackend, target, targetErr := p.coordinator.gatedAgentBackendForModel(request.ModelID)
		if targetErr != nil {
			return AttemptResult{}, targetErr
		}
		provider, targetErr = gatedBackend.AgentProvider(ctx, target)
		if targetErr != nil {
			return AttemptResult{}, targetErr
		}
	}
	ag, err := p.coordinator.createGatedAgent(ctx, provider, agent.AgentConfig{
		Def:               def,
		TeamConfig:        &p.coordinator.session.Config,
		WorkDir:           p.coordinator.projectDir,
		MaxSteps:          request.MaxSteps,
		InvocationModelID: request.ModelID,
		AdmissionContext:  invocation.AdmissionContext,
	}, gatedTools)
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
	output, steps, runErr := p.coordinator.runAgentWithStatusAndHistory(ctx, ag, canonical.Agent.Name, request.Prompt, request.History, timing)
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
