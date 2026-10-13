package team

import (
	"context"
	"fmt"

	contextstore "github.com/kjelly/hufu/internal/context"
	"github.com/kjelly/hufu/internal/semantic"
)

// initSemanticRetrieval resolves the effective semantic-retrieval settings
// (hufu.yaml under the team block) and, unless the mode is off, starts the
// shared runtime and wires it into worker memory. Mode off keeps the legacy
// worker memory service exactly.
func (c *Coordinator) initSemanticRetrieval(repo contextstore.Repository, noNet bool) error {
	settings, err := semantic.Merge(c.session.GlobalSemanticRetrieval, c.session.Config.SemanticRetrieval).Resolve()
	if err != nil {
		return fmt.Errorf("resolve semantic-retrieval: %w", err)
	}
	c.semanticSettings = settings
	runtime, err := newSemanticRetrievalRuntime(settings, repo, c.contextScopeProjectID(), noNet)
	if err != nil {
		return fmt.Errorf("start semantic retrieval: %w", err)
	}
	if runtime == nil {
		c.workerMemorySvc = NewWorkerMemoryService(repo, nil)
		return nil
	}
	hasher, sink, err := c.semanticTraceOptions()
	if err != nil {
		runtime.Close()
		return fmt.Errorf("open semantic retrieval traces: %w", err)
	}
	c.semanticRuntime, c.ownsSemanticRuntime = runtime, true
	c.semanticTraceHasher, c.semanticTraceSink = hasher, sink
	c.workerMemorySvc = NewWorkerMemoryServiceWithOptions(repo, WorkerMemoryOptions{
		Semantic: runtime, Mode: runtime.Mode(), TraceHasher: hasher, TraceSink: sink,
	})
	return nil
}

// persistentSemanticLane is the shared persistent ranking's semantic lane,
// or nil when semantic retrieval is off.
func (c *Coordinator) persistentSemanticLane() *persistentSemanticLane {
	if c == nil || c.semanticRuntime == nil {
		return nil
	}
	return &persistentSemanticLane{
		Vector: c.semanticRuntime, Mode: c.semanticRuntime.Mode(),
		TraceHasher: c.semanticTraceHasher, TraceSink: c.semanticTraceSink,
	}
}

// SemanticRetrievalStatus reports the runtime counters, or ok=false when
// semantic retrieval is off.
func (c *Coordinator) SemanticRetrievalStatus() (SemanticRuntimeStatus, bool) {
	if c == nil || c.semanticRuntime == nil {
		return SemanticRuntimeStatus{}, false
	}
	return c.semanticRuntime.Status(), true
}

// SemanticRetrievalRuntime exposes the runtime to tests and operator tools.
func (c *Coordinator) SemanticRetrievalRuntime() *SemanticRetrievalRuntime {
	if c == nil {
		return nil
	}
	return c.semanticRuntime
}

// sharedPersistentRanking is the result of ranking shared persistent memory
// for one request.
type sharedPersistentRanking struct {
	Selected    []contextstore.ContextItem
	Scores      map[string]MemoryScoreParts
	FinalScores map[string]float64
	Aggregates  map[string]*contextstore.ExperienceAggregate
	Semantic    *SemanticRetrievalIdentity
}

// rankSharedPersistentMemoryForRequest ranks with the semantic lane when one
// is configured. semanticQuery is the text the lane embeds (the request goal).
func (c *Coordinator) rankSharedPersistentMemoryForRequest(ctx context.Context, query, semanticQuery string, base []contextstore.ContextItem, allowed map[string]bool) (sharedPersistentRanking, error) {
	selected, scores, finalScores, aggregates, semanticIdentity, err := c.rankSharedPersistentMemoryWithLane(ctx, query, semanticQuery, base, allowed, c.persistentSemanticLane())
	return sharedPersistentRanking{Selected: selected, Scores: scores, FinalScores: finalScores, Aggregates: aggregates, Semantic: semanticIdentity}, err
}
