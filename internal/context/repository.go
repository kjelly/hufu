package context

import (
	"context"
	"time"
)

type Repository interface {
	Append(context.Context, ...ContextItem) error
	// AppendReducer is the shared working-memory reducer's idempotent append.
	// It deduplicates on execution identity (run/task/attempt from metadata)
	// plus kind and content hash, so two tasks reporting the same finding keep
	// distinct provenance instead of collapsing. On a duplicate it merges
	// immutable evidence refs and refreshes metadata rather than overwriting
	// provenance.
	AppendReducer(context.Context, ...ContextItem) error
	// UpsertCandidate creates a candidate or refreshes an existing
	// non-confirmed candidate with the same canonical identity. A confirmed
	// duplicate remains confirmed and is returned unchanged.
	UpsertCandidate(context.Context, ContextItem) (ContextItem, error)
	Get(context.Context, string) (ContextItem, error)
	GetMany(context.Context, []string) ([]ContextItem, error)
	Query(context.Context, RepositoryQuery) ([]ContextItem, error)
	MarkSuperseded(context.Context, []string, string) error
	// UpdateLifecycle changes the lifecycle of explicitly selected records.
	// Callers must authorise and select IDs before calling this method; it is
	// intentionally an ID-only mutation so repository users cannot broaden a
	// private scope through a lifecycle request.
	UpdateLifecycle(context.Context, []string, ContextLifecycle) error
	// BindCandidates records accepted-run evidence before lifecycle promotion.
	// Callers must authorize and select IDs; the repository deliberately does
	// not widen a caller's scope.
	BindCandidates(context.Context, []string, CandidateBinding) error
	// ConfirmCandidates binds sealed evidence, promotes candidates, and applies
	// any recorded supersession links in one transaction.  This is the only
	// runtime promotion path for candidate revisions.
	ConfirmCandidates(context.Context, []string, CandidateBinding) error
	UpdateEmbeddingState(context.Context, string, string, string) error
	AddEdges(context.Context, ...ContextEdge) error
	SearchExact(context.Context, SearchRequest) ([]SearchResult, error)
	SearchLexical(context.Context, SearchRequest) ([]SearchResult, error)
	RebuildLexical(context.Context) error
	DeleteExpired(context.Context, time.Time) (int64, error)
	RebuildProjection(context.Context, Scope) error
	QuerySharedSessionProjection(context.Context, Scope) ([]ContextItem, error)
	QuerySharedPersistentProjection(context.Context, Scope) ([]ContextItem, error)
	// QuerySharedProjection is a maintenance compatibility query. Runtime
	// projections must use the lifetime-specific methods above.
	QuerySharedProjection(context.Context, Scope) ([]ContextItem, error)
	Revision(context.Context) (int64, error)
	Close() error
}
