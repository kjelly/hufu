package context

import (
	"context"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func testEmbedding(_ context.Context, text string) ([]float32, error) {
	if text == "schema migration" || text == "database upgrade" {
		return []float32{1, 0}, nil
	}
	return []float32{0, 1}, nil
}

func TestVectorStoreSupportsConcurrentRebuildAndSearch(t *testing.T) {
	dir := t.TempDir()
	repo, err := OpenSQLite(filepath.Join(dir, "context.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	scope := Scope{ProjectID: "project"}
	if err := repo.Append(context.Background(), ContextItem{ID: "item", Kind: ContextPattern, Content: "schema migration", Scope: scope}); err != nil {
		t.Fatal(err)
	}
	store, err := NewVectorStore(filepath.Join(dir, "vectors"), "test-v1", testEmbedding)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Rebuild(context.Background(), repo, scope); err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	var rebuilds, searches atomic.Int64
	wg.Add(2)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			if err := store.Rebuild(context.Background(), repo, scope); err != nil {
				errs <- err
				return
			}
			rebuilds.Add(1)
		}
	}()
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			if _, err := store.SearchVector(context.Background(), SearchRequest{Query: "database upgrade", Scope: scope, Limit: 1}); err != nil {
				errs <- err
				return
			}
			searches.Add(1)
		}
	}()
	time.Sleep(200 * time.Millisecond)
	close(stop)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("concurrent vector operation: %v", err)
	}
	if rebuilds.Load() == 0 || searches.Load() == 0 {
		t.Fatalf("expected both operations to run, rebuilds=%d searches=%d", rebuilds.Load(), searches.Load())
	}
}

func TestVectorStoreRebuildsCanonicalItemsAndFindsSemanticMatch(t *testing.T) {
	dir := t.TempDir()
	repo, err := OpenSQLite(filepath.Join(dir, "context.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	scope := Scope{ProjectID: "project"}
	if err := repo.Append(context.Background(), ContextItem{ID: "semantic", Kind: ContextPattern, Content: "schema migration", Scope: scope}); err != nil {
		t.Fatal(err)
	}
	store, err := NewVectorStore(filepath.Join(dir, "vectors"), "test-v1", testEmbedding)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Rebuild(context.Background(), repo, scope); err != nil {
		t.Fatal(err)
	}
	got, err := store.SearchVector(context.Background(), SearchRequest{Query: "database upgrade", Scope: scope, Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Item.ID != "semantic" {
		t.Fatalf("semantic vector result=%#v", got)
	}
	if _, err := NewVectorStore(filepath.Join(dir, "vectors"), "test-v2", testEmbedding); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Get(context.Background(), "semantic"); err != nil {
		t.Fatalf("model migration deleted canonical item: %v", err)
	}
}

func TestVectorStoreHydratesCanonicalItemsAndFiltersTeamScope(t *testing.T) {
	dir := t.TempDir()
	repo, err := OpenSQLite(filepath.Join(dir, "context.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	ctx := context.Background()
	if err := repo.Append(ctx,
		ContextItem{ID: "team-a", Kind: ContextPattern, Content: "schema migration", Scope: Scope{ProjectID: "project", TeamID: "team-a"}},
		ContextItem{ID: "team-b", Kind: ContextPattern, Content: "database upgrade", Scope: Scope{ProjectID: "project", TeamID: "team-b"}},
	); err != nil {
		t.Fatal(err)
	}
	store, err := NewVectorStore(filepath.Join(dir, "vectors"), "test-v1", testEmbedding)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Rebuild(ctx, repo, Scope{ProjectID: "project"}); err != nil {
		t.Fatal(err)
	}
	found, err := store.SearchVector(ctx, SearchRequest{Query: "database upgrade", Scope: Scope{ProjectID: "project", TeamID: "team-a"}, Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 1 || found[0].Item.ID != "team-a" || found[0].Item.Scope.TeamID != "team-a" {
		t.Fatalf("vector search leaked another team's canonical item: %#v", found)
	}
}

func TestVectorStoreRecordsFailedEmbeddingsAndRetriesThemOnRebuild(t *testing.T) {
	dir := t.TempDir()
	repo, err := OpenSQLite(filepath.Join(dir, "context.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	ctx := context.Background()
	scope := Scope{ProjectID: "project"}
	if err := repo.Append(ctx, ContextItem{ID: "retry-me", Kind: ContextPattern, Content: "schema migration", Scope: scope}); err != nil {
		t.Fatal(err)
	}
	failingEmbedding := func(context.Context, string) ([]float32, error) { return nil, context.DeadlineExceeded }
	store, err := NewVectorStore(filepath.Join(dir, "vectors"), "test-v1", failingEmbedding)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Rebuild(ctx, repo, scope); err == nil {
		t.Fatal("Rebuild succeeded despite a failed embedding")
	}
	failed, err := repo.Get(ctx, "retry-me")
	if err != nil {
		t.Fatal(err)
	}
	if failed.EmbeddingState != "pending" {
		t.Fatalf("failed embedding changed canonical state: %#v", failed)
	}

	store, err = NewVectorStore(filepath.Join(dir, "vectors"), "test-v1", testEmbedding)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Rebuild(ctx, repo, scope); err != nil {
		t.Fatalf("rebuild did not retry the failed embedding: %v", err)
	}
	retried, err := repo.Get(ctx, "retry-me")
	if err != nil {
		t.Fatal(err)
	}
	if retried.EmbeddingState != "embedded" || retried.EmbeddingModel != "test-v1" {
		t.Fatalf("successful retry was not recorded in canonical store: %#v", retried)
	}
}

func TestVectorStoreContinuesAfterAnEmbeddingFailure(t *testing.T) {
	dir := t.TempDir()
	repo, err := OpenSQLite(filepath.Join(dir, "context.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	ctx := context.Background()
	scope := Scope{ProjectID: "project"}
	if err := repo.Append(ctx,
		ContextItem{ID: "fails", Kind: ContextPattern, Content: "unembeddable document", Scope: scope},
		ContextItem{ID: "succeeds", Kind: ContextPattern, Content: "schema migration", Scope: scope},
	); err != nil {
		t.Fatal(err)
	}
	embed := func(_ context.Context, text string) ([]float32, error) {
		if text == "unembeddable document" {
			return nil, context.DeadlineExceeded
		}
		return []float32{1, 0}, nil
	}
	store, err := NewVectorStore(filepath.Join(dir, "vectors"), "test-v1", embed)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Rebuild(ctx, repo, scope); err == nil {
		t.Fatal("Rebuild succeeded despite one failed embedding")
	}
	failed, err := repo.Get(ctx, "fails")
	if err != nil {
		t.Fatal(err)
	}
	if failed.EmbeddingState != "pending" {
		t.Fatalf("failed item state changed: %#v", failed)
	}
	succeeded, err := repo.Get(ctx, "succeeds")
	if err != nil {
		t.Fatal(err)
	}
	if succeeded.EmbeddingState != "embedded" || succeeded.EmbeddingModel != "test-v1" {
		t.Fatalf("successful item was not embedded after earlier failure: %#v", succeeded)
	}
	found, err := store.SearchVector(ctx, SearchRequest{Query: "schema migration", Scope: scope, Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 1 || found[0].Item.ID != "succeeds" {
		t.Fatalf("successful item was not indexed after earlier failure: %#v", found)
	}
}

func TestVectorStoreRebuildFailureClearsPreviouslyEmbeddedState(t *testing.T) {
	dir := t.TempDir()
	repo, err := OpenSQLite(filepath.Join(dir, "context.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	ctx := context.Background()
	scope := Scope{ProjectID: "project"}
	if err := repo.Append(ctx, ContextItem{ID: "was-embedded", Kind: ContextPattern, Content: "schema migration", Scope: scope}); err != nil {
		t.Fatal(err)
	}
	store, err := NewVectorStore(filepath.Join(dir, "vectors"), "test-v1", testEmbedding)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Rebuild(ctx, repo, scope); err != nil {
		t.Fatal(err)
	}
	before, err := repo.Get(ctx, "was-embedded")
	if err != nil {
		t.Fatal(err)
	}
	if before.EmbeddingState != "embedded" {
		t.Fatalf("setup did not embed item: %#v", before)
	}

	failingEmbedding := func(context.Context, string) ([]float32, error) { return nil, context.DeadlineExceeded }
	store, err = NewVectorStore(filepath.Join(dir, "vectors"), "test-v1", failingEmbedding)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Rebuild(ctx, repo, scope); err == nil {
		t.Fatal("Rebuild succeeded despite failed re-embedding")
	}
	after, err := repo.Get(ctx, "was-embedded")
	if err != nil {
		t.Fatal(err)
	}
	if after.EmbeddingState == "embedded" {
		t.Fatalf("failed rebuild left stale embedded state: %#v", after)
	}
	if after.EmbeddingState != "pending" {
		t.Fatalf("failed rebuild state=%q, want pending", after.EmbeddingState)
	}
}
