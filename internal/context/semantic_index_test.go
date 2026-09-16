package context

import (
	"context"
	"errors"
	"math"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/kjelly/hufu/internal/embedding"
)

func TestSemanticIndexAuthorizesBeforeDeterministicTopK(t *testing.T) {
	now := time.Now().UTC()
	items := []ContextItem{
		{ID: "a", Kind: ContextDecision, Content: "content a", Scope: Scope{ProjectID: "project", TeamID: "team"}, Confidence: 0.9},
		{ID: "b", Kind: ContextDecision, Content: "content b", Scope: Scope{ProjectID: "project", TeamID: "team"}, Confidence: 0.9},
		{ID: "c", Kind: ContextDecision, Content: "content c", Scope: Scope{ProjectID: "project", TeamID: "team"}, Confidence: 0.9},
		{ID: "private", Kind: ContextDecision, Content: "content private", Scope: Scope{ProjectID: "project", TeamID: "team", AgentID: "other"}, Confidence: 1},
		{ID: "candidate", Kind: ContextDecision, Content: "content candidate", Scope: Scope{ProjectID: "project", TeamID: "team"}, Lifecycle: LifecycleCandidate, Confidence: 1},
		{ID: "expired", Kind: ContextDecision, Content: "content expired", Scope: Scope{ProjectID: "project", TeamID: "team"}, ExpiresAt: timePointer(now.Add(-time.Hour)), Confidence: 1},
	}
	vectors := map[string][]float32{
		"a": {1, 0}, "b": {1, 0}, "c": {0, 1},
		"private": {1, 0}, "candidate": {1, 0}, "expired": {1, 0},
	}
	index, _, _, _ := buildSemanticTestIndex(t, items, vectors)

	results, err := index.SearchVector(t.Context(), SearchRequest{
		Query: "query", Scope: Scope{ProjectID: "project", TeamID: "team"}, Limit: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := searchResultIDs(results); !slices.Equal(got, []string{"a", "b"}) {
		t.Fatalf("result IDs = %v, want [a b]", got)
	}
}

func TestSemanticIndexAllowedIDsOnlyNarrowCanonicalAuthorization(t *testing.T) {
	items := []ContextItem{
		{ID: "a", Kind: ContextDecision, Content: "content a", Scope: Scope{ProjectID: "project", TeamID: "team"}, Confidence: 1},
		{ID: "c", Kind: ContextDecision, Content: "content c", Scope: Scope{ProjectID: "project", TeamID: "team"}, Confidence: 1},
		{ID: "private", Kind: ContextDecision, Content: "content private", Scope: Scope{ProjectID: "project", TeamID: "team", AgentID: "other"}, Confidence: 1},
	}
	vectors := map[string][]float32{"a": {1, 0}, "c": {0, 1}, "private": {1, 0}}
	index, repository, _, _ := buildSemanticTestIndex(t, items, vectors)
	ids := []string{"private", "c", "a", "a"}
	req := SearchRequest{Query: "content", Scope: Scope{ProjectID: "project", TeamID: "team"}}.WithAllowedItemIDs(ids)
	ids[0] = "mutated"
	if !slices.Equal(req.AllowedItemIDs, []string{"a", "c", "private"}) {
		t.Fatalf("normalized allowed IDs = %v", req.AllowedItemIDs)
	}
	results, err := index.SearchVector(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	if got := searchResultIDs(results); !slices.Equal(got, []string{"a", "c"}) {
		t.Fatalf("semantic allowed IDs = %v, want [a c]", got)
	}
	exact, err := repository.SearchExact(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	gotExact := searchResultIDs(exact)
	slices.Sort(gotExact)
	if !slices.Equal(gotExact, []string{"a", "c"}) {
		t.Fatalf("exact allowed IDs = %v, want [a c]", gotExact)
	}

	blocked := req.WithAllowedItemIDs([]string{})
	results, err = index.SearchVector(t.Context(), blocked)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 0 {
		t.Fatalf("non-nil empty allowed IDs returned %v", searchResultIDs(results))
	}
}

func TestSemanticIndexAppliesKindAndMinimumConfidenceBeforeScoring(t *testing.T) {
	items := []ContextItem{
		{ID: "decision-high", Kind: ContextDecision, Content: "high", Scope: Scope{ProjectID: "project"}, Confidence: 0.9},
		{ID: "decision-low", Kind: ContextDecision, Content: "low", Scope: Scope{ProjectID: "project"}, Confidence: 0.2},
		{ID: "error-high", Kind: ContextError, Content: "error", Scope: Scope{ProjectID: "project"}, Confidence: 1},
	}
	vectors := map[string][]float32{
		"decision-high": {0, 1}, "decision-low": {1, 0}, "error-high": {1, 0},
	}
	index, _, _, _ := buildSemanticTestIndex(t, items, vectors)
	minimum := 0.8
	results, err := index.SearchVector(t.Context(), SearchRequest{
		Query: "query", Scope: Scope{ProjectID: "project"},
		Kinds: []ContextKind{ContextDecision}, MinConfidence: &minimum,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := searchResultIDs(results); !slices.Equal(got, []string{"decision-high"}) {
		t.Fatalf("filtered result IDs = %v", got)
	}
}

func TestSemanticIndexReloadsWhenGenerationChangesAtSameRevision(t *testing.T) {
	items := []ContextItem{
		{ID: "a", Kind: ContextDecision, Content: "a", Scope: Scope{ProjectID: "project"}, Confidence: 1},
		{ID: "b", Kind: ContextDecision, Content: "b", Scope: Scope{ProjectID: "project"}, Confidence: 1},
	}
	index, repository, inventory, model := buildSemanticTestIndex(t, items, map[string][]float32{
		"a": {1, 0}, "b": {0, 1},
	})
	request := SearchRequest{Query: "query", Scope: Scope{ProjectID: "project"}, Limit: 1}
	results, err := index.SearchVector(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	if got := searchResultIDs(results); !slices.Equal(got, []string{"a"}) {
		t.Fatalf("initial result IDs = %v", got)
	}

	second := beginProjectionGeneration(t, repository, "semantic-second", inventory, model)
	appendSemanticRows(t, repository, second.GenerationID, inventory, map[string][]float32{
		"a": {0, 1}, "b": {1, 0},
	})
	tx, err := repository.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = tx.ExecContext(t.Context(), "UPDATE context_embedding_generations SET state='superseded' WHERE state='active'"); err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	if _, err = tx.ExecContext(t.Context(), "UPDATE context_embedding_generations SET state='active',activated_at=? WHERE generation_id=?", time.Now().UnixMilli(), second.GenerationID); err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}

	results, err = index.SearchVector(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	if got := searchResultIDs(results); !slices.Equal(got, []string{"b"}) {
		t.Fatalf("reloaded result IDs = %v, want [b]", got)
	}
}

func TestSemanticIndexRejectsCorruptActiveSnapshotWithoutPartialResults(t *testing.T) {
	items := []ContextItem{
		{ID: "a", Kind: ContextDecision, Content: "a", Scope: Scope{ProjectID: "project"}, Confidence: 1},
		{ID: "b", Kind: ContextDecision, Content: "b", Scope: Scope{ProjectID: "project"}, Confidence: 1},
	}
	tests := []struct {
		name   string
		mutate func(*testing.T, *SQLiteRepository, string)
	}{
		{
			name: "missing row",
			mutate: func(t *testing.T, repository *SQLiteRepository, generationID string) {
				_, err := repository.db.ExecContext(t.Context(), "DELETE FROM context_embeddings WHERE generation_id=? AND item_id='b'", generationID)
				if err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "content hash mismatch",
			mutate: func(t *testing.T, repository *SQLiteRepository, generationID string) {
				_, err := repository.db.ExecContext(t.Context(), "UPDATE context_embeddings SET content_hash=? WHERE generation_id=? AND item_id='a'", projectionHash("wrong"), generationID)
				if err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "dimension mismatch",
			mutate: func(t *testing.T, repository *SQLiteRepository, generationID string) {
				_, err := repository.db.ExecContext(t.Context(), "UPDATE context_embeddings SET dimensions=1,vector=? WHERE generation_id=? AND item_id='a'", encodeEmbeddingVector([]float32{1}), generationID)
				if err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "non-finite vector",
			mutate: func(t *testing.T, repository *SQLiteRepository, generationID string) {
				_, err := repository.db.ExecContext(t.Context(), "UPDATE context_embeddings SET vector=? WHERE generation_id=? AND item_id='a'", encodeEmbeddingVector([]float32{float32(math.NaN()), 0}), generationID)
				if err != nil {
					t.Fatal(err)
				}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			index, repository, _, model := buildSemanticTestIndex(t, items, map[string][]float32{"a": {1, 0}, "b": {0, 1}})
			active, err := repository.ActiveGeneration(t.Context(), "project", model)
			if err != nil {
				t.Fatal(err)
			}
			test.mutate(t, repository, active.GenerationID)
			results, err := index.SearchVector(t.Context(), SearchRequest{Query: "query", Scope: Scope{ProjectID: "project"}})
			if !errors.Is(err, ErrSemanticSnapshotCorrupt) {
				t.Fatalf("corrupt snapshot error = %v", err)
			}
			if len(results) != 0 {
				t.Fatalf("corrupt snapshot returned partial results: %v", searchResultIDs(results))
			}
		})
	}
}

func TestSemanticIndexCancellationAndConcurrentRefresh(t *testing.T) {
	items := []ContextItem{{ID: "a", Kind: ContextDecision, Content: "a", Scope: Scope{ProjectID: "project"}, Confidence: 1}}
	index, _, _, _ := buildSemanticTestIndex(t, items, map[string][]float32{"a": {1, 0}})
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := index.SearchVector(canceled, SearchRequest{Query: "query", Scope: Scope{ProjectID: "project"}}); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled search error = %v", err)
	}

	var group sync.WaitGroup
	errorsFound := make(chan error, 16)
	for range 16 {
		group.Go(func() {
			results, err := index.SearchVector(t.Context(), SearchRequest{Query: "query", Scope: Scope{ProjectID: "project"}})
			if err != nil {
				errorsFound <- err
				return
			}
			if len(results) != 1 || results[0].Item.ID != "a" {
				errorsFound <- errors.New("unexpected concurrent semantic result")
			}
		})
	}
	group.Wait()
	close(errorsFound)
	for err := range errorsFound {
		t.Error(err)
	}
}

func TestSemanticIndexWithoutCurrentActiveGenerationIsStale(t *testing.T) {
	repository := openProjectionTestRepository(t)
	embedder := semanticTestEmbedder{model: projectionTestModel("model", "1"), query: []float32{1, 0}}
	index, err := NewSemanticIndex(repository, repository, embedder)
	if err != nil {
		t.Fatal(err)
	}
	_, err = index.SearchVector(t.Context(), SearchRequest{Query: "query", Scope: Scope{ProjectID: "project"}})
	if !errors.Is(err, ErrSemanticIndexStale) {
		t.Fatalf("missing generation error = %v", err)
	}
}

type semanticTestEmbedder struct {
	model embedding.ModelIdentity
	query []float32
}

func (e semanticTestEmbedder) Identity() embedding.ModelIdentity { return e.model }

func (e semanticTestEmbedder) EmbedQuery(ctx context.Context, _ string) ([]float32, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return slices.Clone(e.query), nil
}

func (e semanticTestEmbedder) EmbedDocument(ctx context.Context, _ string) ([]float32, error) {
	return e.EmbedQuery(ctx, "")
}

func (e semanticTestEmbedder) EmbedDocuments(ctx context.Context, texts []string) ([][]float32, error) {
	result := make([][]float32, len(texts))
	for i := range texts {
		vector, err := e.EmbedDocument(ctx, texts[i])
		if err != nil {
			return nil, err
		}
		result[i] = vector
	}
	return result, nil
}

func buildSemanticTestIndex(
	t *testing.T,
	items []ContextItem,
	vectors map[string][]float32,
) (*SemanticIndex, *SQLiteRepository, EmbeddingInventory, embedding.ModelIdentity) {
	t.Helper()
	repository := openProjectionTestRepository(t)
	if err := repository.Append(t.Context(), items...); err != nil {
		t.Fatal(err)
	}
	inventory := loadProjectionInventory(t, repository, "project")
	model := projectionTestModel("semantic-test", "1")
	generation := beginProjectionGeneration(t, repository, "semantic-active", inventory, model)
	appendSemanticRows(t, repository, generation.GenerationID, inventory, vectors)
	if _, err := repository.ActivateGeneration(t.Context(), generation.GenerationID, inventory.Revision, inventory.Digest); err != nil {
		t.Fatal(err)
	}
	index, err := NewSemanticIndex(repository, repository, semanticTestEmbedder{model: model, query: []float32{1, 0}})
	if err != nil {
		t.Fatal(err)
	}
	return index, repository, inventory, model
}

func appendSemanticRows(
	t *testing.T,
	repository *SQLiteRepository,
	generationID string,
	inventory EmbeddingInventory,
	vectors map[string][]float32,
) {
	t.Helper()
	rows := make([]EmbeddingRow, len(inventory.Items))
	for i, item := range inventory.Items {
		vector, ok := vectors[item.ItemID]
		if !ok {
			t.Fatalf("missing test vector for %s", item.ItemID)
		}
		rows[i] = EmbeddingRow{ItemID: item.ItemID, ContentHash: item.ContentHash, Dimensions: len(vector), Vector: slices.Clone(vector)}
	}
	if err := repository.AppendGenerationRows(t.Context(), generationID, rows); err != nil {
		t.Fatal(err)
	}
}

func searchResultIDs(results []SearchResult) []string {
	ids := make([]string, len(results))
	for i, result := range results {
		ids[i] = result.Item.ID
	}
	return ids
}

func timePointer(value time.Time) *time.Time { return &value }
