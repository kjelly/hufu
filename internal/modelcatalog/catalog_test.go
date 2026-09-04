package modelcatalog

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestParseJSONNormalizesModelsDevShapeAndBoolPresence(t *testing.T) {
	catalog, err := ParseJSON([]byte(`{
		"openai": {"models": {"gpt-4o": {
			"name": "GPT-4o", "family": "gpt", "attachment": false,
			"tool_call": true, "limit": {"context": 128000, "output": 16384}
		}}}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	result, ok := catalog.Lookup("OPENAI", "openai/GPT-4O")
	if !ok {
		t.Fatal("exact normalized catalog lookup did not find model")
	}
	if result.Model.Context != 128000 || result.Model.Output != 16384 || result.Model.Family != "gpt" {
		t.Fatalf("normalized limits/model = %#v", result.Model)
	}
	if result.Model.Attachment == nil || *result.Model.Attachment {
		t.Fatalf("explicit false attachment was not preserved: %#v", result.Model.Attachment)
	}
	if result.Model.Reasoning != nil || result.Model.Temperature != nil {
		t.Fatalf("absent booleans became present: %#v", result.Model)
	}
	if result.Model.Estimator != "gpt" || result.Model.EstimatorProvenance != "catalog_family_derived" {
		t.Fatalf("family estimator provenance = %#v", result.Model)
	}
	if _, ok := catalog.Lookup("openai", "gpt-4"); ok {
		t.Fatal("catalog performed family/prefix matching")
	}
}

func TestParseJSONNormalizesOptionalLimitUnknownValues(t *testing.T) {
	tests := []struct {
		name  string
		limit string
	}{
		{name: "null", limit: `{"context":null,"output":null}`},
		{name: "zero", limit: `{"context":0,"output":0}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			input := fmt.Sprintf(`{"openai":{"models":{"model":{"limit":%s}}}}`, test.limit)
			catalog, err := ParseJSON([]byte(input))
			if err != nil {
				t.Fatal(err)
			}
			result, ok := catalog.Lookup("openai", "model")
			if !ok {
				t.Fatal("normalized model was not found")
			}
			if result.Model.Context != 0 || result.Model.Output != 0 {
				t.Fatalf("unknown limits = context %d, output %d", result.Model.Context, result.Model.Output)
			}
		})
	}
}

func TestParseJSONRejectsInvalidOptionalLimits(t *testing.T) {
	for name, limit := range map[string]string{
		"malformed":   `{"context":true}`,
		"negative":    `{"context":-1}`,
		"nonintegral": `{"context":1.5}`,
	} {
		t.Run(name, func(t *testing.T) {
			input := fmt.Sprintf(`{"openai":{"models":{"model":{"limit":%s}}}}`, limit)
			if _, err := ParseJSON([]byte(input)); err == nil {
				t.Fatal("ParseJSON accepted an invalid optional limit")
			}
		})
	}
}

func TestParseJSONRejectsMalformedAndDuplicateKeys(t *testing.T) {
	for name, input := range map[string]string{
		"duplicate":       `{"openai":{"models":{"model":{"id":"model","id":"other"}}}}`,
		"malformed bool":  `{"openai":{"models":{"model":{"tool_call":"yes"}}}}`,
		"malformed limit": `{"openai":{"models":{"model":{"limit":true}}}}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseJSON([]byte(input)); err == nil {
				t.Fatal("ParseJSON accepted malformed catalog")
			}
		})
	}
}

func TestLoadFallsBackToEmbeddedForMissingOrInvalidCache(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), "missing", "models.json"))
	catalog, origin, err := store.LoadWithOrigin()
	if err != nil || origin != OriginEmbedded {
		t.Fatalf("missing cache load = origin %q, err %v", origin, err)
	}
	if _, ok := catalog.Lookup("openai", "gpt-5"); !ok {
		t.Fatal("embedded fallback did not include a model outside the original sample")
	}
	if err := os.MkdirAll(filepath.Dir(store.Path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(store.Path, []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, origin, err = store.LoadWithOrigin()
	if err != nil || origin != OriginEmbedded {
		t.Fatalf("invalid cache load = origin %q, err %v", origin, err)
	}
}

func TestEmbeddedSnapshotIsCompleteAndProvenanceBound(t *testing.T) {
	var manifest struct {
		SourceURL       string `json:"source_url"`
		RawSHA256       string `json:"raw_sha256"`
		SourceCount     int    `json:"source_count"`
		NormalizedCount int    `json:"normalized_count"`
		ArtifactSHA256  string `json:"artifact_sha256"`
	}
	if err := json.Unmarshal(embeddedManifest, &manifest); err != nil {
		t.Fatalf("decode embedded manifest: %v", err)
	}
	if manifest.SourceURL != DefaultSourceURL {
		t.Fatalf("manifest source URL = %q, want %q", manifest.SourceURL, DefaultSourceURL)
	}
	if manifest.RawSHA256 != "4b86142c61f63f951f561ad856a8c4d3a21b775ceb32301d00cdf86335e85199" {
		t.Fatalf("manifest raw SHA-256 = %q", manifest.RawSHA256)
	}
	if manifest.SourceCount != 7523 || manifest.NormalizedCount != 7523 {
		t.Fatalf("manifest counts = source %d, normalized %d", manifest.SourceCount, manifest.NormalizedCount)
	}
	artifactHash := fmt.Sprintf("%x", sha256.Sum256(embeddedSnapshot))
	if manifest.ArtifactSHA256 != artifactHash {
		t.Fatalf("manifest artifact SHA-256 = %q, computed %q", manifest.ArtifactSHA256, artifactHash)
	}

	catalog, err := parseCatalogFile(embeddedSnapshot)
	if err != nil {
		t.Fatalf("parse embedded snapshot: %v", err)
	}
	if len(catalog.Models) != manifest.NormalizedCount {
		t.Fatalf("embedded model count = %d, want %d", len(catalog.Models), manifest.NormalizedCount)
	}
	for _, test := range []struct {
		provider string
		modelID  string
		name     string
		context  int
		output   int
	}{
		{provider: "openai", modelID: "gpt-5", name: "GPT-5", context: 400000, output: 128000},
		{provider: "google", modelID: "gemini-2.5-pro", name: "Gemini 2.5 Pro", context: 1048576, output: 65536},
	} {
		result, ok := catalog.Lookup(test.provider, test.modelID)
		if !ok {
			t.Fatalf("embedded snapshot missing sentinel %s/%s", test.provider, test.modelID)
		}
		if result.Model.Name != test.name || result.Model.Context != test.context || result.Model.Output != test.output {
			t.Fatalf("sentinel %s/%s = %#v", test.provider, test.modelID, result.Model)
		}
	}
}

func TestGeneratorReproducesCommittedEmbeddedSnapshot(t *testing.T) {
	root := repositoryRoot(t)
	temporary := t.TempDir()
	inputPath := filepath.Join(temporary, "source.json")
	outputPath := filepath.Join(temporary, "embedded_models.json")
	manifestPath := filepath.Join(temporary, "embedded_manifest.json")
	input, err := generatorSourceFromEmbedded(t)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(inputPath, input, 0o600); err != nil {
		t.Fatal(err)
	}

	command := exec.Command("go", "run", "./internal/modelcatalog/generate_embedded.go",
		"-input", inputPath,
		"-output", outputPath,
		"-manifest", manifestPath,
	)
	command.Dir = root
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("generate embedded snapshot: %v\n%s", err, output)
	}

	generatedSnapshot, err := os.ReadFile(outputPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(generatedSnapshot, embeddedSnapshot) {
		t.Fatal("generator output differs from the committed embedded snapshot")
	}
	if bytes.HasSuffix(generatedSnapshot, []byte{'\n'}) {
		t.Fatal("generator output has an unexpected terminal newline")
	}

	generatedManifest, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		ArtifactSHA256 string `json:"artifact_sha256"`
	}
	if err := json.Unmarshal(generatedManifest, &manifest); err != nil {
		t.Fatalf("decode generated manifest: %v", err)
	}
	artifactHash := fmt.Sprintf("%x", sha256.Sum256(generatedSnapshot))
	if manifest.ArtifactSHA256 != artifactHash {
		t.Fatalf("generated manifest artifact SHA-256 = %q, computed %q", manifest.ArtifactSHA256, artifactHash)
	}
	mutatedHash := fmt.Sprintf("%x", sha256.Sum256(append(bytes.Clone(generatedSnapshot), '\n')))
	if manifest.ArtifactSHA256 == mutatedHash {
		t.Fatal("generated manifest accepted a terminal-newline artifact mutation")
	}
}

func generatorSourceFromEmbedded(t *testing.T) ([]byte, error) {
	t.Helper()
	var snapshot catalogFile
	if err := json.Unmarshal(embeddedSnapshot, &snapshot); err != nil {
		return nil, fmt.Errorf("decode embedded snapshot: %w", err)
	}
	providers := make(map[string]any)
	for _, model := range snapshot.Models {
		provider, ok := providers[model.Provider].(map[string]any)
		if !ok {
			provider = map[string]any{"models": make(map[string]any)}
			providers[model.Provider] = provider
		}
		models := provider["models"].(map[string]any)
		value := map[string]any{
			"id":   model.ID,
			"name": model.Name,
		}
		if model.Family != "" {
			value["family"] = model.Family
		}
		for key, flag := range map[string]*bool{
			"attachment":  model.Attachment,
			"reasoning":   model.Reasoning,
			"tool_call":   model.ToolCall,
			"temperature": model.Temperature,
		} {
			if flag != nil {
				value[key] = *flag
			}
		}
		if model.Context > 0 {
			value["context_length"] = model.Context
		}
		if model.Output > 0 {
			value["max_output_tokens"] = model.Output
		}
		models[model.ID] = value
	}
	root := map[string]any{"version": snapshot.Version}
	for provider, value := range providers {
		root[provider] = value
	}
	return json.Marshal(root)
}

func repositoryRoot(t *testing.T) string {
	t.Helper()
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate catalog test")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(filename), "../.."))
}

func TestUpdateValidatesAndAtomicallyReplacesCache(t *testing.T) {
	cachePath := filepath.Join(t.TempDir(), "models.json")
	server := newCatalogTLSServer(t, http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write([]byte(`{"openai":{"models":{"updated":{"family":"gpt","limit":{"context":99}}}}}`))
	}))
	defer server.Close()
	store := NewStore(cachePath, StoreOptions{SourceURL: server.URL, Client: server.Client()})
	catalog, err := store.Update(t.Context())
	if err != nil || len(catalog.Models) != 1 {
		t.Fatalf("update = models %d, err %v", len(catalog.Models), err)
	}
	if _, origin, err := store.LoadWithOrigin(); err != nil || origin != OriginCache {
		t.Fatalf("updated cache origin = %q, err %v", origin, err)
	}

	oldData, err := os.ReadFile(cachePath)
	if err != nil {
		t.Fatal(err)
	}
	failing := NewStore(cachePath, StoreOptions{
		SourceURL: server.URL, Client: server.Client(),
		Rename: func(string, string) error { return errors.New("rename failed") },
	})
	if _, err := failing.Update(t.Context()); err == nil {
		t.Fatal("atomic replacement unexpectedly succeeded")
	}
	newData, err := os.ReadFile(cachePath)
	if err != nil {
		t.Fatal(err)
	}
	if string(newData) != string(oldData) {
		t.Fatal("failed atomic replacement changed the old cache")
	}
}

func TestUpdateRejectsEmptyCatalogWithoutReplacingCache(t *testing.T) {
	for _, test := range []struct {
		name string
		body string
	}{
		{name: "empty object", body: `{}`},
		{name: "empty provider models", body: `{"openai":{"models":{}}}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			cachePath := filepath.Join(t.TempDir(), "models.json")
			seedServer := newCatalogTLSServer(t, http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				_, _ = writer.Write([]byte(`{"openai":{"models":{"seed":{"family":"gpt"}}}}`))
			}))
			seedStore := NewStore(cachePath, StoreOptions{SourceURL: seedServer.URL, Client: seedServer.Client()})
			if _, err := seedStore.Update(t.Context()); err != nil {
				seedServer.Close()
				t.Fatal(err)
			}
			seedServer.Close()
			oldData, err := os.ReadFile(cachePath)
			if err != nil {
				t.Fatal(err)
			}

			server := newCatalogTLSServer(t, http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				_, _ = writer.Write([]byte(test.body))
			}))
			defer server.Close()
			replaceCalled := false
			store := NewStore(cachePath, StoreOptions{
				SourceURL: server.URL,
				Client:    server.Client(),
				Rename: func(string, string) error {
					replaceCalled = true
					return nil
				},
			})
			if _, err := store.Update(t.Context()); err == nil {
				t.Fatal("Update accepted an empty catalog")
			}
			if replaceCalled {
				t.Fatal("Update attempted to replace the cache for an invalid catalog")
			}
			newData, err := os.ReadFile(cachePath)
			if err != nil {
				t.Fatal(err)
			}
			if string(newData) != string(oldData) {
				t.Fatal("invalid catalog changed the old cache")
			}
		})
	}
}

func TestUpdateRejectsNoNetStatusOversizeBadJSONAndTimeout(t *testing.T) {
	tests := []struct {
		name string
		body string
		code int
		wait time.Duration
		want string
	}{
		{name: "non-2xx", body: "{}", code: http.StatusBadGateway, want: "status"},
		{name: "oversize", body: `{"openai":{"models":{}}}`, code: http.StatusOK, want: "size limit"},
		{name: "bad json", body: "{", code: http.StatusOK, want: "normalize"},
		{name: "timeout", body: `{}`, code: http.StatusOK, wait: 50 * time.Millisecond, want: "deadline"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := newCatalogTLSServer(t, http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				if test.wait > 0 {
					time.Sleep(test.wait)
				}
				writer.WriteHeader(test.code)
				_, _ = writer.Write([]byte(test.body))
			}))
			defer server.Close()
			options := StoreOptions{SourceURL: server.URL, Client: server.Client(), MaxResponseBytes: 10}
			if test.name == "timeout" {
				options.Client = &http.Client{Transport: server.Client().Transport, Timeout: time.Millisecond}
			}
			_, err := NewStore(filepath.Join(t.TempDir(), "models.json"), options).Update(context.Background())
			if err == nil || !strings.Contains(strings.ToLower(err.Error()), strings.ToLower(test.want)) {
				t.Fatalf("Update error = %v, want substring %q", err, test.want)
			}
		})
	}

	noNet := NewStore(filepath.Join(t.TempDir(), "models.json"), StoreOptions{NoNet: true})
	if _, err := noNet.Update(t.Context()); !errors.Is(err, errCatalogNoNet) {
		t.Fatalf("no-net update error = %v", err)
	}
}

func newCatalogTLSServer(t *testing.T, handler http.Handler) *httptest.Server {
	t.Helper()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &httptest.Server{Listener: listener, Config: &http.Server{Handler: handler}}
	server.StartTLS()
	return server
}
