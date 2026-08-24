package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestDefaultOpenAICompatibleProviderStripsAnyModelNamespace(t *testing.T) {
	provider, err := NewOpenAICompatibleProvider("http://127.0.0.1:1/v1", "", "local")
	if err != nil {
		t.Fatalf("NewOpenAICompatibleProvider() error = %v", err)
	}
	provider.defaultProvider = true

	for _, modelID := range []string{"Qwen3.8-27B-ThinkingCoder", "lemonade/Qwen3.8-27B-ThinkingCoder", "llamacpp/model.gguf", "ollama/qwen3:8b"} {
		if got, want := provider.modelName(modelID), modelID[strIndexAfterSlash(modelID):]; got != want {
			t.Errorf("modelName(%q) = %q, want %q", modelID, got, want)
		}
	}
}

func TestDetectProviderContextLengthUsesModelsMetadata(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/models" {
			t.Errorf("request = %s %s, want GET /v1/models", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Errorf("Authorization = %q, want test key", got)
		}
		_, _ = w.Write([]byte(`{"data":[{"id":"local-model","max_context_window":262144}]}`))
	}))
	defer server.Close()

	got, err := DetectProviderContextLength(context.Background(), server.URL+"/v1", "test-key", "local-model")
	if err != nil {
		t.Fatalf("DetectProviderContextLength() error = %v", err)
	}
	if got != 262144 {
		t.Fatalf("context length = %d, want 262144", got)
	}
}

func TestDetectProviderContextCapacityAcceptsMaxInputTokens(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			t.Fatalf("request path = %q, want /v1/models", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"data":[{"id":"local-model","max_input_tokens":39936}]}`))
	}))
	defer server.Close()

	capacity, err := DetectProviderContextCapacity(context.Background(), server.URL+"/v1", "", "local-model")
	if err != nil {
		t.Fatalf("DetectProviderContextCapacity() error = %v", err)
	}
	if capacity.ContextWindow != 39936 || capacity.Source != ContextCapacitySourceMetadata {
		t.Fatalf("capacity = %#v, want window 39936 from provider metadata", capacity)
	}
}

func strIndexAfterSlash(modelID string) int {
	for i := len(modelID) - 1; i >= 0; i-- {
		if modelID[i] == '/' {
			return i + 1
		}
	}
	return 0
}

func TestOpenAICompatibleProviderTypeRemainsJSONConstructible(t *testing.T) {
	provider, err := NewOpenAICompatibleProvider("http://127.0.0.1:1/v1", "", "lemonade")
	if err != nil {
		t.Fatalf("NewOpenAICompatibleProvider() error = %v", err)
	}
	if _, err := json.Marshal(provider.Name()); err != nil {
		t.Fatalf("provider name is not JSON constructible: %v", err)
	}
}
