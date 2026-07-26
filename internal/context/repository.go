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
	UpdateEmbeddingState(context.Context, string, string, string) error
	AddEdges(context.Context, ...ContextEdge) error
	SearchExact(context.Context, SearchRequest) ([]SearchResult, error)
	SearchLexical(context.Context, SearchRequest) ([]SearchResult, error)
	RebuildLexical(context.Context) error
	DeleteExpired(context.Context, time.Time) (int64, error)
	RebuildProjection(context.Context, Scope) error
	Revision(context.Context) (int64, error)
	Close() error
}
