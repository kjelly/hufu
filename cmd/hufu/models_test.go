package main

import (
	"bytes"
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
