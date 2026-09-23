package context

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func TestOpenOllamaVectorStoreSendsUnprefixedModel(t *testing.T) {
	var mu sync.Mutex
	var models []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		models = append(models, r.URL.Path+" "+body.Model)
		mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"embedding": []float32{0.6, 0.8}})
	}))
	defer server.Close()

	workspace := t.TempDir()
	repo, err := OpenSQLite(filepath.Join(workspace, "context.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	scope := Scope{ProjectID: "project"}
	if err = repo.Append(context.Background(), ContextItem{ID: "item", Kind: ContextPattern, Content: "schema migration", Scope: scope}); err != nil {
		t.Fatal(err)
	}
	store, err := OpenOllamaVectorStore(workspace, "ollama/nomic-embed-text:latest", server.URL)
	if err != nil {
		t.Fatal(err)
	}
	if err = store.Rebuild(context.Background(), repo, scope); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(models) == 0 || models[0] != "/embeddings nomic-embed-text:latest" {
		t.Fatalf("Ollama requests = %v, want the unprefixed model name", models)
	}
	identity, err := os.ReadFile(filepath.Join(workspace, "context-vectors", "embedding_model"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(identity)) != "ollama/nomic-embed-text:latest" {
		t.Fatalf("index identity = %q, want the original model string", identity)
	}
}

func similarTestEmbedding(calls *atomic.Int64) func(context.Context, string) ([]float32, error) {
	return func(_ context.Context, text string) ([]float32, error) {
		calls.Add(1)
		switch {
		case strings.Contains(text, "SQLite"), strings.Contains(text, "PostgreSQL"):
			return []float32{1, 0.1}, nil
		default:
			return []float32{0, 1}, nil
		}
	}
}

func TestSearchSimilarToUsesStoredEmbeddingAndAppliesScope(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	repo, err := OpenSQLite(filepath.Join(dir, "context.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	shared := Scope{ProjectID: "p", TeamID: "t"}
	private := Scope{ProjectID: "p", TeamID: "t", AgentID: "worker"}
	for _, item := range []ContextItem{
		{ID: "sqlite", Kind: ContextDecision, Content: "Use SQLite for storage.", Scope: shared},
		{ID: "postgres", Kind: ContextArchitecture, Content: "Architecture migrated to PostgreSQL.", Scope: shared},
		{ID: "private-postgres", Kind: ContextDecision, Content: "PostgreSQL only for the worker.", Scope: private},
		{ID: "unrelated", Kind: ContextConvention, Content: "Tabs for indentation.", Scope: shared},
	} {
		if err = repo.Append(ctx, item); err != nil {
			t.Fatal(err)
		}
	}
	var calls atomic.Int64
	store, err := NewVectorStore(filepath.Join(dir, "vectors"), "test-similar", similarTestEmbedding(&calls))
	if err != nil {
		t.Fatal(err)
	}
	if err = store.Rebuild(ctx, repo, shared); err != nil {
		t.Fatal(err)
	}
	afterRebuild := calls.Load()
	results, err := store.SearchSimilarTo(ctx, "sqlite", SearchRequest{Scope: shared, Visibility: VisibilityExact, Limit: 5})
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != afterRebuild {
		t.Fatalf("SearchSimilarTo made %d embedding calls, want 0", calls.Load()-afterRebuild)
	}
	if len(results) == 0 || results[0].Item.ID != "postgres" {
		t.Fatalf("results = %+v, want postgres first", results)
	}
	for _, result := range results {
		if result.Item.ID == "sqlite" {
			t.Fatal("source item returned as its own neighbor")
		}
		if result.Item.ID == "private-postgres" {
			t.Fatal("private item leaked through exact shared scope")
		}
	}
}

func TestSearchSimilarToUnknownItem(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	repo, err := OpenSQLite(filepath.Join(dir, "context.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	scope := Scope{ProjectID: "p"}
	if err = repo.Append(ctx, ContextItem{ID: "item", Kind: ContextPattern, Content: "Use SQLite.", Scope: scope}); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int64
	store, err := NewVectorStore(filepath.Join(dir, "vectors"), "test-similar", similarTestEmbedding(&calls))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.SearchSimilarTo(ctx, "item", SearchRequest{Scope: scope}); err == nil {
		t.Fatal("search before rebuild succeeded")
	}
	if err = store.Rebuild(ctx, repo, scope); err != nil {
		t.Fatal(err)
	}
	if _, err = store.SearchSimilarTo(ctx, "missing", SearchRequest{Scope: scope}); !errors.Is(err, ErrVectorItemNotIndexed) {
		t.Fatalf("unknown item err = %v, want ErrVectorItemNotIndexed", err)
	}
}
