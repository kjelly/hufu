package context

import (
	"container/heap"
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
	"sort"
	"sync"

	"github.com/kjelly/hufu/internal/embedding"
)

var (
	ErrSemanticSnapshotCorrupt = errors.New("semantic snapshot is corrupt")
	ErrSemanticIndexStale      = errors.New("semantic index has no active generation for the canonical revision")
)

type RetrievalCandidateIterator interface {
	IterateRetrievable(context.Context, SearchRequest, func(ContextItem) error) error
}

type SemanticIndex struct {
	repo       Repository
	projection SemanticProjectionRepository
	embedder   embedding.Embedder
	iterator   RetrievalCandidateIterator
	inventory  CanonicalEmbeddingInventory

	mu             sync.RWMutex
	refreshMu      sync.Mutex
	model          embedding.ModelIdentity
	generationID   string
	sourceRevision int64
	snapshot       flatVectors
}

type flatVectors struct {
	IDs        []string
	Data       []float32
	Dimensions int
}

func NewSemanticIndex(
	repo Repository,
	projection SemanticProjectionRepository,
	embedder embedding.Embedder,
) (*SemanticIndex, error) {
	if repo == nil || projection == nil || embedder == nil {
		return nil, errors.New("semantic index dependencies are required")
	}
	iterator, ok := repo.(RetrievalCandidateIterator)
	if !ok {
		return nil, errors.New("semantic index repository does not provide canonical retrieval iteration")
	}
	inventory, ok := repo.(CanonicalEmbeddingInventory)
	if !ok {
		return nil, errors.New("semantic index repository does not provide canonical embedding inventory")
	}
	model := embedder.Identity()
	if err := validateModelIdentity(model); err != nil {
		return nil, err
	}
	return &SemanticIndex{
		repo: repo, projection: projection, embedder: embedder,
		iterator: iterator, inventory: inventory, model: model,
	}, nil
}

func (s *SemanticIndex) SearchVector(ctx context.Context, req SearchRequest) ([]SearchResult, error) {
	if req.Scope.ProjectID == "" {
		return nil, errors.New("project scope is required")
	}
	if err := s.ensureSnapshot(ctx, req.Scope.ProjectID); err != nil {
		return nil, err
	}
	query, err := s.embedder.EmbedQuery(ctx, req.Query)
	if err != nil {
		return nil, err
	}

	s.mu.RLock()
	snapshot := s.snapshot
	sourceRevision := s.sourceRevision
	generationID := s.generationID
	s.mu.RUnlock()
	if err = validateSemanticQueryVector(query, snapshot.Dimensions); err != nil {
		return nil, err
	}

	limit := req.Limit
	if limit <= 0 {
		limit = 20
	}
	best := make(resultMinHeap, 0, min(limit, len(snapshot.IDs)))
	heap.Init(&best)
	err = s.iterator.IterateRetrievable(ctx, req, func(item ContextItem) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		index, found := slices.BinarySearch(snapshot.IDs, item.ID)
		if !found {
			return fmt.Errorf("%w: authorized item %s is absent", ErrSemanticSnapshotCorrupt, item.ID)
		}
		start := index * snapshot.Dimensions
		score := dotProduct(query, snapshot.Data[start:start+snapshot.Dimensions])
		candidate := SearchResult{Item: item, Score: score}
		if len(best) < limit {
			heap.Push(&best, candidate)
			return nil
		}
		if searchResultBetter(candidate, best[0]) {
			best[0] = candidate
			heap.Fix(&best, 0)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	currentRevision, err := s.repo.Revision(ctx)
	if err != nil {
		return nil, err
	}
	if currentRevision != sourceRevision {
		return nil, ErrSemanticIndexStale
	}
	active, err := s.projection.ActiveGeneration(ctx, req.Scope.ProjectID, s.model)
	if err != nil {
		return nil, err
	}
	if active.GenerationID != generationID {
		return nil, ErrSemanticIndexStale
	}

	results := make([]SearchResult, len(best))
	copy(results, best)
	sort.Slice(results, func(i, j int) bool {
		return searchResultBetter(results[i], results[j])
	})
	return results, nil
}

func (s *SemanticIndex) ensureSnapshot(ctx context.Context, projectID string) error {
	revision, active, err := s.currentProjectionIdentity(ctx, projectID)
	if err != nil {
		return err
	}
	if s.snapshotMatches(active.GenerationID, revision) {
		return nil
	}

	s.refreshMu.Lock()
	defer s.refreshMu.Unlock()
	for range 2 {
		revision, active, err = s.currentProjectionIdentity(ctx, projectID)
		if err != nil {
			return err
		}
		if s.snapshotMatches(active.GenerationID, revision) {
			return nil
		}
		if active.SourceRevision != revision {
			return ErrSemanticIndexStale
		}

		inventory, loadErr := s.inventory.LoadEmbeddingInventory(ctx, projectID)
		if loadErr != nil {
			return loadErr
		}
		rows, loadErr := s.projection.LoadActiveGeneration(ctx, active.GenerationID)
		if loadErr != nil {
			return fmt.Errorf("%w: %v", ErrSemanticSnapshotCorrupt, loadErr)
		}
		flat, loadErr := buildFlatVectors(active, inventory, rows)
		if loadErr != nil {
			return loadErr
		}

		finalRevision, finalActive, loadErr := s.currentProjectionIdentity(ctx, projectID)
		if loadErr != nil {
			return loadErr
		}
		if finalRevision != revision || finalActive.GenerationID != active.GenerationID {
			continue
		}
		s.mu.Lock()
		s.generationID = active.GenerationID
		s.sourceRevision = active.SourceRevision
		s.snapshot = flat
		s.mu.Unlock()
		return nil
	}
	return ErrSemanticIndexStale
}

func (s *SemanticIndex) currentProjectionIdentity(ctx context.Context, projectID string) (int64, Generation, error) {
	revision, err := s.repo.Revision(ctx)
	if err != nil {
		return 0, Generation{}, err
	}
	active, err := s.projection.ActiveGeneration(ctx, projectID, s.model)
	if err != nil {
		if errors.Is(err, ErrNoActiveEmbeddingGeneration) {
			return 0, Generation{}, ErrSemanticIndexStale
		}
		return 0, Generation{}, err
	}
	return revision, active, nil
}

func (s *SemanticIndex) snapshotMatches(generationID string, revision int64) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.generationID != "" && s.generationID == generationID && s.sourceRevision == revision
}

func buildFlatVectors(active Generation, inventory EmbeddingInventory, rows []EmbeddingRow) (flatVectors, error) {
	if inventory.ProjectID != active.ProjectID || inventory.Revision != active.SourceRevision ||
		inventory.Digest != active.ContentDigest || len(inventory.Items) != active.ExpectedCount ||
		len(rows) != active.ExpectedCount {
		return flatVectors{}, fmt.Errorf("%w: active generation inventory mismatch", ErrSemanticSnapshotCorrupt)
	}
	flat := flatVectors{
		IDs: make([]string, len(rows)), Data: make([]float32, len(rows)*active.Model.Dimensions),
		Dimensions: active.Model.Dimensions,
	}
	for i, row := range rows {
		if i >= len(inventory.Items) || row.ItemID != inventory.Items[i].ItemID ||
			row.ContentHash != inventory.Items[i].ContentHash || row.Dimensions != flat.Dimensions ||
			len(row.Vector) != flat.Dimensions {
			return flatVectors{}, fmt.Errorf("%w: embedding row %d does not match canonical inventory", ErrSemanticSnapshotCorrupt, i)
		}
		if err := validateNormalizedVector(row.Vector); err != nil {
			return flatVectors{}, fmt.Errorf("%w: item %s: %v", ErrSemanticSnapshotCorrupt, row.ItemID, err)
		}
		flat.IDs[i] = row.ItemID
		copy(flat.Data[i*flat.Dimensions:(i+1)*flat.Dimensions], row.Vector)
	}
	if !slices.IsSorted(flat.IDs) {
		return flatVectors{}, fmt.Errorf("%w: embedding rows are not bytewise sorted", ErrSemanticSnapshotCorrupt)
	}
	return flat, nil
}

func validateSemanticQueryVector(vector []float32, dimensions int) error {
	if len(vector) != dimensions {
		return fmt.Errorf("%w: query dimensions are %d, want %d", ErrSemanticSnapshotCorrupt, len(vector), dimensions)
	}
	if err := validateNormalizedVector(vector); err != nil {
		return fmt.Errorf("%w: query vector: %v", ErrSemanticSnapshotCorrupt, err)
	}
	return nil
}

func validateNormalizedVector(vector []float32) error {
	var squared float64
	for _, value := range vector {
		converted := float64(value)
		if math.IsNaN(converted) || math.IsInf(converted, 0) {
			return errors.New("vector contains non-finite value")
		}
		squared += converted * converted
	}
	if math.Abs(squared-1) > 1e-4 {
		return fmt.Errorf("vector squared norm is %f, want 1", squared)
	}
	return nil
}

func dotProduct(left, right []float32) float64 {
	var score float64
	for i, value := range left {
		score += float64(value) * float64(right[i])
	}
	return score
}

func searchResultBetter(left, right SearchResult) bool {
	if left.Score != right.Score {
		return left.Score > right.Score
	}
	return left.Item.ID < right.Item.ID
}

type resultMinHeap []SearchResult

func (h resultMinHeap) Len() int { return len(h) }

func (h resultMinHeap) Less(i, j int) bool {
	if h[i].Score != h[j].Score {
		return h[i].Score < h[j].Score
	}
	return h[i].Item.ID > h[j].Item.ID
}

func (h resultMinHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i] }

func (h *resultMinHeap) Push(value any) { *h = append(*h, value.(SearchResult)) }

func (h *resultMinHeap) Pop() any {
	old := *h
	last := len(old) - 1
	result := old[last]
	*h = old[:last]
	return result
}

func (r *SQLiteRepository) IterateRetrievable(ctx context.Context, req SearchRequest, visit func(ContextItem) error) error {
	if visit == nil {
		return errors.New("retrieval iteration requires a visitor")
	}
	req.AllowedItemIDs = normalizeAllowedItemIDs(req.AllowedItemIDs)
	var allowed map[string]struct{}
	if req.AllowedItemIDs != nil {
		allowed = make(map[string]struct{}, len(req.AllowedItemIDs))
		for _, id := range req.AllowedItemIDs {
			allowed[id] = struct{}{}
		}
	}
	return r.Iterate(ctx, RepositoryQuery{
		Scope: req.Scope, Visibility: req.Visibility, Kinds: req.Kinds,
		IncludeCandidates: req.IncludeCandidates, MinConfidence: req.MinConfidence,
	}, func(item ContextItem) error {
		if allowed != nil {
			if _, ok := allowed[item.ID]; !ok {
				return nil
			}
		}
		return visit(item)
	})
}

var _ VectorSearcher = (*SemanticIndex)(nil)
var _ RetrievalCandidateIterator = (*SQLiteRepository)(nil)
