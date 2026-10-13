package team

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"sync"
	"time"

	contextstore "github.com/kjelly/hufu/internal/context"
	"github.com/kjelly/hufu/internal/embedding"
	"github.com/kjelly/hufu/internal/semantic"
)

// Semantic runtime warm states (spec D1).
const (
	semanticStateCold    = "cold"
	semanticStateWarming = "warming"
	semanticStateReady   = "ready"
	semanticStateFailed  = "failed"
)

const (
	semanticWarmBackoffInitial = 5 * time.Second
	semanticWarmBackoffMax     = 5 * time.Minute
	semanticRefreshPoll        = 2 * time.Second
	semanticRefreshDebounce    = 250 * time.Millisecond
)

// semanticRuntimeRepository is the canonical store surface the runtime needs.
type semanticRuntimeRepository interface {
	contextstore.Repository
	contextstore.SemanticBuildRepository
	Revision(context.Context) (int64, error)
}

// SemanticRetrievalRuntime owns one coordinator's Ollama encoder, semantic
// index and refresher. Model loading happens in the background; a query never
// waits for it (spec D1). Queries share bounded embedding slots and their
// queueing counts toward the query deadline (spec D3). The runtime is safe
// for concurrent use and is shared by every worker of the coordinator.
type SemanticRetrievalRuntime struct {
	settings  semantic.Settings
	mode      contextstore.RetrievalMode
	repo      semanticRuntimeRepository
	projectID string
	client    *embedding.OllamaClient
	slot      chan struct{}
	pollEvery time.Duration

	ctx       context.Context
	cancel    context.CancelFunc
	wg        sync.WaitGroup
	warmCh    chan struct{}
	refreshCh chan struct{}

	mu      sync.Mutex
	state   string
	failure error
	ollama  *embedding.OllamaEmbedder
	index   *contextstore.SemanticIndex
	builder *contextstore.SemanticBuilder
	stats   SemanticRuntimeStatus
}

// SemanticRuntimeStatus is a content-free view for status output and tests.
type SemanticRuntimeStatus struct {
	Mode               string         `json:"mode"`
	State              string         `json:"state"`
	Model              string         `json:"model"`
	Endpoint           string         `json:"endpoint"`
	Failure            string         `json:"failure,omitempty"`
	ProfileFingerprint string         `json:"profile_fingerprint,omitempty"`
	ArtifactDigest     string         `json:"artifact_digest,omitempty"`
	QueryPolicy        string         `json:"query_policy,omitempty"`
	Queries            int            `json:"queries"`
	RoutedSemantic     int            `json:"routed_semantic,omitempty"`
	RoutedLexical      int            `json:"routed_lexical,omitempty"`
	SemanticResults    int            `json:"semantic_results"`
	TruncatedQueries   int            `json:"truncated_queries"`
	Fallbacks          map[string]int `json:"fallbacks,omitempty"`
	Warmups            int            `json:"warmups"`
	Refreshes          int            `json:"refreshes"`
	RefreshFailures    int            `json:"refresh_failures"`
	LastRefreshAt      time.Time      `json:"last_refresh_at,omitzero"`
	LastRefreshEmbeds  int            `json:"last_refresh_embedded"`
	GenerationID       string         `json:"generation_id,omitempty"`
}

// newSemanticRetrievalRuntime returns nil for mode off: off must send no
// request and start no goroutine (INV-04).
func newSemanticRetrievalRuntime(settings semantic.Settings, repo contextstore.Repository, projectID string, noNet bool) (*SemanticRetrievalRuntime, error) {
	return newSemanticRetrievalRuntimeWithPoll(settings, repo, projectID, noNet, semanticRefreshPoll)
}

// newSemanticRetrievalRuntimeWithPoll lets tests disable revision polling so
// only incomplete queries trigger a refresh.
func newSemanticRetrievalRuntimeWithPoll(settings semantic.Settings, repo contextstore.Repository, projectID string, noNet bool, pollEvery time.Duration) (*SemanticRetrievalRuntime, error) {
	if !settings.Enabled() {
		return nil, nil
	}
	if noNet && !settings.EndpointIsLoopback() {
		return nil, fmt.Errorf("semantic-retrieval endpoint %s is not loopback and --no-net is set", embedding.RedactEndpoint(settings.Endpoint))
	}
	store, ok := repo.(semanticRuntimeRepository)
	if !ok {
		return nil, errors.New("semantic retrieval requires the SQLite canonical context store")
	}
	if projectID == "" {
		return nil, errors.New("semantic retrieval requires a project scope")
	}
	client, err := embedding.NewOllamaClient(settings.Endpoint, nil)
	if err != nil {
		return nil, err
	}
	mode := contextstore.RetrievalShadow
	if settings.ChangesSelection() {
		mode = contextstore.RetrievalActive
	}
	ctx, cancel := context.WithCancel(context.Background())
	r := &SemanticRetrievalRuntime{
		settings: settings, mode: mode, repo: store, projectID: projectID, client: client,
		slot: make(chan struct{}, settings.MaxConcurrent), pollEvery: pollEvery,
		ctx: ctx, cancel: cancel, warmCh: make(chan struct{}, 1), refreshCh: make(chan struct{}, 1),
		state: semanticStateCold,
		stats: SemanticRuntimeStatus{Mode: string(settings.Mode), Model: settings.Model, Endpoint: embedding.RedactEndpoint(settings.Endpoint)},
	}
	r.wg.Add(2)
	go r.warmLoop()
	go r.refreshLoop()
	return r, nil
}

// Mode is the retrieval mode the runtime serves (shadow or active); auto
// serves active for the requests it routes to the semantic lane.
func (r *SemanticRetrievalRuntime) Mode() contextstore.RetrievalMode { return r.mode }

// Close stops background work; queries afterwards report warming.
func (r *SemanticRetrievalRuntime) Close() {
	if r == nil {
		return
	}
	r.cancel()
	r.wg.Wait()
}

// Status returns a copy of the content-free counters.
func (r *SemanticRetrievalRuntime) Status() SemanticRuntimeStatus {
	r.mu.Lock()
	defer r.mu.Unlock()
	status := r.stats
	status.State = r.state
	if r.failure != nil {
		status.Failure = string(contextstore.SemanticFallbackReasonForError(r.failureError()))
	}
	status.Fallbacks = maps.Clone(r.stats.Fallbacks)
	return status
}

// SearchVector implements contextstore.VectorSearcher. It never blocks on
// model loading, and only the caller's own cancellation is returned as a
// context error; the runtime's query deadline becomes ErrSemanticTimeout.
func (r *SemanticRetrievalRuntime) SearchVector(ctx context.Context, req contextstore.SearchRequest) ([]contextstore.SearchResult, error) {
	index, err := r.readyIndex()
	if err != nil {
		r.recordQuery(err)
		return nil, err
	}
	queryCtx, cancel := context.WithTimeout(ctx, r.settings.QueryTimeout)
	defer cancel()
	results, err := index.SearchVector(queryCtx, req)
	if err != nil {
		if parentErr := ctx.Err(); parentErr != nil {
			return nil, parentErr
		}
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			// The model may have been unloaded or reloaded by another
			// client; re-verify in the background (singleflight).
			r.requestRewarm()
			err = contextstore.ErrSemanticTimeout
		}
		r.recordQuery(err)
		return nil, err
	}
	r.recordQuery(nil)
	return results, nil
}

// SemanticTraceIdentity implements the trace identity provider.
func (r *SemanticRetrievalRuntime) SemanticTraceIdentity() (embedding.ModelIdentity, string, int64) {
	r.mu.Lock()
	index := r.index
	r.mu.Unlock()
	if index == nil {
		return embedding.ModelIdentity{}, "", 0
	}
	return index.SemanticTraceIdentity()
}

func (r *SemanticRetrievalRuntime) readyIndex() (*contextstore.SemanticIndex, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	switch {
	case r.state == semanticStateReady && r.index != nil:
		return r.index, nil
	case r.state == semanticStateFailed:
		return nil, r.failureError()
	default:
		return nil, contextstore.ErrSemanticWarming
	}
}

// failureError maps a warm failure onto a sentinel that never wraps a
// context error, so retrieval cannot mistake it for caller cancellation.
// Callers hold r.mu.
func (r *SemanticRetrievalRuntime) failureError() error {
	switch {
	case errors.Is(r.failure, embedding.ErrModelProfileMismatch):
		return embedding.ErrModelProfileMismatch
	case errors.Is(r.failure, embedding.ErrModelNotFound), errors.Is(r.failure, embedding.ErrOllamaUnavailable),
		errors.Is(r.failure, context.DeadlineExceeded):
		return embedding.ErrOllamaUnavailable
	default:
		return contextstore.ErrSemanticWarming
	}
}

func (r *SemanticRetrievalRuntime) recordQuery(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.stats.Queries++
	if err == nil {
		r.stats.SemanticResults++
		return
	}
	if r.stats.Fallbacks == nil {
		r.stats.Fallbacks = map[string]int{}
	}
	r.stats.Fallbacks[string(contextstore.SemanticFallbackReasonForError(err))]++
}

func (r *SemanticRetrievalRuntime) requestRewarm() {
	r.mu.Lock()
	if r.state == semanticStateReady {
		r.state = semanticStateWarming
	}
	r.mu.Unlock()
	select {
	case r.warmCh <- struct{}{}:
	default:
	}
}

func (r *SemanticRetrievalRuntime) nudgeRefresh() {
	select {
	case r.refreshCh <- struct{}{}:
	default:
	}
}

func (r *SemanticRetrievalRuntime) warmLoop() {
	defer r.wg.Done()
	backoff := semanticWarmBackoffInitial
	for {
		var retry <-chan time.Time
		if r.warm() {
			backoff = semanticWarmBackoffInitial
		} else {
			retry = time.After(backoff)
			backoff = nextSemanticBackoff(backoff)
		}
		select {
		case <-r.ctx.Done():
			return
		case <-r.warmCh:
		case <-retry:
		}
	}
}

// warm resolves and pins the profile, preloads the model with an empty input
// and verifies /api/ps. The index is rebuilt only when the profile changed.
func (r *SemanticRetrievalRuntime) warm() bool {
	r.mu.Lock()
	r.state = semanticStateWarming
	r.stats.Warmups++
	r.mu.Unlock()
	ctx, cancel := context.WithTimeout(r.ctx, r.settings.LoadTimeout)
	defer cancel()
	profile, query, err := embedding.ResolveProfile(ctx, r.client, embedding.ProfileSettings{
		Model: r.settings.Model, MaxInputTokens: r.settings.MaxInputTokens, QueryMaxTokens: r.settings.QueryMaxTokens,
	})
	var ollama *embedding.OllamaEmbedder
	if err == nil {
		ollama, err = embedding.NewOllamaEmbedder(r.client, profile, query, r.settings.KeepAlive)
	}
	if err == nil {
		err = ollama.Preload(ctx)
	}
	if err == nil {
		err = ollama.VerifyLoaded(ctx)
	}
	var index *contextstore.SemanticIndex
	var builder *contextstore.SemanticBuilder
	if err == nil {
		gated := &gatedEmbedder{runtime: r, inner: ollama}
		index, err = contextstore.NewSemanticIndexWithOptions(r.repo, r.repo, gated, contextstore.SemanticIndexOptions{
			InventoryPolicy: contextstore.InventoryPolicyRetrievable, OnIncomplete: r.nudgeRefresh,
		})
		if err == nil {
			builder, err = contextstore.NewSemanticBuilder(r.repo, gated, contextstore.SemanticBuilderOptions{
				InventoryPolicy: contextstore.InventoryPolicyRetrievable, BatchSize: r.settings.IndexBatchSize,
			})
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if err != nil {
		if r.ctx.Err() == nil {
			r.state, r.failure = semanticStateFailed, err
		}
		return false
	}
	if r.ollama == nil || r.ollama.Profile() != profile || r.ollama.QueryPolicy() != query {
		r.ollama, r.index, r.builder = ollama, index, builder
		r.stats.ProfileFingerprint = profile.Fingerprint()
		r.stats.ArtifactDigest = profile.ArtifactDigest
		r.stats.QueryPolicy = query.Fingerprint()
	}
	r.state, r.failure = semanticStateReady, nil
	r.nudgeRefresh()
	return true
}

func (r *SemanticRetrievalRuntime) readyBuilder() *contextstore.SemanticBuilder {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.state != semanticStateReady {
		return nil
	}
	return r.builder
}

// refreshLoop keeps the generation current. The global canonical revision
// only signals that something may have changed; the builder decides per
// item what to embed.
func (r *SemanticRetrievalRuntime) refreshLoop() {
	defer r.wg.Done()
	ticker := time.NewTicker(r.pollEvery)
	defer ticker.Stop()
	lastRevision := int64(-1)
	var retryAt time.Time
	backoff := semanticWarmBackoffInitial
	for {
		nudged := false
		select {
		case <-r.ctx.Done():
			return
		case <-r.refreshCh:
			nudged = true
		case <-ticker.C:
		}
		builder := r.readyBuilder()
		if builder == nil || time.Now().Before(retryAt) {
			continue
		}
		revision, err := r.repo.Revision(r.ctx)
		if err != nil || (!nudged && revision == lastRevision) {
			continue
		}
		if nudged {
			// Coalesce a burst of incomplete queries into one refresh.
			select {
			case <-r.ctx.Done():
				return
			case <-time.After(semanticRefreshDebounce):
			}
			select {
			case <-r.refreshCh:
			default:
			}
		}
		ctx, cancel := context.WithTimeout(r.ctx, r.settings.IndexTimeout)
		result, err := builder.Refresh(ctx, r.projectID)
		cancel()
		r.mu.Lock()
		if err != nil {
			r.stats.RefreshFailures++
			r.mu.Unlock()
			retryAt = time.Now().Add(backoff)
			backoff = nextSemanticBackoff(backoff)
			continue
		}
		r.stats.Refreshes++
		r.stats.LastRefreshAt = time.Now().UTC()
		r.stats.LastRefreshEmbeds = result.Embedded
		r.stats.GenerationID = result.GenerationID
		r.mu.Unlock()
		lastRevision, retryAt, backoff = revision, time.Time{}, semanticWarmBackoffInitial
	}
}

// nextSemanticBackoff doubles d up to the maximum. (The package's tests
// define their own min, so the builtin is avoided here.)
func nextSemanticBackoff(d time.Duration) time.Duration {
	if d*2 > semanticWarmBackoffMax {
		return semanticWarmBackoffMax
	}
	return d * 2
}

// RefreshNow runs one synchronous refresh once the encoder is ready; it is
// used by tests and operator commands that need a deterministic index.
func (r *SemanticRetrievalRuntime) RefreshNow(ctx context.Context) (contextstore.SemanticRefreshResult, error) {
	builder := r.readyBuilder()
	if builder == nil {
		return contextstore.SemanticRefreshResult{}, contextstore.ErrSemanticWarming
	}
	return builder.Refresh(ctx, r.projectID)
}

// WaitReady blocks until the encoder is ready or ctx ends (tests, CLI).
func (r *SemanticRetrievalRuntime) WaitReady(ctx context.Context) error {
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		r.mu.Lock()
		state, failure := r.state, r.failure
		r.mu.Unlock()
		if state == semanticStateReady {
			return nil
		}
		select {
		case <-ctx.Done():
			if failure != nil {
				return fmt.Errorf("semantic encoder not ready: %w", failure)
			}
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// gatedEmbedder bounds concurrent embedding requests. A query waits for a
// slot only until its deadline (then ErrSemanticBusy); document batches from
// the refresher wait under their own index deadline.
type gatedEmbedder struct {
	runtime *SemanticRetrievalRuntime
	inner   *embedding.OllamaEmbedder
}

func (g *gatedEmbedder) Identity() embedding.ModelIdentity { return g.inner.Identity() }

func (g *gatedEmbedder) acquire(ctx context.Context) bool {
	select {
	case g.runtime.slot <- struct{}{}:
		return true
	case <-ctx.Done():
		return false
	}
}

func (g *gatedEmbedder) release() { <-g.runtime.slot }

func (g *gatedEmbedder) EmbedQuery(ctx context.Context, text string) ([]float32, error) {
	probe := contextstore.SemanticQueryProbeFrom(ctx)
	queued := time.Now()
	if !g.acquire(ctx) {
		probe.RecordQuery(0, false, time.Since(queued), 0)
		return nil, contextstore.ErrSemanticBusy
	}
	defer g.release()
	queueWait := time.Since(queued)
	started := time.Now()
	result, err := g.inner.EmbedQueryDetailed(ctx, text)
	tokens := result.PromptTokens
	if tokens == 0 {
		tokens = result.Shaped.EstimatedTokens
	}
	probe.RecordQuery(tokens, result.Shaped.Truncated, queueWait, time.Since(started))
	if err == nil && result.Shaped.Truncated {
		g.runtime.mu.Lock()
		g.runtime.stats.TruncatedQueries++
		g.runtime.mu.Unlock()
	}
	return result.Vector, err
}

func (g *gatedEmbedder) EmbedDocument(ctx context.Context, text string) ([]float32, error) {
	vectors, err := g.EmbedDocuments(ctx, []string{text})
	if err != nil {
		return nil, err
	}
	return vectors[0], nil
}

func (g *gatedEmbedder) EmbedDocuments(ctx context.Context, texts []string) ([][]float32, error) {
	if !g.acquire(ctx) {
		return nil, ctx.Err()
	}
	defer g.release()
	return g.inner.EmbedDocuments(ctx, texts)
}

var (
	_ contextstore.VectorSearcher = (*SemanticRetrievalRuntime)(nil)
	_ embedding.Embedder          = (*gatedEmbedder)(nil)
)
