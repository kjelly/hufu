package providerintrospection

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestParseOllamaParameters(t *testing.T) {
	got := ParseOllamaParameters(`
# defaults
PARAMETER num_ctx 32768 # context
temperature 0.2
top_p 0.9
top_k 40
num_predict 4096
custom_key custom value
equals_key=value
`)
	for key, want := range map[string]string{
		"num_ctx":     "32768",
		"temperature": "0.2",
		"top_p":       "0.9",
		"top_k":       "40",
		"num_predict": "4096",
		"custom_key":  "custom value",
		"equals_key":  "value",
	} {
		if got[key] != want {
			t.Errorf("parameter %q = %q, want %q", key, got[key], want)
		}
	}
}

func TestFindOllamaContextLength(t *testing.T) {
	if got, ok := FindOllamaContextLength(map[string]any{"qwen.context_length": 131072}); !ok || got != 131072 {
		t.Fatalf("unique context = %d, %t", got, ok)
	}
	if got, ok := FindOllamaContextLength(map[string]any{"a.context_length": 131072, "b.context_length": 131072}); !ok || got != 131072 {
		t.Fatalf("same contexts = %d, %t", got, ok)
	}
	if got, ok := FindOllamaContextLength(map[string]any{"a.context_length": 131072, "b.context_length": 65536}); ok || got != 0 {
		t.Fatalf("conflicting contexts = %d, %t, want zero false", got, ok)
	}
}

func TestParseOllamaShow(t *testing.T) {
	info, err := ParseOllamaShow(map[string]any{
		"model":        "qwen3:8b",
		"parameters":   "num_ctx 65536\nnum_predict 4096\ntemperature 0.2\ncustom x",
		"model_info":   map[string]any{"qwen.context_length": 131072, "custom.info": "kept"},
		"capabilities": []any{"completion", "tools", "thinking", "future-capability"},
		"details":      map[string]any{"family": "qwen", "parameter_size": "8B", "quantization_level": "Q4_K_M"},
		"custom_raw":   "retained",
	})
	if err != nil {
		t.Fatalf("ParseOllamaShow() error = %v", err)
	}
	if info.ConfiguredContext != 65536 || info.ModelMaxContext != 131072 || info.MaxOutputTokens != 4096 {
		t.Fatalf("limits = configured context %d, max context %d, max output %d", info.ConfiguredContext, info.ModelMaxContext, info.MaxOutputTokens)
	}
	if info.Family != "qwen" || info.ParameterSize != "8B" || info.Quantization != "Q4_K_M" {
		t.Fatalf("details = %#v", info)
	}
	if len(info.Capabilities) != 4 || info.Raw["custom_raw"] != "retained" {
		t.Fatalf("capabilities/raw = %#v / %#v", info.Capabilities, info.Raw)
	}
}

func TestParseOllamaPS(t *testing.T) {
	raw := map[string]any{"models": []any{
		map[string]any{"name": "other", "context_length": 1},
		map[string]any{"model": "qwen3:8b", "context_length": 32768, "size": 100, "size_vram": 80, "expires_at": "tomorrow"},
	}}
	info, found, err := ParseOllamaPS(raw, "ollama/qwen3:8b")
	if err != nil || !found || info.RuntimeContext != 32768 || info.Size != 100 || info.SizeVRAM != 80 || info.ExpiresAt != "tomorrow" {
		t.Fatalf("loaded = %#v, found=%t, err=%v", info, found, err)
	}
	if _, found, err := ParseOllamaPS(raw, "missing"); err != nil || found {
		t.Fatalf("missing model = found=%t, err=%v", found, err)
	}
}

func TestModelIDMatchingPreservesNamespacesAndProviderBoundaries(t *testing.T) {
	tests := []struct {
		name      string
		candidate string
		requested string
		want      bool
	}{
		{name: "qualified response versus namespaced request", candidate: "ollama/namespace/model:tag", requested: "namespace/model:tag", want: true},
		{name: "namespaced response versus qualified request", candidate: "namespace/model:tag", requested: "ollama/namespace/model:tag", want: true},
		{name: "different qualified providers", candidate: "ollama/namespace/model:tag", requested: "openai/namespace/model:tag", want: false},
		{name: "exact namespaced generic ID", candidate: "namespace/model:tag", requested: "namespace/model:tag", want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := matchesRequestedModel(tt.candidate, tt.requested); got != tt.want {
				t.Fatalf("matchesRequestedModel(%q, %q) = %t, want %t", tt.candidate, tt.requested, got, tt.want)
			}
		})
	}

	if got := unqualifiedModelID("ollama/namespace/model:tag"); got != "namespace/model:tag" {
		t.Fatalf("unqualifiedModelID() = %q, want namespace/model:tag", got)
	}
}

func TestParseOllamaPSMatchesNamespacedModelBothDirections(t *testing.T) {
	raw := map[string]any{"models": []any{
		map[string]any{"name": "ollama/namespace/model:tag", "context_length": 32768},
	}}
	for _, requested := range []string{"namespace/model:tag", "ollama/namespace/model:tag"} {
		t.Run(requested, func(t *testing.T) {
			info, found, err := ParseOllamaPS(raw, requested)
			if err != nil || !found || info.RuntimeContext != 32768 {
				t.Fatalf("loaded = %#v, found=%t, err=%v", info, found, err)
			}
		})
	}
}

func TestParseOpenAIModelsCommonFields(t *testing.T) {
	info, err := ParseOpenAIModels(map[string]any{"data": []any{
		map[string]any{"id": "other", "context_length": 1},
		map[string]any{"id": "local-model", "max_input_tokens": 39936, "max_completion_tokens": 4096, "custom": "kept"},
	}}, "provider/local-model")
	if err != nil || info.ModelID != "local-model" || info.ModelMaxContext != 39936 || info.MaxOutputTokens != 4096 || info.Raw["custom"] != "kept" {
		t.Fatalf("info = %#v, err=%v", info, err)
	}
}

func TestOllamaIntrospectorShowPSAndNativeURLNormalization(t *testing.T) {
	server := newIPv4Server(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/show":
			if r.Method != http.MethodPost || r.Header.Get("Authorization") != "Bearer secret-key" {
				t.Errorf("show request = %s %s auth=%q", r.Method, r.URL.Path, r.Header.Get("Authorization"))
			}
			var body map[string]string
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode show request: %v", err)
			} else if body["model"] != "namespace/qwen3:8b" {
				t.Errorf("show request model = %q, want namespace/qwen3:8b", body["model"])
			}
			_, _ = w.Write([]byte(`{"model":"namespace/qwen3:8b","parameters":"num_ctx 65536\nnum_predict 4096","model_info":{"qwen.context_length":131072},"capabilities":["tools","thinking"],"details":{"family":"qwen"},"custom":"kept"}`))
		case "/api/ps":
			_, _ = w.Write([]byte(`{"models":[{"name":"namespace/qwen3:8b","context_length":32768}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	info, err := (&OllamaIntrospector{BaseURL: server.URL + "/v1", APIKey: "secret-key"}).InspectModel(context.Background(), ProviderRef{}, "ollama/namespace/qwen3:8b")
	if err != nil {
		t.Fatalf("InspectModel() error = %v", err)
	}
	if info.RuntimeContext != 32768 || info.ConfiguredContext != 65536 || info.ModelMaxContext != 131072 || info.MaxOutputTokens != 4096 {
		t.Fatalf("contexts = %#v", info)
	}
}

func TestParseOpenAIModelsMatchesQualifiedResponseInReverse(t *testing.T) {
	info, err := ParseOpenAIModels(map[string]any{"data": []any{
		map[string]any{"id": "provider/namespace/model:tag", "context_length": 128},
	}}, "namespace/model:tag")
	if err != nil || info.ModelID != "provider/namespace/model:tag" || info.ModelMaxContext != 128 {
		t.Fatalf("info = %#v, err=%v", info, err)
	}
}

func TestOllamaIntrospectorMissingPSIsNotError(t *testing.T) {
	server := newIPv4Server(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/show" {
			_, _ = w.Write([]byte(`{"model":"m","parameters":"num_ctx 8"}`))
			return
		}
		_, _ = w.Write([]byte(`{"models":[]}`))
	}))
	defer server.Close()
	info, err := (&OllamaIntrospector{BaseURL: server.URL}).InspectModel(t.Context(), ProviderRef{}, "m")
	if err != nil || info.RuntimeContext != 0 {
		t.Fatalf("info = %#v, err=%v", info, err)
	}
}

func TestOllamaShowTransportValidation(t *testing.T) {
	tests := []struct {
		name    string
		handler http.Handler
		want    string
	}{
		{name: "malformed", handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("{")) }), want: "JSON"},
		{name: "non2xx", handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "secret-key body", http.StatusBadGateway) }), want: "status 502"},
		{name: "oversized", handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"model":"` + strings.Repeat("x", maxResponseBody) + `"}`))
		}), want: "size limit"},
		{name: "timeout", handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			<-time.After(100 * time.Millisecond)
			_, _ = w.Write([]byte(`{"model":"m"}`))
		}), want: "request failed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := newIPv4Server(t, tt.handler)
			defer server.Close()
			ctx := t.Context()
			if tt.name == "timeout" {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, 10*time.Millisecond)
				defer cancel()
			}
			_, err := (&OllamaIntrospector{BaseURL: server.URL}).InspectModel(ctx, ProviderRef{}, "m")
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want substring %q", err, tt.want)
			}
			if strings.Contains(err.Error(), "secret-key") || strings.Contains(err.Error(), "body") {
				t.Fatalf("error leaked sensitive response data: %v", err)
			}
		})
	}

	target := newIPv4Server(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{"model":"m"}`)) }))
	defer target.Close()
	source := newIPv4Server(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Redirect(w, &http.Request{Method: http.MethodPost}, target.URL, http.StatusFound)
	}))
	defer source.Close()
	_, err := (&OllamaIntrospector{BaseURL: source.URL}).InspectModel(t.Context(), ProviderRef{}, "m")
	if err == nil || strings.Contains(err.Error(), target.URL) {
		t.Fatalf("redirect error = %v", err)
	}
}

func TestOpenAICompatibleIntrospectorRootAndV1(t *testing.T) {
	server := newIPv4Server(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			t.Errorf("path = %q, want /v1/models", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"data":[{"id":"m","context_length":128,"max_output_tokens":32}]}`))
	}))
	defer server.Close()
	for _, baseURL := range []string{server.URL, server.URL + "/v1/"} {
		info, err := (&OpenAICompatibleIntrospector{BaseURL: baseURL}).InspectModel(t.Context(), ProviderRef{}, "m")
		if err != nil || info.ModelMaxContext != 128 || info.MaxOutputTokens != 32 {
			t.Fatalf("base %q: info=%#v err=%v", baseURL, info, err)
		}
	}
}

func TestIntrospectionRejectsMalformedNon2xxOversizedTimeoutAndRedirect(t *testing.T) {
	tests := []struct {
		name    string
		handler http.Handler
		want    string
	}{
		{name: "malformed", handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("{")) }), want: "JSON"},
		{name: "non2xx", handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "secret-key body", http.StatusUnauthorized)
		}), want: "status 401"},
		{name: "oversized", handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"data":"` + strings.Repeat("x", maxResponseBody) + `"}`))
		}), want: "size limit"},
		{name: "timeout", handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			<-time.After(100 * time.Millisecond)
			_, _ = w.Write([]byte(`{"data":[]}`))
		}), want: "request failed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := newIPv4Server(t, tt.handler)
			defer server.Close()
			ctx := t.Context()
			if tt.name == "timeout" {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, 10*time.Millisecond)
				defer cancel()
			}
			_, err := (&OpenAICompatibleIntrospector{BaseURL: server.URL}).InspectModel(ctx, ProviderRef{}, "m")
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want substring %q", err, tt.want)
			}
			if strings.Contains(err.Error(), "secret-key") || strings.Contains(err.Error(), "body") {
				t.Fatalf("error leaked sensitive response data: %v", err)
			}
		})
	}

	redirectTarget := newIPv4Server(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{"data":[]}`)) }))
	defer redirectTarget.Close()
	redirectSource := newIPv4Server(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Redirect(w, &http.Request{Method: http.MethodGet}, redirectTarget.URL, http.StatusFound)
	}))
	defer redirectSource.Close()
	_, err := (&OpenAICompatibleIntrospector{BaseURL: redirectSource.URL}).InspectModel(t.Context(), ProviderRef{}, "m")
	if err == nil || strings.Contains(err.Error(), redirectTarget.URL) {
		t.Fatalf("redirect error = %v", err)
	}
}

func TestIntrospectionRawRedactsAPIKey(t *testing.T) {
	server := newIPv4Server(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"id":"m","custom":"secret-key"}]}`))
	}))
	defer server.Close()
	info, err := (&OpenAICompatibleIntrospector{BaseURL: server.URL, APIKey: "secret-key"}).InspectModel(t.Context(), ProviderRef{}, "m")
	if err != nil {
		t.Fatalf("InspectModel() error = %v", err)
	}
	if fmt.Sprint(info.Raw) == "" || strings.Contains(fmt.Sprint(info.Raw), "secret-key") {
		t.Fatalf("raw metadata leaked API key: %#v", info.Raw)
	}
}

func newIPv4Server(t *testing.T, handler http.Handler) *httptest.Server {
	t.Helper()
	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	server := httptest.NewUnstartedServer(handler)
	server.Listener = listener
	server.Start()
	return server
}

var _ ModelIntrospector = (*OllamaIntrospector)(nil)
var _ ModelIntrospector = (*OpenAICompatibleIntrospector)(nil)
