package context

import (
	"context"
	"time"
)

// QueryWork counts retrieval API calls. VectorCalls is not an embedding-call
// count: a vector backend can issue more than one embedding request internally.
type QueryWork struct {
	ExactCalls   int
	LexicalCalls int
	VectorCalls  int
	// CandidateRows counts source rows before filtering/fusion/limits. The
	// same item returned by multiple sources contributes once per source.
	CandidateRows int
	Elapsed       time.Duration
}

type queryWorkObserverKey struct{}

func WithQueryWorkObserver(ctx context.Context, observer func(context.Context, QueryWork)) context.Context {
	return context.WithValue(ctx, queryWorkObserverKey{}, observer)
}

func QueryWorkObserverEnabled(ctx context.Context) bool {
	observer, ok := ctx.Value(queryWorkObserverKey{}).(func(context.Context, QueryWork))
	return ok && observer != nil
}

func observeQueryWork(ctx context.Context, work QueryWork) {
	if observer, ok := ctx.Value(queryWorkObserverKey{}).(func(context.Context, QueryWork)); ok && observer != nil {
		observer(ctx, work)
	}
}
