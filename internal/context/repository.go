package context

import (
	"context"
	"time"
)

type Repository interface {
	Append(context.Context, ...ContextItem) error
	Get(context.Context, string) (ContextItem, error)
	GetMany(context.Context, []string) ([]ContextItem, error)
	Query(context.Context, RepositoryQuery) ([]ContextItem, error)
	MarkSuperseded(context.Context, []string, string) error
	// UpdateLifecycle changes the lifecycle of explicitly selected records.
	// Callers must authorise and select IDs before calling this method; it is
	// intentionally an ID-only mutation so repository users cannot broaden a
	// private scope through a lifecycle request.
	UpdateLifecycle(context.Context, []string, ContextLifecycle) error
	UpdateEmbeddingState(context.Context, string, string, string) error
	AddEdges(context.Context, ...ContextEdge) error
	SearchExact(context.Context, SearchRequest) ([]SearchResult, error)
	SearchLexical(context.Context, SearchRequest) ([]SearchResult, error)
	RebuildLexical(context.Context) error
	DeleteExpired(context.Context, time.Time) (int64, error)
	RebuildProjection(context.Context, Scope) error
	QuerySharedProjection(context.Context, Scope) ([]ContextItem, error)
	Revision(context.Context) (int64, error)
	Close() error
}
