package context

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"

	"github.com/kjelly/hufu/internal/embedding"
)

// keywordEmbedder maps text to fixed unit vectors so a test can place items
// near or far from a query, and counts encoder calls.
type keywordEmbedder struct {
	model     embedding.ModelIdentity
	vector    func(string) []float32
	mu        sync.Mutex
	documents int
	queries   int
}

func newKeywordEmbedder(vector func(string) []float32) *keywordEmbedder {
	model := newHashEmbedder().model
	model.Dimensions = 2
	return &keywordEmbedder{model: model, vector: vector}
}

// migrationKeywordVector puts the two migration phrases on one axis and
// every other text on the other.
func migrationKeywordVector(text string) []float32 {
	if text == "schema migration" || text == "database upgrade" {
		return []float32{1, 0}
	}
	return []float32{0, 1}
}

func (e *keywordEmbedder) Identity() embedding.ModelIdentity { return e.model }

func (e *keywordEmbedder) EmbedQuery(ctx context.Context, text string) ([]float32, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	e.mu.Lock()
	e.queries++
	e.mu.Unlock()
	return e.lookup(text)
}

func (e *keywordEmbedder) EmbedDocument(ctx context.Context, text string) ([]float32, error) {
	vectors, err := e.EmbedDocuments(ctx, []string{text})
	if err != nil {
		return nil, err
	}
	return vectors[0], nil
}

func (e *keywordEmbedder) EmbedDocuments(ctx context.Context, texts []string) ([][]float32, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	result := make([][]float32, len(texts))
	for i, text := range texts {
		vector, err := e.lookup(text)
		if err != nil {
			return nil, err
		}
		result[i] = vector
	}
	e.mu.Lock()
	e.documents += len(texts)
	e.mu.Unlock()
	return result, nil
}

func (e *keywordEmbedder) lookup(text string) ([]float32, error) {
	vector := e.vector(text)
	if vector == nil {
		return nil, fmt.Errorf("unexpected synthetic embedding input %q", text)
	}
	return slices.Clone(vector), nil
}

func (e *keywordEmbedder) calls() (documents, queries int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.documents, e.queries
}

// newRefreshedSemanticIndex builds the project's generation with embedder and
// returns an index over it, the way CLI retrieval commands open the lane.
func newRefreshedSemanticIndex(t *testing.T, repo *SQLiteRepository, projectID string, embedder embedding.Embedder) *SemanticIndex {
	t.Helper()
	if _, err := newTestBuilder(t, repo, embedder).Refresh(t.Context(), projectID); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	index, err := NewSemanticIndexWithOptions(repo, repo, embedder, SemanticIndexOptions{InventoryPolicy: InventoryPolicyRetrievable})
	if err != nil {
		t.Fatalf("NewSemanticIndexWithOptions: %v", err)
	}
	return index
}

func TestSemanticIndexSearchSimilarToUsesStoredVectorAndScope(t *testing.T) {
	repo := openProjectionTestRepository(t)
	team := Scope{ProjectID: "p", TeamID: "team"}
	if err := repo.Append(t.Context(),
		ContextItem{ID: "source", Kind: ContextDecision, Content: "source", Scope: team},
		ContextItem{ID: "near", Kind: ContextDecision, Content: "near", Scope: team},
		ContextItem{ID: "far", Kind: ContextDecision, Content: "far", Scope: team},
		ContextItem{ID: "other-team", Kind: ContextDecision, Content: "near twin", Scope: Scope{ProjectID: "p", TeamID: "other"}},
		ContextItem{ID: "agent-private", Kind: ContextDecision, Content: "near twin", Scope: Scope{ProjectID: "p", TeamID: "team", AgentID: "a"}},
	); err != nil {
		t.Fatal(err)
	}
	embedder := newKeywordEmbedder(func(text string) []float32 {
		switch text {
		case "source":
			return []float32{1, 0}
		case "near", "near twin":
			return []float32{0.8, 0.6}
		case "far":
			return []float32{0, 1}
		}
		return nil
	})
	index := newRefreshedSemanticIndex(t, repo, "p", embedder)
	seeded, _ := embedder.calls()

	results, err := index.SearchSimilarTo(t.Context(), "source", SearchRequest{Scope: team, Visibility: VisibilityExact, Limit: 10})
	if err != nil {
		t.Fatalf("SearchSimilarTo: %v", err)
	}
	if got := searchResultIDs(results); !slices.Equal(got, []string{"near", "far"}) {
		t.Fatalf("neighbors = %v, want [near far] without the source, other team, or agent child", got)
	}
	if score := results[0].Score; score < 0.79 || score > 0.81 {
		t.Fatalf("near score = %v, want cosine 0.8", score)
	}
	if documents, queries := embedder.calls(); documents != seeded || queries != 0 {
		t.Fatalf("SearchSimilarTo called the encoder: documents %d->%d, queries %d", seeded, documents, queries)
	}
}

func TestSemanticIndexSearchSimilarToRejectsItemsWithoutCurrentVectors(t *testing.T) {
	repo := openProjectionTestRepository(t)
	scope := Scope{ProjectID: "p"}
	if err := repo.Append(t.Context(), ContextItem{ID: "indexed", Kind: ContextDecision, Content: "indexed", Scope: scope}); err != nil {
		t.Fatal(err)
	}
	index := newRefreshedSemanticIndex(t, repo, "p", newKeywordEmbedder(func(string) []float32 { return []float32{1, 0} }))
	// Appended after the refresh, so the active generation lacks it.
	if err := repo.Append(t.Context(), ContextItem{ID: "fresh", Kind: ContextDecision, Content: "fresh", Scope: scope}); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name   string
		itemID string
		want   error
	}{
		{name: "unknown item", itemID: "missing", want: ErrVectorItemNotIndexed},
		{name: "item newer than the generation", itemID: "fresh", want: ErrVectorItemNotIndexed},
		{name: "uncovered candidate", itemID: "indexed", want: ErrSemanticProjectionIncomplete},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := index.SearchSimilarTo(t.Context(), tc.itemID, SearchRequest{Scope: scope, Limit: 5})
			if !errors.Is(err, tc.want) {
				t.Fatalf("SearchSimilarTo(%s) error = %v, want %v", tc.itemID, err, tc.want)
			}
		})
	}
}
