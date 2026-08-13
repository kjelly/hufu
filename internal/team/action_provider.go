package team

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/kjelly/hufu/internal/agent"
)

// Action represents a generic structured action intent.
type Action struct {
	// Capability selects the registered provider. It is bound by a static team
	// contract and is never accepted from the coordinator's agent tool.
	Capability string `json:"capability" yaml:"capability"`
	Type       string `json:"type" yaml:"type"`
	Payload    string `json:"payload" yaml:"payload"`
}

// ActionProvider defines the generic mechanism to validate and execute actions.
type ActionProvider interface {
	Validate(action Action) error
	Execute(ctx context.Context, action Action) (interface{}, error)
}

// NamedActionProvider optionally exposes the stable adapter identity used in
// lifecycle telemetry. Capability and provider are deliberately separate:
// one capability may be backed by different adapters in different teams.
type NamedActionProvider interface {
	ProviderName() string
}

// ProviderRegistry holds the registered action providers.
type ProviderRegistry struct {
	mu        sync.RWMutex
	providers map[string]ActionProvider
}

// NewProviderRegistry creates a new registry.
func NewProviderRegistry() *ProviderRegistry {
	return &ProviderRegistry{
		providers: make(map[string]ActionProvider),
	}
}

// Register registers an action provider for a capability.
func (r *ProviderRegistry) Register(capability string, provider ActionProvider) {
	capability = normalizeCapability(capability)
	if capability == "" || provider == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.providers[capability] = provider
}

// Get returns the provider for a capability.
func (r *ProviderRegistry) Get(capability string) (ActionProvider, bool) {
	if r == nil {
		return nil, false
	}
	capability = normalizeCapability(capability)
	r.mu.RLock()
	defer r.mu.RUnlock()
	p, ok := r.providers[capability]
	return p, ok
}

// Has checks if a capability is registered.
func (r *ProviderRegistry) Has(capability string) bool {
	if r == nil {
		return false
	}
	capability = normalizeCapability(capability)
	r.mu.RLock()
	defer r.mu.RUnlock()
	_, ok := r.providers[capability]
	return ok
}

// ProviderName returns a stable identity for the provider bound to a
// capability. Unnamed providers retain a useful type identity for telemetry.
func (r *ProviderRegistry) ProviderName(capability string) string {
	provider, ok := r.Get(capability)
	if !ok || provider == nil {
		return ""
	}
	if named, ok := provider.(NamedActionProvider); ok && strings.TrimSpace(named.ProviderName()) != "" {
		return strings.TrimSpace(named.ProviderName())
	}
	return fmt.Sprintf("%T", provider)
}

// Clone returns an isolated registry snapshot. Team-specific adapter
// registration therefore cannot mutate the process-wide default registry or
// leak one team's provider configuration into another team.
func (r *ProviderRegistry) Clone() (*ProviderRegistry, error) {
	clone := NewProviderRegistry()
	if r == nil {
		return clone, nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	for capability, provider := range r.providers {
		if provider == nil {
			return nil, fmt.Errorf("provider registry contains nil provider for %q", capability)
		}
		clone.providers[capability] = provider
	}
	return clone, nil
}

func normalizeCapability(capability string) string {
	return strings.ToLower(strings.TrimSpace(capability))
}

// DefaultProviderRegistry is the global registry.
var DefaultProviderRegistry = NewProviderRegistry()

// commandActionProvider is a domain-neutral process adapter. A configured
// command receives the Action JSON on stdin and must emit one JSON value on
// stdout. Individual teams can bind any external system behind this boundary
// without teaching Hufu about its executable, action format, or semantics.
type commandActionProvider struct {
	capability string
	command    []string
	dir        string
	timeout    time.Duration
}

func (p *commandActionProvider) ProviderName() string {
	if p == nil || len(p.command) == 0 {
		return "command"
	}
	return "command:" + p.command[0]
}

func registerConfiguredActionProviders(registry *ProviderRegistry, configs map[string]agent.ActionProviderConfig) error {
	if len(configs) == 0 {
		return nil
	}
	if registry == nil {
		return fmt.Errorf("action provider registry is nil")
	}
	for rawCapability, config := range configs {
		capability := normalizeCapability(rawCapability)
		if capability == "" {
			return fmt.Errorf("action-providers contains an empty capability")
		}
		if len(config.Command) == 0 || strings.TrimSpace(config.Command[0]) == "" {
			return fmt.Errorf("action provider %q requires a command", capability)
		}
		if config.Timeout < 0 {
			return fmt.Errorf("action provider %q timeout cannot be negative", capability)
		}
		registry.Register(capability, &commandActionProvider{
			capability: capability,
			command:    append([]string(nil), config.Command...),
			dir:        strings.TrimSpace(config.Dir),
			timeout:    time.Duration(config.Timeout) * time.Second,
		})
	}
	return nil
}

func (p *commandActionProvider) Validate(action Action) error {
	if p == nil || len(p.command) == 0 {
		return fmt.Errorf("provider is not configured")
	}
	if normalizeCapability(action.Capability) != p.capability {
		return fmt.Errorf("action capability %q does not match provider capability %q", action.Capability, p.capability)
	}
	if strings.TrimSpace(action.Type) == "" {
		return fmt.Errorf("action type is required")
	}
	return nil
}

func (p *commandActionProvider) Execute(ctx context.Context, action Action) (interface{}, error) {
	if err := p.Validate(action); err != nil {
		return nil, err
	}
	if p.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, p.timeout)
		defer cancel()
	}
	payload, err := json.Marshal(action)
	if err != nil {
		return nil, fmt.Errorf("encode action: %w", err)
	}
	cmd := exec.CommandContext(ctx, p.command[0], p.command[1:]...)
	cmd.Dir = p.dir
	cmd.Stdin = bytes.NewReader(payload)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		detail := strings.TrimSpace(stderr.String())
		if detail == "" {
			detail = err.Error()
		}
		return nil, fmt.Errorf("adapter command failed: %s", detail)
	}
	var result interface{}
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		return nil, fmt.Errorf("adapter command returned invalid JSON: %w", err)
	}
	return result, nil
}

// ActionProviderError represents a provider execution error.
type ActionProviderError struct {
	Capability string
	Cause      error
}

func (e ActionProviderError) Error() string {
	return fmt.Sprintf("provider for %q failed: %v", e.Capability, e.Cause)
}

func (e ActionProviderError) Unwrap() error { return e.Cause }

// ActionValidationError marks a permanent invalid-action response from a
// provider. It is distinct from an execution failure so the retry engine does
// not repeatedly send an invalid action to an adapter.
type ActionValidationError struct {
	Capability string
	Cause      error
}

func (e ActionValidationError) Error() string {
	return fmt.Sprintf("provider for %q rejected action: %v", e.Capability, e.Cause)
}

func (e ActionValidationError) Unwrap() error { return e.Cause }
