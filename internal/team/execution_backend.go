package team

import (
	"context"
	"fmt"
	"strings"

	"charm.land/fantasy"
	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/execution"
)

// ExecutionBackend runs one already-authorized Hufu worker attempt. Hufu
// keeps lifecycle, verification, receipts, recovery, and policy ownership.
type ExecutionBackend interface {
	Name() string
	Kind() execution.BackendKind
	Capabilities() execution.BackendCapabilities
	ValidateTarget(context.Context, execution.ExecutionTarget) error
	RunAttempt(context.Context, AttemptRequest) (AttemptResult, error)
}

// LanguageModelBackend is deliberately optional: an agent execution backend
// such as Codex cannot be used for coordinator or auxiliary LLM roles merely
// because it can execute worker attempts.
type LanguageModelBackend interface {
	ExecutionBackend
	LanguageModel(context.Context, execution.ExecutionTarget) (fantasy.LanguageModel, error)
}

// ModelCatalogBackend is the optional diagnostics capability exposed by an
// LLM backend. Keeping model discovery behind the backend prevents preflight
// code from reaching into ProviderManager and accidentally becoming a second
// provider-selection authority.
type ModelCatalogBackend interface {
	LanguageModelBackend
	ListModelNames(context.Context, execution.ExecutionTarget) ([]string, error)
}

// GatedAgentBackend is the private worker-runtime capability required to
// construct a Fantasy worker. It keeps direct/nested worker creation behind
// the same canonical LLM backend selected by ExecutionRegistry.
type GatedAgentBackend interface {
	LanguageModelBackend
	AgentProvider(context.Context, execution.ExecutionTarget) (*agent.OpenAICompatibleProvider, error)
}

func (c *Coordinator) gatedAgentBackendForModel(model string) (GatedAgentBackend, execution.ExecutionTarget, error) {
	target, err := c.resolveCanonicalTaskTarget(model, "")
	if err != nil {
		return nil, execution.ExecutionTarget{}, err
	}
	return c.gatedAgentBackendForTarget(target)
}

func (c *Coordinator) gatedAgentBackendForTarget(target execution.ExecutionTarget) (GatedAgentBackend, execution.ExecutionTarget, error) {
	backend, err := c.ExecutionRegistry().LanguageModelBackend(target)
	if err != nil {
		return nil, execution.ExecutionTarget{}, err
	}
	gatedBackend, ok := backend.(GatedAgentBackend)
	if !ok {
		return nil, execution.ExecutionTarget{}, fmt.Errorf("execution backend %q cannot construct a gated Fantasy worker", target.Backend)
	}
	return gatedBackend, target, nil
}

// LLMExecutionBackend adapts the existing OpenAI-compatible provider manager
// and Hufu/Fantasy worker runner behind the unified execution boundary.
type LLMExecutionBackend struct {
	name           string
	manager        *agent.ProviderManager
	runner         AttemptRunner
	admitExecution func() error
}

// NewLLMExecutionBackend binds every transport-capable accessor to the
// coordinator's execution-policy admission boundary. A nil admission function
// is rejected so a new backend cannot accidentally reintroduce a raw provider
// escape hatch.
func NewLLMExecutionBackend(name string, manager *agent.ProviderManager, runner AttemptRunner, admitExecution func() error) (*LLMExecutionBackend, error) {
	name = execution.CanonicalTargetBackendName(name)
	if name == "" {
		return nil, fmt.Errorf("LLM execution backend name is required")
	}
	if manager == nil {
		return nil, fmt.Errorf("LLM execution backend %q requires a provider manager", name)
	}
	if runner == nil {
		return nil, fmt.Errorf("LLM execution backend %q requires an attempt runner", name)
	}
	if admitExecution == nil {
		return nil, fmt.Errorf("LLM execution backend %q requires execution-policy admission", name)
	}
	return &LLMExecutionBackend{name: name, manager: manager, runner: runner, admitExecution: admitExecution}, nil
}

func (b *LLMExecutionBackend) Name() string { return b.name }

func (*LLMExecutionBackend) Kind() execution.BackendKind { return execution.BackendKindLLM }

func (*LLMExecutionBackend) Capabilities() execution.BackendCapabilities {
	return execution.BackendCapabilities{DirectLanguageModel: true, StructuredResult: true, WorkspaceRead: true, WorkspaceWrite: true, Shell: true}
}

func (b *LLMExecutionBackend) ValidateTarget(_ context.Context, target execution.ExecutionTarget) error {
	if b == nil || b.manager == nil || b.runner == nil {
		return fmt.Errorf("LLM execution backend is unavailable")
	}
	if err := target.Validate(); err != nil {
		return err
	}
	if !execution.BackendNamesEqual(target.Backend, b.name) {
		return fmt.Errorf("execution target %q does not belong to LLM backend %q", target, b.name)
	}
	return nil
}

func (b *LLMExecutionBackend) LanguageModel(ctx context.Context, target execution.ExecutionTarget) (fantasy.LanguageModel, error) {
	provider, err := b.AgentProvider(ctx, target)
	if err != nil {
		return nil, err
	}
	return provider.LanguageModel(ctx, target.Model)
}

func (b *LLMExecutionBackend) ListModelNames(ctx context.Context, target execution.ExecutionTarget) ([]string, error) {
	provider, err := b.AgentProvider(ctx, target)
	if err != nil {
		return nil, err
	}
	return provider.ListModelNames(ctx)
}

// AgentProvider exposes the existing gated-agent constructor dependency only
// after unified target validation and execution-policy admission have both
// succeeded. Returning an error, rather than a nil provider, keeps the
// fail-closed boundary observable to every caller.
func (b *LLMExecutionBackend) AgentProvider(ctx context.Context, target execution.ExecutionTarget) (*agent.OpenAICompatibleProvider, error) {
	if err := b.ValidateTarget(ctx, target); err != nil {
		return nil, err
	}
	if err := b.admit(); err != nil {
		return nil, fmt.Errorf("admit execution policy before provider access: %w", err)
	}
	return b.manager.GetProvider(b.providerModelID(target)), nil
}

func (b *LLMExecutionBackend) RunAttempt(ctx context.Context, request AttemptRequest) (AttemptResult, error) {
	if b == nil || b.runner == nil {
		return AttemptResult{}, fmt.Errorf("LLM execution backend is unavailable")
	}
	if err := b.ValidateTarget(ctx, request.ExecutionTarget); err != nil {
		return AttemptResult{}, err
	}
	if err := b.admit(); err != nil {
		return AttemptResult{}, fmt.Errorf("admit execution policy before LLM attempt: %w", err)
	}
	request.ModelID = b.providerModelID(request.ExecutionTarget)
	return b.runner.RunAttempt(ctx, request)
}

func (b *LLMExecutionBackend) admit() error {
	if b == nil || b.admitExecution == nil {
		return fmt.Errorf("LLM execution backend is missing execution-policy admission")
	}
	return b.admitExecution()
}

func (b *LLMExecutionBackend) providerModelID(target execution.ExecutionTarget) string {
	if execution.IsOllamaBackend(target.Backend) {
		return target.Model
	}
	return b.name + "/" + target.Model
}

// AgentExecutionBackend adapts an existing external SubagentProvider without
// reimplementing its protocol or weakening its result canonicalization.
type AgentExecutionBackend struct {
	name     string
	provider SubagentProvider
}

func NewAgentExecutionBackend(name string, provider SubagentProvider) (*AgentExecutionBackend, error) {
	name = execution.CanonicalBackendName(name)
	if name == "" {
		return nil, fmt.Errorf("agent execution backend name is required")
	}
	if provider == nil {
		return nil, fmt.Errorf("agent execution backend %q requires a provider", name)
	}
	return &AgentExecutionBackend{name: name, provider: provider}, nil
}

func (b *AgentExecutionBackend) Name() string { return b.name }

func (*AgentExecutionBackend) Kind() execution.BackendKind { return execution.BackendKindAgent }

func (b *AgentExecutionBackend) Capabilities() execution.BackendCapabilities {
	if b == nil || b.provider == nil {
		return execution.BackendCapabilities{}
	}
	caps := b.provider.Capabilities()
	return execution.BackendCapabilities{StructuredResult: caps.SupportsTypedResult, WorkspaceRead: true, WorkspaceWrite: true, Resume: caps.SupportsResumeToken}
}

func (b *AgentExecutionBackend) ValidateTarget(_ context.Context, target execution.ExecutionTarget) error {
	if b == nil || b.provider == nil {
		return fmt.Errorf("agent execution backend is unavailable")
	}
	if err := target.Validate(); err != nil {
		return err
	}
	if target.Backend != b.name {
		return fmt.Errorf("execution target %q does not belong to agent backend %q", target, b.name)
	}
	return nil
}

func (b *AgentExecutionBackend) RunAttempt(ctx context.Context, request AttemptRequest) (AttemptResult, error) {
	if b == nil || b.provider == nil {
		return AttemptResult{}, fmt.Errorf("agent execution backend is unavailable")
	}
	if err := b.ValidateTarget(ctx, request.ExecutionTarget); err != nil {
		return AttemptResult{}, err
	}
	request.ModelID = request.ExecutionTarget.Model
	request.Provider = b.name
	request.ProviderBinding = providerBindingFromBackendBinding(request.BackendBinding)
	return b.provider.RunAttempt(ctx, request)
}

func executionBackendName(backend ExecutionBackend) (string, error) {
	if backend == nil {
		return "", fmt.Errorf("execution backend is required")
	}
	name := execution.CanonicalBackendName(backend.Name())
	if name == "" || strings.TrimSpace(backend.Name()) != backend.Name() {
		return "", fmt.Errorf("execution backend name is required")
	}
	return name, nil
}
