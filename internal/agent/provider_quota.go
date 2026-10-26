package agent

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"strings"

	"charm.land/fantasy"
	"github.com/kjelly/hufu/internal/ollamaquota"
)

// ProviderRequestRecovery handles a rejected generation, never an agent/tool
// replay. Every resend still passes admission, slot acquisition and accounting.
type ProviderRequestRecovery interface {
	CanRecoverProviderRequest(ProviderRequest) bool
	WaitProviderRequest(context.Context, ProviderRequest) (func(bool) error, error)
	RecoverProviderRequest(context.Context, ProviderRequest, error) (bool, error)
}

func (pm *ProviderManager) SupportsProviderQuota(modelID string) bool {
	_, _, ok := pm.quotaTarget(modelID)
	return ok
}

func (pm *ProviderManager) quotaTarget(modelID string) (string, string, bool) {
	if pm == nil {
		return "", "", false
	}
	provider := pm.effectiveProviderKey(modelID)
	resolved := pm.GetProvider(modelID)
	if resolved == nil {
		return "", "", false
	}
	if provider != "local" && resolved.Name() == "local" {
		provider = "local"
	}
	pm.mu.RLock()
	target := pm.effectiveProviderTargetLocked(provider)
	pm.mu.RUnlock()
	endpoint, err := url.Parse(target.upstreamURL)
	if err != nil || endpoint.User != nil {
		return "", "", false
	}
	host := endpoint.Hostname()
	cloud := endpoint.Scheme == "https" && strings.EqualFold(host, "ollama.com") && (endpoint.Port() == "" || endpoint.Port() == "443")
	addr, _ := netip.ParseAddr(host)
	loopback := host == "localhost" || addr.IsLoopback()
	// The local daemon forwards :cloud models using its own sign-in. Never
	// forward arbitrary compatible-provider credentials to Ollama's account API.
	relay := loopback && strings.HasSuffix(modelID, ":cloud")
	if !cloud && !relay {
		return "", "", false
	}
	key := target.apiKey
	if !cloud {
		key = strings.TrimSpace(os.Getenv("OLLAMA_API_KEY"))
	}
	identity := key
	if key == "" {
		identity = target.upstreamURL
	}
	digest := sha256.Sum256([]byte("ollama-quota\x00" + identity))
	return fmt.Sprintf("%x", digest), key, true
}

func (pm *ProviderManager) quotaEngine() *ollamaquota.Engine {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	if pm.quota == nil {
		pm.quota = ollamaquota.New(nil)
	}
	return pm.quota
}

// ProviderQuotaWaiting reports an invocation-owned wait to the stall watchdog.
func (pm *ProviderManager) ProviderQuotaWaiting() bool {
	if pm == nil {
		return false
	}
	pm.mu.RLock()
	engine := pm.quota
	pm.mu.RUnlock()
	return engine != nil && engine.IsWaiting()
}

// WaitProviderQuota restores an account cooldown before opening a provider
// invocation. Only the credential-free identity is exposed to the store.
func (pm *ProviderManager) WaitProviderQuota(ctx context.Context, modelID string, store ollamaquota.Store) (func(bool) error, error) {
	id, key, ok := pm.quotaTarget(modelID)
	if !ok {
		return nil, nil
	}
	return pm.quotaEngine().Wait(ctx, id, key, store)
}

func (pm *ProviderManager) RecoverProviderQuota(ctx context.Context, modelID string, providerErr error, store ollamaquota.Store) (bool, error) {
	failed, ok := errors.AsType[*fantasy.ProviderError](providerErr)
	if !ok || failed.StatusCode != http.StatusTooManyRequests || failed.AuthError {
		return false, nil
	}
	id, key, ok := pm.quotaTarget(modelID)
	if !ok {
		return false, nil
	}
	var retry string
	for name, value := range failed.ResponseHeaders {
		if strings.EqualFold(name, "Retry-After") {
			retry = value
		}
	}
	if err := pm.quotaEngine().Rejected(ctx, id, key, retry, store); err != nil {
		return false, err
	}
	return true, nil
}
