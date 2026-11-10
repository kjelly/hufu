package context

import (
	"container/heap"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"slices"
	"sort"
	"strings"
	"sync"

	"github.com/kjelly/hufu/internal/embedding"
)

var (
	ErrSemanticSnapshotCorrupt   = errors.New("semantic snapshot is corrupt")
	ErrSemanticIndexStale        = errors.New("semantic index has no active generation for the canonical revision")
	ErrSemanticProjectionRefresh = errors.New("semantic projection refresh failed")
	ErrSemanticInvalidVector     = errors.New("semantic query vector is invalid")
	ErrSemanticCanonicalRepo     = errors.New("canonical repository failed during semantic retrieval")
	// ErrSemanticProjectionIncomplete means at least one authorized
	// candidate has no vector for its current content hash. The request
	// falls back to lexical retrieval rather than ranking new memories
	// lower only because they are not embedded yet.
	ErrSemanticProjectionIncomplete = errors.New("semantic projection does not cover every authorized candidate")
	// ErrVectorItemNotIndexed reports that an item has no stored vector for
	// its current content.
	ErrVectorItemNotIndexed = errors.New("context item is not in the vector index")
)

type RetrievalCandidateIterator interface {
	IterateRetrievable(context.Context, SearchRequest, func(ContextItem) error) error
}

// SemanticIndexOptions configure a SemanticIndex.
type SemanticIndexOptions struct {
	// InventoryPolicy selects the generations the index reads; empty means
	// InventoryPolicyProjectAll.
	InventoryPolicy string
	// OnIncomplete is called (without locks held) when a request finds an
	// uncovered candidate, so a refresher can catch up.
	OnIncomplete func()
}

// SemanticIndex scores authorized canonical candidates against the active
// generation. Freshness is per item: a candidate is covered when the
// generation holds a vector or an exclusion for its current content hash.
// Unrelated canonical writes therefore never invalidate the index.
type SemanticIndex struct {
	repo         Repository
	projection   SemanticProjectionRepository
	embedder     embedding.Embedder
	iterator     RetrievalCandidateIterator
	model        embedding.ModelIdentity
	policy       string
	onIncomplete func()

	mu        sync.RWMutex
	refreshMu sync.Mutex
	snapshot  *semanticSnapshot
}

// semanticSnapshot is immutable once published.
type semanticSnapshot struct {
	projectID      string
	generationID   string
	sourceRevision int64
	ids            []string
	hashes         []string
	data           []float32
	dimensions     int
	excluded       map[string]string
}

func NewSemanticIndex(repo Repository, projection SemanticProjectionRepository, embedder embedding.Embedder) (*SemanticIndex, error) {
	return NewSemanticIndexWithOptions(repo, projection, embedder, SemanticIndexOptions{})
}

func NewSemanticIndexWithOptions(
	repo Repository,
	projection SemanticProjectionRepository,
	embedder embedding.Embedder,
	options SemanticIndexOptions,
) (*SemanticIndex, error) {
	if repo == nil || projection == nil || embedder == nil {
		return nil, errors.New("semantic index dependencies are required")
	}
	iterator, ok := repo.(RetrievalCandidateIterator)
	if !ok {
		return nil, errors.New("semantic index repository does not provide canonical retrieval iteration")
	}
	model := embedder.Identity()
	model.Scheme = model.EffectiveScheme()
	if err := validateModelIdentity(model); err != nil {
		return nil, err
	}
	policy := options.InventoryPolicy
	if policy == "" {
		policy = InventoryPolicyProjectAll
	}
	if !ValidInventoryPolicy(policy) {
		return nil, fmt.Errorf("unknown embedding inventory policy %q", policy)
	}
	return &SemanticIndex{
		repo: repo, projection: projection, embedder: embedder, iterator: iterator,
		model: model, policy: policy, onIncomplete: options.OnIncomplete,
	}, nil
}

type coveredCandidate struct {
	item  ContextItem
	index int
}

func (s *SemanticIndex) SearchVector(ctx context.Context, req SearchRequest) ([]SearchResult, error) {
	if err := validateAllowedItemIDs(req.AllowedItemIDs); err != nil {
		return nil, err
	}
	if req.Scope.ProjectID == "" {
		return nil, errors.New("project scope is required")
	}
	snapshot, err := s.ensureSnapshot(ctx, req.Scope.ProjectID)
	if err != nil {
		return nil, err
	}
	// Coverage is checked before embedding the query so an incomplete
	// projection costs no model call.
	covered, err := s.coveredCandidates(ctx, req, snapshot, "")
	if err != nil {
		return nil, err
	}
	if len(covered) == 0 {
		return []SearchResult{}, nil
	}
	query, err := s.embedder.EmbedQuery(ctx, req.Query)
	if err != nil {
		return nil, err
	}
	if err = validateSemanticQueryVector(query, snapshot.dimensions); err != nil {
		return nil, err
	}
	return topSemanticResults(covered, snapshot, query, req.Limit), nil
}

// SearchSimilarTo ranks the authorized candidates of req against the stored
// vector of itemID, so it makes no embedding request. The source item is
// excluded. An item without a vector for its current content hash is
// ErrVectorItemNotIndexed.
func (s *SemanticIndex) SearchSimilarTo(ctx context.Context, itemID string, req SearchRequest) ([]SearchResult, error) {
	if err := validateAllowedItemIDs(req.AllowedItemIDs); err != nil {
		return nil, err
	}
	if req.Scope.ProjectID == "" {
		return nil, errors.New("project scope is required")
	}
	snapshot, err := s.ensureSnapshot(ctx, req.Scope.ProjectID)
	if err != nil {
		return nil, err
	}
	source, err := s.repo.Get(ctx, itemID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: %s", ErrVectorItemNotIndexed, itemID)
	}
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, err
		}
		return nil, fmt.Errorf("%w: %w", ErrSemanticCanonicalRepo, err)
	}
	index, found := slices.BinarySearch(snapshot.ids, itemID)
	if !found || snapshot.hashes[index] != strings.ToLower(source.ContentHash) {
		return nil, fmt.Errorf("%w: %s", ErrVectorItemNotIndexed, itemID)
	}
	covered, err := s.coveredCandidates(ctx, req, snapshot, itemID)
	if err != nil {
		return nil, err
	}
	start := index * snapshot.dimensions
	return topSemanticResults(covered, snapshot, snapshot.data[start:start+snapshot.dimensions], req.Limit), nil
}

// coveredCandidates lists the authorized candidates of req that have a
// vector for their current content hash, skipping exclude. Any other
// uncovered candidate makes the request ErrSemanticProjectionIncomplete.
func (s *SemanticIndex) coveredCandidates(ctx context.Context, req SearchRequest, snapshot *semanticSnapshot, exclude string) ([]coveredCandidate, error) {
	var covered []coveredCandidate
	uncovered := 0
	err := s.iterator.IterateRetrievable(ctx, req, func(item ContextItem) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if item.ID == exclude {
			return nil
		}
		if !inventoryPolicyCovers(s.policy, item) {
			// Rows outside the policy (for example a rejected candidate a
			// candidate-inclusive request can see) are not vector-scored;
			// they never count as missing coverage.
			return nil
		}
		hash := strings.ToLower(item.ContentHash)
		if index, found := slices.BinarySearch(snapshot.ids, item.ID); found && snapshot.hashes[index] == hash {
			covered = append(covered, coveredCandidate{item: item, index: index})
			return nil
		}
		if excludedHash, found := snapshot.excluded[item.ID]; found && excludedHash == hash {
			return nil
		}
		uncovered++
		return nil
	})
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, err
		}
		return nil, fmt.Errorf("%w: %w", ErrSemanticCanonicalRepo, err)
	}
	SemanticQueryProbeFrom(ctx).RecordCoverage(len(covered), uncovered)
	if uncovered > 0 {
		if s.onIncomplete != nil {
			s.onIncomplete()
		}
		return nil, fmt.Errorf("%w: %d of %d candidates lack vectors", ErrSemanticProjectionIncomplete, uncovered, uncovered+len(covered))
	}
	return covered, nil
}

// topSemanticResults scores covered candidates against query and returns
// the best limit (default 20) in deterministic order.
func topSemanticResults(covered []coveredCandidate, snapshot *semanticSnapshot, query []float32, limit int) []SearchResult {
	if limit <= 0 {
		limit = 20
	}
	best := make(resultMinHeap, 0, min(limit, len(covered)))
	heap.Init(&best)
	for _, candidate := range covered {
		start := candidate.index * snapshot.dimensions
		result := SearchResult{Item: candidate.item, Score: dotProduct(query, snapshot.data[start:start+snapshot.dimensions])}
		if len(best) < limit {
			heap.Push(&best, result)
			continue
		}
		if searchResultBetter(result, best[0]) {
			best[0] = result
			heap.Fix(&best, 0)
		}
	}
	results := make([]SearchResult, len(best))
	copy(results, best)
	sort.Slice(results, func(i, j int) bool {
		return searchResultBetter(results[i], results[j])
	})
	return results
}

// ensureSnapshot returns the snapshot of the project's active generation,
// reloading it only when the active generation changed.
func (s *SemanticIndex) ensureSnapshot(ctx context.Context, projectID string) (*semanticSnapshot, error) {
	active, err := s.activeGeneration(ctx, projectID)
	if err != nil {
		return nil, err
	}
	if snapshot := s.currentSnapshot(projectID, active.GenerationID); snapshot != nil {
		return snapshot, nil
	}
	s.refreshMu.Lock()
	defer s.refreshMu.Unlock()
	if snapshot := s.currentSnapshot(projectID, active.GenerationID); snapshot != nil {
		return snapshot, nil
	}
	for attempt := 0; ; attempt++ {
		rows, err := s.projection.LoadActiveGeneration(ctx, active.GenerationID)
		if err == nil {
			var exclusions []EmbeddingExclusion
			if exclusions, err = s.projection.LoadGenerationExclusions(ctx, active.GenerationID); err != nil {
				return nil, fmt.Errorf("%w: %w", ErrSemanticProjectionRefresh, err)
			}
			snapshot, buildErr := buildSemanticSnapshot(active, rows, exclusions)
			if buildErr != nil {
				return nil, buildErr
			}
			s.mu.Lock()
			s.snapshot = snapshot
			s.mu.Unlock()
			return snapshot, nil
		}
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, err
		}
		// A concurrent refresh may have superseded the generation between
		// the two reads; retry once against the new winner before treating
		// an unreadable active generation as corrupt.
		latest, latestErr := s.activeGeneration(ctx, projectID)
		if attempt > 0 || latestErr != nil || latest.GenerationID == active.GenerationID {
			return nil, fmt.Errorf("%w: %w", ErrSemanticSnapshotCorrupt, err)
		}
		active = latest
	}
}

func (s *SemanticIndex) activeGeneration(ctx context.Context, projectID string) (Generation, error) {
	active, err := s.projection.ActiveGeneration(ctx, projectID, s.model, s.policy)
	if err == nil {
		return active, nil
	}
	if errors.Is(err, ErrNoActiveEmbeddingGeneration) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return Generation{}, err
	}
	return Generation{}, fmt.Errorf("%w: %w", ErrSemanticProjectionRefresh, err)
}

func (s *SemanticIndex) currentSnapshot(projectID, generationID string) *semanticSnapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.snapshot != nil && s.snapshot.projectID == projectID && s.snapshot.generationID == generationID {
		return s.snapshot
	}
	return nil
}

func buildSemanticSnapshot(active Generation, rows []EmbeddingRow, exclusions []EmbeddingExclusion) (*semanticSnapshot, error) {
	if len(rows) != active.RowCount || len(rows)+len(exclusions) != active.ExpectedCount {
		return nil, fmt.Errorf("%w: active generation entry count mismatch", ErrSemanticSnapshotCorrupt)
	}
	dims := active.Model.Dimensions
	snapshot := &semanticSnapshot{
		projectID: active.ProjectID, generationID: active.GenerationID, sourceRevision: active.SourceRevision,
		ids: make([]string, len(rows)), hashes: make([]string, len(rows)),
		data: make([]float32, len(rows)*dims), dimensions: dims, excluded: make(map[string]string, len(exclusions)),
	}
	for i, row := range rows {
		if row.Dimensions != dims || len(row.Vector) != dims {
			return nil, fmt.Errorf("%w: embedding row %d has wrong dimensions", ErrSemanticSnapshotCorrupt, i)
		}
		if err := validateNormalizedVector(row.Vector); err != nil {
			return nil, fmt.Errorf("%w: item %s: %v", ErrSemanticSnapshotCorrupt, row.ItemID, err)
		}
		snapshot.ids[i] = row.ItemID
		snapshot.hashes[i] = strings.ToLower(row.ContentHash)
		copy(snapshot.data[i*dims:(i+1)*dims], row.Vector)
	}
	if !slices.IsSorted(snapshot.ids) {
		return nil, fmt.Errorf("%w: embedding rows are not bytewise sorted", ErrSemanticSnapshotCorrupt)
	}
	for _, exclusion := range exclusions {
		snapshot.excluded[exclusion.ItemID] = strings.ToLower(exclusion.ContentHash)
	}
	return snapshot, nil
}

func validateSemanticQueryVector(vector []float32, dimensions int) error {
	if len(vector) != dimensions {
		return fmt.Errorf("%w: query dimensions are %d, want %d", ErrSemanticInvalidVector, len(vector), dimensions)
	}
	if err := validateNormalizedVector(vector); err != nil {
		return fmt.Errorf("%w: query vector: %v", ErrSemanticInvalidVector, err)
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
	return r.Iterate(ctx, RepositoryQuery{
		Scope: req.Scope, Visibility: req.Visibility, Kinds: req.Kinds,
		IncludeCandidates: req.IncludeCandidates, MinConfidence: req.MinConfidence,
		IDs: req.AllowedItemIDs,
	}, visit)
}

// SemanticTraceIdentity reports the encoder and the generation of the last
// loaded snapshot.
func (s *SemanticIndex) SemanticTraceIdentity() (embedding.ModelIdentity, string, int64) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.snapshot == nil {
		return s.model, "", 0
	}
	return s.model, s.snapshot.generationID, s.snapshot.sourceRevision
}

var _ VectorSearcher = (*SemanticIndex)(nil)
var _ RetrievalCandidateIterator = (*SQLiteRepository)(nil)
