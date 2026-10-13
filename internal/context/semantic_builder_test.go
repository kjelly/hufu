package context

import (
	"context"
	"crypto/sha256"
	"errors"
	"math"
	"strings"
	"sync"
	"testing"

	"github.com/kjelly/hufu/internal/embedding"
)

// hashEmbedder derives a deterministic unit vector from the text and counts
// how many documents it embedded.
type hashEmbedder struct {
	model     embedding.ModelIdentity
	mu        sync.Mutex
	documents int
	queries   int
	failWith  error
}

func newHashEmbedder() *hashEmbedder {
	return &hashEmbedder{model: embedding.ModelIdentity{
		Scheme: embedding.IdentitySchemeOllamaEncoder, ID: "ollama/embeddinggemma:latest",
		Revision: projectionHash("profile"), Dimensions: 4, ManifestSHA256: projectionHash("digest"),
	}}
}

func (e *hashEmbedder) Identity() embedding.ModelIdentity { return e.model }

func hashVector(text string) []float32 {
	sum := sha256.Sum256([]byte(text))
	vector := make([]float32, 4)
	var norm float64
	for i := range vector {
		vector[i] = float32(sum[i]) + 1
		norm += float64(vector[i]) * float64(vector[i])
	}
	for i := range vector {
		vector[i] /= float32(math.Sqrt(norm))
	}
	return vector
}

func (e *hashEmbedder) EmbedQuery(ctx context.Context, text string) ([]float32, error) {
	e.mu.Lock()
	e.queries++
	e.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return hashVector(text), nil
}

func (e *hashEmbedder) EmbedDocument(ctx context.Context, text string) ([]float32, error) {
	vectors, err := e.EmbedDocuments(ctx, []string{text})
	if err != nil {
		return nil, err
	}
	return vectors[0], nil
}

func (e *hashEmbedder) EmbedDocuments(ctx context.Context, texts []string) ([][]float32, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.failWith != nil {
		return nil, e.failWith
	}
	result := make([][]float32, len(texts))
	for i, text := range texts {
		if strings.Contains(text, "HUGE") {
			return nil, embedding.ErrEmbeddingInputTooLong
		}
		result[i] = hashVector(text)
	}
	e.documents += len(texts)
	return result, nil
}

func (e *hashEmbedder) embedded() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.documents
}

func newTestBuilder(t *testing.T, repository *SQLiteRepository, embedder embedding.Embedder) *SemanticBuilder {
	t.Helper()
	builder, err := NewSemanticBuilder(repository, embedder, SemanticBuilderOptions{BatchSize: 2})
	if err != nil {
		t.Fatal(err)
	}
	return builder
}

func TestSemanticBuilderRefreshIsIncremental(t *testing.T) {
	repository := openProjectionTestRepository(t)
	appendProjectionItems(t, repository, "p", "a", "b", "c")
	embedder := newHashEmbedder()
	builder := newTestBuilder(t, repository, embedder)
	ctx := t.Context()

	first, err := builder.Refresh(ctx, "p")
	if err != nil {
		t.Fatal(err)
	}
	if !first.Activated || first.Embedded != 3 || first.Reused != 0 || embedder.embedded() != 3 {
		t.Fatalf("initial refresh = %+v, embedded %d", first, embedder.embedded())
	}

	unchanged, err := builder.Refresh(ctx, "p")
	if err != nil {
		t.Fatal(err)
	}
	if !unchanged.Unchanged || unchanged.GenerationID != first.GenerationID || embedder.embedded() != 3 {
		t.Fatalf("unchanged refresh = %+v, embedded %d", unchanged, embedder.embedded())
	}

	// Writes outside the policy (rejected, other project) change nothing.
	if err := repository.Append(ctx,
		ContextItem{ID: "rejected", Kind: ContextDecision, Content: "rejected", Scope: Scope{ProjectID: "p"}, Lifecycle: LifecycleRejected},
		ContextItem{ID: "elsewhere", Kind: ContextDecision, Content: "elsewhere", Scope: Scope{ProjectID: "q"}},
	); err != nil {
		t.Fatal(err)
	}
	if again, err := builder.Refresh(ctx, "p"); err != nil || !again.Unchanged {
		t.Fatalf("refresh after unrelated writes = %+v, %v", again, err)
	}

	appendProjectionItems(t, repository, "p", "d")
	if _, err := repository.db.ExecContext(ctx, "UPDATE context_items SET content='edited b', content_hash=? WHERE id='b'", projectionHash("edited b")); err != nil {
		t.Fatal(err)
	}
	second, err := builder.Refresh(ctx, "p")
	if err != nil {
		t.Fatal(err)
	}
	if !second.Activated || second.Embedded != 2 || second.Reused != 2 || second.PreviousGenerationID != first.GenerationID {
		t.Fatalf("incremental refresh = %+v", second)
	}
	if embedder.embedded() != 5 {
		t.Fatalf("embedded %d documents in total, want 5", embedder.embedded())
	}
	previous, err := loadGeneration(ctx, repository.db, first.GenerationID)
	if err != nil {
		t.Fatal(err)
	}
	if previous.State != GenerationSuperseded {
		t.Fatalf("previous generation state = %s", previous.State)
	}

	if _, err := repository.db.ExecContext(ctx, "UPDATE context_items SET lifecycle='rejected' WHERE id='c'"); err != nil {
		t.Fatal(err)
	}
	third, err := builder.Refresh(ctx, "p")
	if err != nil {
		t.Fatal(err)
	}
	if !third.Activated || third.Embedded != 0 || third.Reused != 3 || third.Inventory != 3 {
		t.Fatalf("refresh after removal = %+v", third)
	}

	index, err := NewSemanticIndexWithOptions(repository, repository, embedder, SemanticIndexOptions{InventoryPolicy: InventoryPolicyRetrievable})
	if err != nil {
		t.Fatal(err)
	}
	results, err := index.SearchVector(ctx, SearchRequest{Query: "content for d", Scope: Scope{ProjectID: "p"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 3 || results[0].Item.ID != "d" {
		t.Fatalf("search after refresh = %v", searchResultIDs(results))
	}
}

func TestSemanticBuilderExcludesOversizeDocuments(t *testing.T) {
	repository := openProjectionTestRepository(t)
	if err := repository.Append(t.Context(),
		ContextItem{ID: "a", Kind: ContextDecision, Content: "normal a", Scope: Scope{ProjectID: "p"}},
		ContextItem{ID: "b", Kind: ContextDecision, Content: "HUGE b", Scope: Scope{ProjectID: "p"}},
		ContextItem{ID: "c", Kind: ContextDecision, Content: "normal c", Scope: Scope{ProjectID: "p"}},
	); err != nil {
		t.Fatal(err)
	}
	embedder := newHashEmbedder()
	result, err := newTestBuilder(t, repository, embedder).Refresh(t.Context(), "p")
	if err != nil {
		t.Fatal(err)
	}
	if !result.Activated || result.Embedded != 2 || result.Excluded != 1 {
		t.Fatalf("refresh = %+v", result)
	}
	index, err := NewSemanticIndexWithOptions(repository, repository, embedder, SemanticIndexOptions{InventoryPolicy: InventoryPolicyRetrievable})
	if err != nil {
		t.Fatal(err)
	}
	results, err := index.SearchVector(t.Context(), SearchRequest{Query: "normal a", Scope: Scope{ProjectID: "p"}})
	if err != nil {
		t.Fatalf("oversize item must count as covered: %v", err)
	}
	if got := searchResultIDs(results); len(got) != 2 || got[0] != "a" {
		t.Fatalf("results = %v", got)
	}
	// A carried-forward exclusion survives the next incremental refresh.
	appendProjectionItems(t, repository, "p", "d")
	next, err := newTestBuilder(t, repository, embedder).Refresh(t.Context(), "p")
	if err != nil {
		t.Fatal(err)
	}
	if next.Embedded != 1 || next.Excluded != 1 || next.Reused != 3 {
		t.Fatalf("next refresh = %+v", next)
	}
}

func TestSemanticBuilderFailuresLeaveActiveGeneration(t *testing.T) {
	repository := openProjectionTestRepository(t)
	appendProjectionItems(t, repository, "p", "a")
	embedder := newHashEmbedder()
	builder := newTestBuilder(t, repository, embedder)
	first, err := builder.Refresh(t.Context(), "p")
	if err != nil {
		t.Fatal(err)
	}
	appendProjectionItems(t, repository, "p", "b")

	embedder.failWith = embedding.ErrOllamaUnavailable
	if _, err = builder.Refresh(t.Context(), "p"); !errors.Is(err, embedding.ErrOllamaUnavailable) {
		t.Fatalf("refresh error = %v", err)
	}
	embedder.failWith = nil
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err = builder.Refresh(canceled, "p"); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled refresh error = %v", err)
	}
	generations, err := repository.ListGenerations(t.Context(), "p")
	if err != nil {
		t.Fatal(err)
	}
	if len(generations) != 1 || generations[0].GenerationID != first.GenerationID || generations[0].State != GenerationActive {
		t.Fatalf("failed refreshes created or changed generations: %+v", generations)
	}
}

func TestConcurrentSemanticBuildersChooseOneWinner(t *testing.T) {
	repository := openProjectionTestRepository(t)
	appendProjectionItems(t, repository, "p", "a", "b", "c", "d")
	var group sync.WaitGroup
	results := make(chan SemanticRefreshResult, 4)
	errs := make(chan error, 4)
	for range 4 {
		group.Go(func() {
			result, err := newTestBuilder(t, repository, newHashEmbedder()).Refresh(t.Context(), "p")
			if err != nil {
				errs <- err
				return
			}
			results <- result
		})
	}
	group.Wait()
	close(results)
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	winner := ""
	for result := range results {
		if winner == "" {
			winner = result.GenerationID
		} else if result.GenerationID != winner {
			t.Fatalf("builders disagree on the winner: %s vs %s", winner, result.GenerationID)
		}
	}
	var active int
	if err := repository.db.QueryRowContext(t.Context(), "SELECT COUNT(*) FROM context_embedding_generations WHERE project_id='p' AND state='active'").Scan(&active); err != nil {
		t.Fatal(err)
	}
	if active != 1 {
		t.Fatalf("active generations = %d, want 1", active)
	}
}
