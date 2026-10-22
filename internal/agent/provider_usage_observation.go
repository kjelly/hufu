package agent

import (
	"context"
	"sync"
)

type providerUsageKey struct{}

// providerUsageObservation belongs to one admitted provider invocation. A
// transport can preserve usage presence that an SDK's value fields erase.
type providerUsageObservation struct {
	mu       sync.Mutex
	reported bool
	conflict bool
	usage    *ProviderInvocationUsage
}

func withProviderUsageObservation(ctx context.Context) context.Context {
	return context.WithValue(ctx, providerUsageKey{}, &providerUsageObservation{})
}

// ObserveProviderResponseUsage is for trusted host-owned transport adapters.
// Call once with the complete response usage; nil explicitly records absence.
// It grants no admission and is ignored outside an admitted invocation. Model
// output and tool arguments must never be passed to this accounting seam.
func ObserveProviderResponseUsage(ctx context.Context, usage *ProviderInvocationUsage) {
	observation, ok := ctx.Value(providerUsageKey{}).(*providerUsageObservation)
	if !ok {
		return
	}
	observation.mu.Lock()
	defer observation.mu.Unlock()
	if observation.reported {
		observation.conflict = true
		return
	}
	observation.reported = true
	if usage != nil {
		owned := *usage
		observation.usage = &owned
	}
}

func observedProviderUsage(ctx context.Context, result ProviderInvocationResult) ProviderInvocationResult {
	observation, ok := ctx.Value(providerUsageKey{}).(*providerUsageObservation)
	if !ok {
		return result
	}
	observation.mu.Lock()
	defer observation.mu.Unlock()
	if observation.reported {
		result.Usage = nil
		result.UsageReported = false
		if result.ResponseObserved && !observation.conflict && observation.usage != nil {
			owned := *observation.usage
			result.Usage = &owned
			result.UsageReported = true
		}
	}
	return result
}
