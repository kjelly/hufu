package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/kjelly/hufu/internal/modelcatalog"
	"github.com/kjelly/hufu/internal/modelprofile"
)

func TestModelsInspectJSONUsesSharedDiagnosticCatalogIdentity(t *testing.T) {
	cachePath := filepath.Join(t.TempDir(), "models.json")
	if err := os.WriteFile(cachePath, []byte(`{"version":"test","models":[{"provider":"openai","id":"gpt-4o","context":128000},{"provider":"ollama","id":"qwen3:8b","context":131072}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	previousFactory := newModelCatalogStore
	previousPath := modelsCachePath
	previousJSON := modelsJSON
	previousInspectNoNet := modelsInspectNoNet
	previousProviderURL := opts.providerURL
	previousProviderAPIKey := opts.providerAPIKey
	previousNoNet := opts.noNet
	newModelCatalogStore = func() *modelcatalog.Store {
		return modelcatalog.NewStore(modelsCachePath, modelcatalog.StoreOptions{NoNet: true})
	}
	modelsCachePath, modelsJSON, modelsInspectNoNet = cachePath, true, true
	opts.providerURL, opts.providerAPIKey, opts.noNet = "http://127.0.0.1:11434/v1", "", false
	t.Chdir(t.TempDir())
	if err := os.WriteFile("hufu.yaml", []byte("providers:\n  ollama-gateway:\n    provider-url: http://gateway.example/v1\n    introspection-type: ollama\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		newModelCatalogStore = previousFactory
		modelsCachePath, modelsJSON, modelsInspectNoNet = previousPath, previousJSON, previousInspectNoNet
		opts.providerURL, opts.providerAPIKey, opts.noNet = previousProviderURL, previousProviderAPIKey, previousNoNet
	})

	tests := []struct {
		name         string
		model        string
		wantProvider string
		wantContext  int
	}{
		{name: "unconfigured openai", model: "openai/gpt-4o", wantProvider: "openai", wantContext: 128_000},
		{name: "local ollama", model: "local/qwen3:8b", wantProvider: "ollama", wantContext: 131_072},
		{name: "configured ollama gateway", model: "ollama-gateway/qwen3:8b", wantProvider: "ollama", wantContext: 131_072},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := newRootCommand()
			var output bytes.Buffer
			root.SetOut(&output)
			root.SetArgs([]string{"models", "inspect", test.model, "--no-net"})
			if err := root.Execute(); err != nil {
				t.Fatal(err)
			}
			var payload struct {
				Found   bool                             `json:"found"`
				Model   *modelcatalog.CatalogModel       `json:"model"`
				Profile modelprofile.TelemetryProjection `json:"profile"`
			}
			if err := json.Unmarshal(output.Bytes(), &payload); err != nil {
				t.Fatalf("inspect JSON = %s: %v", output.String(), err)
			}
			if !payload.Found || payload.Model == nil {
				t.Fatalf("catalog lookup = found:%t model:%#v", payload.Found, payload.Model)
			}
			if payload.Model.Provider != test.wantProvider || payload.Profile.Provider != test.wantProvider {
				t.Fatalf("providers = model:%q profile:%q, want %q", payload.Model.Provider, payload.Profile.Provider, test.wantProvider)
			}
			if payload.Profile.Catalog.Value != test.wantContext || payload.Profile.Catalog.Source != modelprofile.SourceCatalog {
				t.Fatalf("profile catalog evidence = %#v, want %d/catalog", payload.Profile.Catalog, test.wantContext)
			}
		})
	}
}

func TestModelsInspectIsOfflineAndReportsEmbeddedOrCacheOrigin(t *testing.T) {
	cachePath := filepath.Join(t.TempDir(), "models.json")
	if err := os.WriteFile(cachePath, []byte(`{"version":"test","models":[{"provider":"openai","id":"gpt-4o","context":123}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var requests atomic.Int32
	server := newModelsHTTPServer(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		requests.Add(1)
	}))
	defer server.Close()
	previousFactory := newModelCatalogStore
	previousPath := modelsCachePath
	previousJSON := modelsJSON
	previousInspectNoNet := modelsInspectNoNet
	previousProviderURL := opts.providerURL
	previousProviderAPIKey := opts.providerAPIKey
	previousNoNet := opts.noNet
	newModelCatalogStore = func() *modelcatalog.Store {
		return modelcatalog.NewStore(modelsCachePath, modelcatalog.StoreOptions{SourceURL: server.URL, Client: server.Client()})
	}
	modelsCachePath, modelsJSON, modelsInspectNoNet = cachePath, true, true
	opts.providerURL, opts.providerAPIKey, opts.noNet = server.URL, "", false
	t.Cleanup(func() {
		newModelCatalogStore = previousFactory
		modelsCachePath, modelsJSON, modelsInspectNoNet = previousPath, previousJSON, previousInspectNoNet
		opts.providerURL, opts.providerAPIKey, opts.noNet = previousProviderURL, previousProviderAPIKey, previousNoNet
	})

	root := newRootCommand()
	var output bytes.Buffer
	root.SetOut(&output)
	root.SetArgs([]string{"models", "inspect", "openai/gpt-4o"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	if requests.Load() != 0 {
		t.Fatal("models inspect performed an HTTP request")
	}
	if !strings.Contains(output.String(), `"origin":"cache"`) || !strings.Contains(output.String(), `"found":true`) || !strings.Contains(output.String(), `"provider":"openai"`) || !strings.Contains(output.String(), `"catalog_context"`) || !strings.Contains(output.String(), `"value":123,"source":"catalog"`) {
		t.Fatalf("inspect output = %s", output.String())
	}
}

func TestModelsUpdateIsTheNetworkPathAndHonorsNoNet(t *testing.T) {
	cachePath := filepath.Join(t.TempDir(), "models.json")
	var requests atomic.Int32
	server := newModelsTLSServer(t, http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		_, _ = writer.Write([]byte(`{"openai":{"models":{"model":{"limit":{"context":456}}}}}`))
	}))
	defer server.Close()
	previousFactory := newModelCatalogStore
	previousPath := modelsCachePath
	previousJSON := modelsJSON
	previousUpdateNoNet := modelsUpdateNoNet
	previousNoNet := opts.noNet
	newModelCatalogStore = func() *modelcatalog.Store {
		return modelcatalog.NewStore(modelsCachePath, modelcatalog.StoreOptions{SourceURL: server.URL, Client: server.Client(), NoNet: opts.noNet || modelsUpdateNoNet})
	}
	modelsCachePath, modelsJSON, modelsUpdateNoNet, opts.noNet = cachePath, true, false, false
	t.Cleanup(func() {
		newModelCatalogStore = previousFactory
		modelsCachePath, modelsJSON, modelsUpdateNoNet, opts.noNet = previousPath, previousJSON, previousUpdateNoNet, previousNoNet
	})

	root := newRootCommand()
	var output bytes.Buffer
	root.SetOut(&output)
	root.SetArgs([]string{"models", "update"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	if requests.Load() != 1 || !strings.Contains(output.String(), `"models":1`) {
		t.Fatalf("update requests=%d output=%s", requests.Load(), output.String())
	}

	requests.Store(0)
	root = newRootCommand()
	root.SetArgs([]string{"models", "update", "--no-net"})
	if err := root.Execute(); err == nil || requests.Load() != 0 {
		t.Fatalf("no-net update err=%v requests=%d", err, requests.Load())
	}
}

func TestModelsInspectUsesConfiguredProviderForQualifiedModel(t *testing.T) {
	cachePath := filepath.Join(t.TempDir(), "models.json")
	if err := os.WriteFile(cachePath, []byte(`{"version":"test","models":[{"provider":"openai","id":"gpt-4o","context":123}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var requests atomic.Int32
	server := newModelsHTTPServer(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests.Add(1)
		if request.URL.Path != "/v1/models" {
			t.Errorf("configured provider request path = %q, want /v1/models", request.URL.Path)
		}
		if got := request.Header.Get("Authorization"); got != "Bearer inspect-secret" {
			t.Errorf("configured provider authorization = %q, want configured key", got)
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(writer, `{"data":[{"id":"gpt-4o","context_length":8192}]}`)
	}))
	defer server.Close()

	configDir := t.TempDir()
	t.Setenv("HOME", configDir)
	t.Chdir(t.TempDir())
	configYAML := fmt.Sprintf("providers:\n  openai:\n    provider-url: %s/v1\n    provider-api-key: inspect-secret\n", server.URL)
	if err := os.WriteFile("hufu.yaml", []byte(configYAML), 0o600); err != nil {
		t.Fatal(err)
	}

	previousFactory := newModelCatalogStore
	previousPath := modelsCachePath
	previousJSON := modelsJSON
	previousInspectNoNet := modelsInspectNoNet
	previousProviderURL := opts.providerURL
	previousProviderAPIKey := opts.providerAPIKey
	previousNoNet := opts.noNet
	newModelCatalogStore = func() *modelcatalog.Store {
		return modelcatalog.NewStore(modelsCachePath, modelcatalog.StoreOptions{NoNet: true})
	}
	modelsCachePath, modelsJSON, modelsInspectNoNet = cachePath, true, false
	opts.providerURL, opts.providerAPIKey, opts.noNet = "", "", false
	t.Cleanup(func() {
		newModelCatalogStore = previousFactory
		modelsCachePath, modelsJSON, modelsInspectNoNet = previousPath, previousJSON, previousInspectNoNet
		opts.providerURL, opts.providerAPIKey, opts.noNet = previousProviderURL, previousProviderAPIKey, previousNoNet
	})

	root := newRootCommand()
	var output bytes.Buffer
	root.SetOut(&output)
	root.SetArgs([]string{"models", "inspect", "openai/gpt-4o"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	if requests.Load() != 1 {
		t.Fatalf("configured provider requests = %d, want one request", requests.Load())
	}
	if !strings.Contains(output.String(), `"provider":"openai"`) || !strings.Contains(output.String(), `"provider_metadata_context"`) {
		t.Fatalf("inspect output = %s", output.String())
	}
}

func newModelsTLSServer(t *testing.T, handler http.Handler) *httptest.Server {
	t.Helper()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &httptest.Server{Listener: listener, Config: &http.Server{Handler: handler}}
	server.StartTLS()
	return server
}

func newModelsHTTPServer(t *testing.T, handler http.Handler) *httptest.Server {
	t.Helper()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Skipf("network listener unavailable in this environment: %v", err)
	}
	server := httptest.NewUnstartedServer(handler)
	server.Listener = listener
	server.Start()
	return server
}
