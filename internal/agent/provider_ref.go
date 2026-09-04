package agent

import (
	"fmt"
	"slices"
	"strings"

	"github.com/kjelly/hufu/internal/providerintrospection"
)

// ProviderExecutionPolicy is the immutable runtime policy for the effective
// provider selected for a model ID. A zero MaxConcurrent means that the
// provider has no additional concurrency limit beyond the team-wide limit.
type ProviderExecutionPolicy struct {
	ProviderKey   string
	MaxConcurrent int
}

// ResolveProviderExecutionPolicy resolves the same effective provider target
// used for model invocation and returns its provider-scoped execution policy.
// The returned value is a copy; ProviderManager state remains private.
func (pm *ProviderManager) ResolveProviderExecutionPolicy(modelID string) (ProviderExecutionPolicy, error) {
	if pm == nil {
		return ProviderExecutionPolicy{}, fmt.Errorf("provider manager unavailable")
	}
	providerName := pm.effectiveProviderKey(modelID)
	provider := pm.GetProvider(modelID)
	if provider == nil {
		return ProviderExecutionPolicy{}, fmt.Errorf("no provider for model %q", modelID)
	}
	if providerName != "local" && provider.Name() == "local" {
		providerName = "local"
	}

	pm.mu.RLock()
	maxConcurrent := 0
	if cfg, ok := pm.configs[providerName]; ok {
		maxConcurrent = cfg.MaxConcurrent
	}
	pm.mu.RUnlock()
	return ProviderExecutionPolicy{
		ProviderKey:   providerName,
		MaxConcurrent: maxConcurrent,
	}, nil
}

// effectiveProviderKey returns the canonical configured provider key selected
// by model invocation. Unknown provider prefixes use the default local target.
func (pm *ProviderManager) effectiveProviderKey(modelID string) string {
	prefix, _ := ParseModelProvider(modelID)
	providerName := canonicalProviderName(prefix)
	if providerName == "local" {
		return providerName
	}
	pm.mu.RLock()
	_, configured := pm.configs[providerName]
	pm.mu.RUnlock()
	if !configured {
		return "local"
	}
	return providerName
}

// ResolveProviderRef returns the immutable provider identity used by both
// model invocation and profile introspection. The returned reference has no
// credential accessor; its key is consumed only by the introspection client.
func (pm *ProviderManager) ResolveProviderRef(modelID string) (providerintrospection.ProviderRef, error) {
	if pm == nil {
		return providerintrospection.ProviderRef{}, fmt.Errorf("provider manager unavailable")
	}
	providerName := pm.effectiveProviderKey(modelID)
	provider := pm.GetProvider(modelID)
	if provider == nil {
		return providerintrospection.ProviderRef{}, fmt.Errorf("no provider for model %q", modelID)
	}
	if providerName != "local" && provider.Name() == "local" {
		providerName = "local"
	}
	pm.mu.RLock()
	target := pm.effectiveProviderTargetLocked(providerName)
	providerType := "openai-compatible"
	if providerName == "local" {
		providerType = "ollama"
	}
	if cfg, ok := pm.configs[providerName]; ok && strings.TrimSpace(cfg.IntrospectionType) != "" {
		providerType = strings.ToLower(strings.TrimSpace(cfg.IntrospectionType))
	}
	pm.mu.RUnlock()
	// Provider identity is derived from the configured upstream only. The
	// effective endpoint may be a short-lived loopback invocation proxy and is
	// private to the transport boundary, not a provider identity.
	if target.upstreamURL == "" {
		return providerintrospection.ProviderRef{}, fmt.Errorf("provider %q has no upstream URL", providerName)
	}
	return providerintrospection.NewProviderRef(providerName, providerName, providerType, target.upstreamURL, target.apiKey, false), nil
}

// EffectiveProviderRefs returns the provider identities that can be selected
// by invocation resolution. The references carry credentials only inside the
// provider-introspection package; this package exposes no credential accessor.
// The result is deterministic and contains the canonical local provider plus
// each configured named provider.
func (pm *ProviderManager) EffectiveProviderRefs() []providerintrospection.ProviderRef {
	if pm == nil {
		return nil
	}
	pm.mu.RLock()
	providers := make([]string, 0, len(pm.configs)+1)
	providers = append(providers, "local")
	for name := range pm.configs {
		if canonical := canonicalProviderName(name); canonical != "local" && !slices.Contains(providers, canonical) {
			providers = append(providers, canonical)
		}
	}
	slices.Sort(providers[1:])
	refs := make([]providerintrospection.ProviderRef, 0, len(providers))
	for _, name := range providers {
		target := pm.effectiveProviderTargetLocked(name)
		if strings.TrimSpace(target.upstreamURL) == "" {
			continue
		}
		providerType := "openai-compatible"
		if name == "local" {
			providerType = "ollama"
		}
		if cfg, ok := pm.configs[name]; ok && strings.TrimSpace(cfg.IntrospectionType) != "" {
			providerType = strings.ToLower(strings.TrimSpace(cfg.IntrospectionType))
		}
		refs = append(refs, providerintrospection.NewProviderRef(name, name, providerType, target.upstreamURL, target.apiKey, false))
	}
	pm.mu.RUnlock()
	return refs
}
