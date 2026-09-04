package agent

import (
	"context"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/kjelly/hufu/internal/config"
	"github.com/kjelly/hufu/internal/modelprofile"
	"github.com/kjelly/hufu/internal/providerintrospection"
)

func TestProviderManagerResolvesMatchingAndDefaultProviderRefs(t *testing.T) {
	manager, err := NewProviderManager("http://127.0.0.1:11434/v1", "default-secret", map[string]config.ProviderConfig{
		"openai": {ProviderURL: "https://api.example/v1", ProviderAPIKey: "openai-secret"},
	})
	if err != nil {
		t.Fatal(err)
	}
	openAI, err := manager.ResolveProviderRef("openai/gpt-4o")
	if err != nil {
		t.Fatal(err)
	}
	if openAI.Provider != "openai" || openAI.BaseURL != "https://api.example/v1" || openAI.Type != "openai-compatible" {
		t.Fatalf("named provider ref = %#v", openAI)
	}
	for _, modelID := range []string{"qwen3:8b", "ollama/qwen3:8b", "unknown/model"} {
		ref, resolveErr := manager.ResolveProviderRef(modelID)
		if resolveErr != nil {
			t.Fatalf("ResolveProviderRef(%q): %v", modelID, resolveErr)
		}
		if ref.Provider != "local" || ref.BaseURL != "http://127.0.0.1:11434/v1" || ref.Type != "ollama" {
			t.Fatalf("default provider ref for %q = %#v", modelID, ref)
		}
	}
}

func TestProviderManagerResolvesCanonicalExecutionPolicy(t *testing.T) {
	manager, err := NewProviderManager("http://127.0.0.1:11434/v1", "", map[string]config.ProviderConfig{
		"local":  {MaxConcurrent: 1},
		"ollama": {MaxConcurrent: 2},
	})
	if err != nil {
		t.Fatal(err)
	}

	for _, modelID := range []string{"local/model", "ollama/model", "model"} {
		policy, resolveErr := manager.ResolveProviderExecutionPolicy(modelID)
		if resolveErr != nil {
			t.Fatalf("ResolveProviderExecutionPolicy(%q): %v", modelID, resolveErr)
		}
		if policy.ProviderKey != "local" || policy.MaxConcurrent != 1 {
			t.Fatalf("execution policy for %q = %#v, want local/capacity 1", modelID, policy)
		}
	}
}

func TestProviderManagerPreservesLocalhostProviderRefAndNormalizesIdentity(t *testing.T) {
	manager, err := NewProviderManager("http://localhost:11434/v1", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	provider := manager.GetProvider("qwen3:8b")
	if provider.baseURL != "http://localhost:11434/v1" {
		t.Fatalf("provider URL = %q, want configured localhost URL", provider.baseURL)
	}
	ref, err := manager.ResolveProviderRef("qwen3:8b")
	if err != nil {
		t.Fatal(err)
	}
	if ref.BaseURL != "http://localhost:11434/v1" {
		t.Fatalf("provider ref URL = %q, want configured localhost URL", ref.BaseURL)
	}
	identity, err := modelprofile.Identity(ref, "qwen3:8b")
	if err != nil {
		t.Fatal(err)
	}
	if identity.BaseURL != "http://127.0.0.1:11434" {
		t.Fatalf("cache identity URL = %q, want literal loopback", identity.BaseURL)
	}
}

func TestProviderManagerSelectsConfiguredIntrospectionAdapters(t *testing.T) {
	manager, err := NewProviderManager("http://127.0.0.1:11434/v1", "", map[string]config.ProviderConfig{
		"ollama-gateway": {
			ProviderURL:       "http://gateway.example:1234/v1",
			IntrospectionType: "ollama",
		},
		"generic-gateway": {ProviderURL: "https://api.example/v1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	ollamaRef, err := manager.ResolveProviderRef("ollama-gateway/qwen3:8b")
	if err != nil {
		t.Fatal(err)
	}
	if ollamaRef.Type != "ollama" || ollamaRef.BaseURL != "http://gateway.example:1234/v1" {
		t.Fatalf("configured Ollama ref = %#v", ollamaRef)
	}
	genericRef, err := manager.ResolveProviderRef("generic-gateway/model")
	if err != nil {
		t.Fatal(err)
	}
	if genericRef.Type != "openai-compatible" || genericRef.BaseURL != "https://api.example/v1" {
		t.Fatalf("configured generic ref = %#v", genericRef)
	}

	if _, err := NewProviderManager("http://127.0.0.1:11434/v1", "", map[string]config.ProviderConfig{
		"invalid": {IntrospectionType: "unknown-adapter"},
	}); err == nil {
		t.Fatal("invalid introspection adapter was accepted")
	}
}

func TestProviderRefUsesConfiguredUpstreamWhenInvocationProxyIsActive(t *testing.T) {
	manager, err := NewProviderManager("https://remote.example/v1", "secret", nil)
	if err != nil {
		t.Fatal(err)
	}
	provider := manager.GetProvider("qwen3:8b")
	provider.setBoundary("http://127.0.0.1:43123/v1", &http.Client{})

	ref, err := manager.ResolveProviderRef("ollama/qwen3:8b")
	if err != nil {
		t.Fatal(err)
	}
	if ref.BaseURL != "https://remote.example/v1" {
		t.Fatalf("provider ref base URL = %q, want configured upstream", ref.BaseURL)
	}
	identity, err := modelprofile.Identity(ref, "ollama/qwen3:8b")
	if err != nil {
		t.Fatal(err)
	}
	if identity.BaseURL != "https://remote.example" {
		t.Fatalf("cache identity base URL = %q, want configured upstream", identity.BaseURL)
	}

	var called atomic.Bool
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		called.Store(true)
		return nil, context.Canceled
	})}
	ref.NoNet = true
	introspector := &providerintrospection.OpenAICompatibleIntrospector{BaseURL: ref.BaseURL, Client: client}
	if _, err := introspector.InspectModel(t.Context(), ref, "qwen3:8b"); err == nil {
		t.Fatal("no-net introspection unexpectedly allowed configured remote upstream")
	}
	if called.Load() {
		t.Fatal("no-net introspection reached HTTP client through invocation proxy")
	}
}

func TestEffectiveProviderRefsAreDeterministicAndSecretFree(t *testing.T) {
	manager, err := NewProviderManager("http://127.0.0.1:11434/v1", "local-secret", map[string]config.ProviderConfig{
		"zeta":  {ProviderURL: "https://zeta.example/v1", ProviderAPIKey: "zeta-secret"},
		"alpha": {ProviderURL: "https://alpha.example/v1", ProviderAPIKey: "alpha-secret"},
	})
	if err != nil {
		t.Fatal(err)
	}
	refs := manager.EffectiveProviderRefs()
	if len(refs) != 3 || refs[0].Provider != "local" || refs[1].Provider != "alpha" || refs[2].Provider != "zeta" {
		t.Fatalf("effective refs = %#v", refs)
	}
	for _, ref := range refs {
		if strings.Contains(ref.BaseURL, "secret") {
			t.Fatalf("provider reference leaked credential: %#v", ref)
		}
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}
