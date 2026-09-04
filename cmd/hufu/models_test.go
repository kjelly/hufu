package main

import (
	"bytes"
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
)

func TestModelsInspectIsOfflineAndReportsEmbeddedOrCacheOrigin(t *testing.T) {
	cachePath := filepath.Join(t.TempDir(), "models.json")
	if err := os.WriteFile(cachePath, []byte(`{"version":"test","models":[{"provider":"openai","id":"model","context":123}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var requests atomic.Int32
	server := newModelsTLSServer(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		requests.Add(1)
	}))
	defer server.Close()
	previousFactory := newModelCatalogStore
	previousPath := modelsCachePath
	previousJSON := modelsJSON
	previousInspectNoNet := modelsInspectNoNet
	newModelCatalogStore = func() *modelcatalog.Store {
		return modelcatalog.NewStore(modelsCachePath, modelcatalog.StoreOptions{SourceURL: server.URL, Client: server.Client()})
	}
	modelsCachePath, modelsJSON, modelsInspectNoNet = cachePath, true, true
	t.Cleanup(func() {
		newModelCatalogStore = previousFactory
		modelsCachePath, modelsJSON, modelsInspectNoNet = previousPath, previousJSON, previousInspectNoNet
	})

	root := newRootCommand()
	var output bytes.Buffer
	root.SetOut(&output)
	root.SetArgs([]string{"models", "inspect", "openai/model"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	if requests.Load() != 0 {
		t.Fatal("models inspect performed an HTTP request")
	}
	if !strings.Contains(output.String(), `"origin":"cache"`) || !strings.Contains(output.String(), `"found":true`) || !strings.Contains(output.String(), `"effective_context"`) {
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
