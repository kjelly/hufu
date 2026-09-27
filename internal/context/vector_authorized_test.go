package context

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

// BUG-13: the store took chromem's top-limit neighbours before authorizing
// them, so out-of-scope neighbours shrank the result below limit.
func TestVectorSearchFillsLimitPastUnauthorizedNeighbours(t *testing.T) {
	dir := t.TempDir()
	repo, err := OpenSQLite(filepath.Join(dir, "context.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	var items []ContextItem
	for i := range 12 {
		items = append(items, ContextItem{ID: fmt.Sprintf("rival-%02d", i), Kind: ContextPattern, Content: fmt.Sprintf("rival note %d", i), Scope: Scope{ProjectID: "project", TeamID: "rival"}})
	}
	for i := range 3 {
		items = append(items, ContextItem{ID: fmt.Sprintf("mine-%d", i), Kind: ContextPattern, Content: fmt.Sprintf("mine note %d", i), Scope: Scope{ProjectID: "project", TeamID: "mine"}})
	}
	if err := repo.Append(context.Background(), items...); err != nil {
		t.Fatal(err)
	}
	embed := func(_ context.Context, text string) ([]float32, error) {
		if strings.HasPrefix(text, "mine") {
			return []float32{0.8, 0.6}, nil
		}
		return []float32{1, 0}, nil
	}
	store, err := NewVectorStore(filepath.Join(dir, "vectors"), "test-v1", embed)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Rebuild(context.Background(), repo, Scope{ProjectID: "project"}); err != nil {
		t.Fatal(err)
	}
	req := SearchRequest{Query: "needle", Scope: Scope{ProjectID: "project", TeamID: "mine"}, Limit: 3}
	results, err := store.SearchVector(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 3 {
		t.Fatalf("SearchVector returned %d authorized results, want 3", len(results))
	}
	similar, err := store.SearchSimilarTo(context.Background(), "mine-0", SearchRequest{Scope: req.Scope, Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(similar) != 2 || similar[0].Item.ID == "mine-0" || similar[1].Item.ID == "mine-0" {
		t.Fatalf("SearchSimilarTo = %v, want the two other authorized items", resultIDs(similar))
	}
	for _, result := range append(results, similar...) {
		if result.Item.Scope.TeamID != "mine" {
			t.Fatalf("unauthorized result %s", result.Item.ID)
		}
	}
}
