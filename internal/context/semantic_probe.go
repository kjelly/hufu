package context

import (
	"context"
	"sync"
	"time"
)

// SemanticQueryProbe collects the per-query details only the vector searcher
// knows (candidate coverage, query shaping, queue wait, embedding time) for
// the content-free trace. HybridRetrieveWithOptions installs one per traced
// query; every method is safe on a nil probe.
type SemanticQueryProbe struct {
	mu        sync.Mutex
	covered   int
	uncovered int
	tokens    int
	truncated bool
	queueWait time.Duration
	embed     time.Duration
}

type semanticQueryProbeKey struct{}

// WithSemanticQueryProbe returns ctx carrying probe.
func WithSemanticQueryProbe(ctx context.Context, probe *SemanticQueryProbe) context.Context {
	return context.WithValue(ctx, semanticQueryProbeKey{}, probe)
}

// SemanticQueryProbeFrom returns the probe installed in ctx, or nil.
func SemanticQueryProbeFrom(ctx context.Context) *SemanticQueryProbe {
	probe, _ := ctx.Value(semanticQueryProbeKey{}).(*SemanticQueryProbe)
	return probe
}

// RecordCoverage records how many authorized candidates had a fresh vector.
func (p *SemanticQueryProbe) RecordCoverage(covered, uncovered int) {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.covered, p.uncovered = covered, uncovered
}

// RecordQuery records the embedded query's token count, whether it was cut
// to the query budget, the wait for an encoder slot, and the embedding time.
func (p *SemanticQueryProbe) RecordQuery(tokens int, truncated bool, queueWait, embed time.Duration) {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.tokens, p.truncated, p.queueWait, p.embed = tokens, truncated, queueWait, embed
}

func (p *SemanticQueryProbe) apply(trace *SemanticRetrievalTrace) {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	trace.CoveredCandidateCount, trace.UncoveredCandidateCount = p.covered, p.uncovered
	trace.QueryTokens, trace.QueryTruncated = p.tokens, p.truncated
	trace.QueueWaitMillis, trace.EmbedMillis = p.queueWait.Milliseconds(), p.embed.Milliseconds()
}
