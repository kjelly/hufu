package team

import (
	"context"
	"fmt"
	"sync"

	"github.com/kjelly/hufu/internal/execution"
)

// ExecutionRegistry is the sole worker-backend registry. It resolves trusted
// selectors before admission and never applies the legacy unknown-prefix
// fallback used internally by ProviderManager.
type ExecutionRegistry struct {
	mu       sync.RWMutex
	backends map[string]ExecutionBackend
}

func NewExecutionRegistry() *ExecutionRegistry {
	return &ExecutionRegistry{backends: make(map[string]ExecutionBackend)}
}

func (r *ExecutionRegistry) Register(backend ExecutionBackend) error {
	name, err := executionBackendName(backend)
	if err != nil {
		return fmt.Errorf("register execution backend: %w", err)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.backends == nil {
		r.backends = make(map[string]ExecutionBackend)
	}
	for existingName := range r.backends {
		if execution.BackendNamesEqual(existingName, name) {
			return fmt.Errorf("register execution backend: %q already registered", existingName)
		}
	}
	if _, exists := r.backends[name]; exists {
		return fmt.Errorf("register execution backend: %q already registered", name)
	}
	r.backends[name] = backend
	return nil
}

func (r *ExecutionRegistry) ResolveBackend(name string) (ExecutionBackend, error) {
	if r == nil {
		return nil, fmt.Errorf("execution registry is unavailable")
	}
	name = execution.CanonicalBackendName(name)
	if name == "" {
		return nil, fmt.Errorf("execution backend is required")
	}
	r.mu.RLock()
	backend := r.backends[name]
	if backend == nil && execution.IsOllamaBackend(name) {
		backend = r.backends[execution.OllamaBackendName]
		if backend == nil {
			backend = r.backends[execution.LegacyLocalBackendName]
		}
	}
	r.mu.RUnlock()
	if backend == nil {
		return nil, fmt.Errorf("unknown execution backend %q", name)
	}
	return backend, nil
}

func (r *ExecutionRegistry) ResolveTarget(selector execution.ExecutionSelector, defaults execution.TargetDefaults) (execution.ExecutionTarget, ExecutionBackend, error) {
	backendName := selector.Backend
	if backendName == "" {
		backendName = execution.CanonicalTargetBackendName(defaults.DefaultLLMBackend)
		if backendName == "" {
			return execution.ExecutionTarget{}, nil, fmt.Errorf("default LLM backend is required for bare execution selector %q", selector.Raw)
		}
	}
	backend, err := r.ResolveBackend(backendName)
	if err != nil {
		return execution.ExecutionTarget{}, nil, err
	}
	if selector.Backend == "" && backend.Kind() != execution.BackendKindLLM {
		return execution.ExecutionTarget{}, nil, fmt.Errorf("default LLM backend %q is not a language-model backend", backendName)
	}
	target := execution.ExecutionTarget{Backend: execution.CanonicalTargetBackendName(backend.Name()), Model: selector.Model}
	if err := backend.ValidateTarget(context.Background(), target); err != nil {
		return execution.ExecutionTarget{}, nil, fmt.Errorf("resolve execution target %q: %w", target, err)
	}
	return target, backend, nil
}

func (r *ExecutionRegistry) LanguageModelBackend(target execution.ExecutionTarget) (LanguageModelBackend, error) {
	backend, err := r.ResolveBackend(target.Backend)
	if err != nil {
		return nil, err
	}
	llm, ok := backend.(LanguageModelBackend)
	if !ok || !backend.Capabilities().DirectLanguageModel {
		return nil, fmt.Errorf("execution target %q does not provide a direct language model", target)
	}
	if err := llm.ValidateTarget(context.Background(), target); err != nil {
		return nil, err
	}
	return llm, nil
}
