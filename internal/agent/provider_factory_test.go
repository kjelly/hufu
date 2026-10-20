package agent

import (
	"context"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/kjelly/hufu/internal/providerproxy"
	"github.com/kjelly/hufu/internal/scripted"
)

func TestProviderFactoryFailureNeverFallsBack(t *testing.T) {
	var fallbackCalls atomic.Int64
	pm, err := NewProviderManagerWithBoundaryFactory(scripted.Endpoint, "", nil,
		func(context.Context, string, providerproxy.Config) (providerproxy.Boundary, error) {
			return nil, providerproxy.ErrListenerUnavailable
		})
	if err != nil {
		t.Fatal(err)
	}
	pm.startProcessBoundary = func(context.Context, string, providerproxy.Config) (providerproxy.Boundary, error) {
		fallbackCalls.Add(1)
		return nil, nil
	}
	pm.startInProcessBoundary = func(context.Context, providerproxy.Config) (providerproxy.Boundary, error) {
		fallbackCalls.Add(1)
		return nil, nil
	}
	if err := pm.StartInvocationProxy(t.Context(), "unused"); !errors.Is(err, providerproxy.ErrListenerUnavailable) {
		t.Fatal(err)
	}
	if fallbackCalls.Load() != 0 || len(pm.invocationProxies) != 0 {
		t.Fatal("injected transport failure enabled fallback")
	}
}

func TestProviderFactoryRequiresExplicitTransportAndDrains(t *testing.T) {
	for _, boundary := range []*fakeProviderBoundary{
		nil,
		{endpoint: scripted.Endpoint},
		{endpoint: scripted.Endpoint, client: &http.Client{}},
	} {
		pm, err := NewProviderManagerWithBoundaryFactory(scripted.Endpoint, "", nil,
			func(context.Context, string, providerproxy.Config) (providerproxy.Boundary, error) {
				if boundary == nil {
					return nil, nil
				}
				return boundary, nil
			})
		if err != nil {
			t.Fatal(err)
		}
		if err := pm.StartInvocationProxy(t.Context(), "unused"); err == nil {
			t.Fatal("accepted incomplete factory result")
		}
		if boundary != nil && boundary.abortCalls.Load() != 1 {
			t.Fatal("incomplete boundary was not drained")
		}
	}
}

func TestProviderFactoryUsesOwnedClient(t *testing.T) {
	provider := scripted.NewProvider("test", nil)
	var boundary *scripted.Boundary
	pm, err := NewProviderManagerWithBoundaryFactory(scripted.Endpoint, "", nil,
		func(ctx context.Context, name string, cfg providerproxy.Config) (providerproxy.Boundary, error) {
			if name != "local" || cfg.UpstreamURL != scripted.Endpoint {
				t.Fatalf("unexpected target %s %+v", name, cfg)
			}
			var err error
			boundary, err = scripted.NewBoundary(ctx, provider)
			return boundary, err
		})
	if err != nil {
		t.Fatal(err)
	}
	if err := pm.StartInvocationProxy(t.Context(), "unused"); err != nil {
		t.Fatal(err)
	}
	endpoint, client, active := pm.GetProvider("local/test").effectiveBaseURL()
	if !active || endpoint != scripted.Endpoint || client != boundary.HTTPClient() {
		t.Fatal("factory transport was not installed")
	}
	if err := pm.AbortInvocationProxy(); err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, scripted.Endpoint+"/models", nil)
	if err != nil {
		t.Fatal(err)
	}
	if response, err := client.Do(request); err == nil {
		_ = response.Body.Close()
		t.Fatal("aborted client remained active")
	}
}
