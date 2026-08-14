package team

import (
	"fmt"
	"strings"
	"sync"
)

const localSubagentProviderName = "hufu-local"

// SubagentRegistry resolves production providers by explicit key. Unknown
// keys fail closed before a model call or any side effect can occur.
type SubagentRegistry struct {
	mu        sync.RWMutex
	providers map[string]SubagentProvider
}

func NewSubagentRegistry(local SubagentProvider) *SubagentRegistry {
	r := &SubagentRegistry{providers: make(map[string]SubagentProvider)}
	if local != nil {
		r.providers[localSubagentProviderName] = local
	}
	return r
}

func (r *SubagentRegistry) Register(provider SubagentProvider) error {
	if r == nil || provider == nil || strings.TrimSpace(provider.Name()) == "" {
		return fmt.Errorf("register subagent provider: provider name is required")
	}
	name := strings.ToLower(strings.TrimSpace(provider.Name()))
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.providers == nil {
		r.providers = make(map[string]SubagentProvider)
	}
	if _, exists := r.providers[name]; exists {
		return fmt.Errorf("register subagent provider: %q already registered", name)
	}
	r.providers[name] = provider
	return nil
}

func (r *SubagentRegistry) Resolve(name string) (SubagentProvider, error) {
	if r == nil {
		return nil, fmt.Errorf("subagent registry is unavailable")
	}
	name = strings.ToLower(strings.TrimSpace(name))
	if name == "" {
		name = localSubagentProviderName
	}
	r.mu.RLock()
	provider := r.providers[name]
	r.mu.RUnlock()
	if provider == nil {
		return nil, fmt.Errorf("unknown subagent provider %q", name)
	}
	return provider, nil
}
