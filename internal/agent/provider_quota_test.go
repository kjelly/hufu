package agent

import (
	"context"
	"strings"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/kjelly/hufu/internal/config"
)

func TestProviderQuotaTargetBindsOnlyOllamaCloud(t *testing.T) {
	t.Setenv("OLLAMA_API_KEY", "cloud-key")
	for _, tt := range []struct {
		name, url, key, model, wantKey string
		eligible                       bool
	}{
		{"cloud", "https://ollama.com/v1", "provider-key", "ollama/glm-5.3", "provider-key", true},
		{"local relay", "http://localhost:11434/v1", "local-only-key", "ollama/glm-5.3:cloud", "cloud-key", true},
		{"ipv6 relay", "http://[::1]:11434/v1", "", "minimax-m3:cloud", "cloud-key", true},
		{"local model", "http://localhost:11434/v1", "", "qwen3:8b", "", false},
		{"other provider", "https://example.com/v1", "other-key", "glm-5.3:cloud", "", false},
		{"spoofed host", "https://ollama.com.example.com/v1", "other-key", "glm-5.3:cloud", "", false},
		{"insecure cloud", "http://ollama.com/v1", "secret", "glm-5.3:cloud", "", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			pm, err := NewProviderManager(tt.url, tt.key, nil)
			if err != nil {
				t.Fatal(err)
			}
			id, key, ok := pm.quotaTarget(tt.model)
			if ok != tt.eligible || key != tt.wantKey {
				t.Fatalf("eligible=%v key matches=%v", ok, key == tt.wantKey)
			}
			if ok && (len(id) != 64 || strings.Contains(id, key)) {
				t.Fatal("unsafe quota identity")
			}
		})
	}
	pm, err := NewProviderManager("http://localhost:11434/v1", "", map[string]config.ProviderConfig{
		"cloud-a": {ProviderURL: "https://ollama.com/v1", ProviderAPIKey: "same-account"},
		"cloud-b": {ProviderURL: "https://ollama.com/v1", ProviderAPIKey: "same-account"},
	})
	if err != nil {
		t.Fatal(err)
	}
	a, _, _ := pm.quotaTarget("cloud-a/model")
	b, _, _ := pm.quotaTarget("cloud-b/other")
	if a == "" || a != b {
		t.Fatal("account aliases do not share quota")
	}
}

type quotaTestStore struct{ until time.Time }

func (s *quotaTestStore) Load(context.Context, string) (time.Time, error) { return s.until, nil }
func (s *quotaTestStore) Save(_ context.Context, _ string, until time.Time) error {
	s.until = until
	return nil
}

func TestProviderQuotaRecoveryIgnoresOtherFailures(t *testing.T) {
	t.Setenv("OLLAMA_API_KEY", "")
	pm, err := NewProviderManager("http://localhost:11434/v1", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		name, model string
		failure     *fantasy.ProviderError
		want        bool
	}{
		{"cloud quota", "ollama/glm:cloud", &fantasy.ProviderError{StatusCode: 429}, true},
		{"local rate limit", "qwen:8b", &fantasy.ProviderError{StatusCode: 429}, false},
		{"server failure", "ollama/glm:cloud", &fantasy.ProviderError{StatusCode: 503}, false},
		{"auth failure", "ollama/glm:cloud", &fantasy.ProviderError{StatusCode: 429, AuthError: true}, false},
		{"error text", "ollama/glm:cloud", &fantasy.ProviderError{Message: "too many requests 429"}, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			store := &quotaTestStore{}
			retry, err := pm.RecoverProviderQuota(t.Context(), tt.model, tt.failure, store)
			if err != nil || retry != tt.want {
				t.Fatalf("retry=%v err=%v", retry, err)
			}
			if tt.want && time.Until(store.until) < 9*time.Minute {
				t.Fatal("fallback was shorter than ten minutes")
			}
		})
	}
}
