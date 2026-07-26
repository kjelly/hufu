package context

import (
	"context"
	"path/filepath"
	"testing"
)

func testEmbedding(_ context.Context, text string) ([]float32, error) {
	if text == "schema migration" || text == "database upgrade" {
		return []float32{1, 0}, nil
	}
	return []float32{0, 1}, nil
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
