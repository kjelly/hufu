package context

import (
	"context"
	"errors"
	"time"
)

// ErrReadScopeDenied means the item exists but is outside the caller's scope.
var ErrReadScopeDenied = errors.New("context read scope denied")

// ScopedReadOptions is the authorization boundary for reading one context
// item by ID. An ID is only a lookup key: project/team/private-agent scope is
// still enforced, and content is omitted unless IncludeContent is explicit.
type ScopedReadOptions struct {
	Scope          Scope
	AllAgents      bool
	IncludeContent bool
}

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
	// Iterate streams one ordered read snapshot. The visitor must not call
	// this repository or perform writes: canonical repositories intentionally
	// own one SQLite connection, so re-entry while Rows is open can deadlock.
	Iterate(context.Context, RepositoryQuery, func(ContextItem) error) error
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

// ReadOnlyRepository is the query surface exposed to inspectors and other
// offline projections. It intentionally omits every mutation and projection
// rebuild method from Repository.
type ReadOnlyRepository interface {
	GetScoped(context.Context, string, ScopedReadOptions) (ContextItem, error)
	Query(context.Context, RepositoryQuery) ([]ContextItem, error)
	// Iterate has the same non-reentrancy contract as Repository.Iterate.
	Iterate(context.Context, RepositoryQuery, func(ContextItem) error) error
	SearchExact(context.Context, SearchRequest) ([]SearchResult, error)
	SearchLexical(context.Context, SearchRequest) ([]SearchResult, error)
	QuerySharedSessionProjection(context.Context, Scope) ([]ContextItem, error)
	QuerySharedPersistentProjection(context.Context, Scope) ([]ContextItem, error)
	Revision(context.Context) (int64, error)
	ExperienceAggregate(context.Context, string, string) (ExperienceAggregate, error)
	ListExperienceAggregates(context.Context, string) ([]ExperienceAggregate, error)
	ListExperienceAggregatesForScope(context.Context, string, Scope) ([]ExperienceAggregate, error)
	ActiveMemoryPolicyVersion(context.Context) (MemoryPolicyVersionRecord, error)
	// Promotion queries belong on the read-only surface so operator list,
	// detail, completion, and refresh paths never need a writable repository.
	GetPromotion(context.Context, string, string, string) (PromotionProposal, error)
	ListPromotions(context.Context, string, string) ([]PromotionProposal, error)
	ListPromotionMetadataForScope(context.Context, string, string, int) ([]PromotionMetadata, error)
	ListContextMetadataForScope(context.Context, Scope, int) ([]ContextMetadata, error)
	// Conflict queries degrade on stores older than migration 11: listings
	// return ErrConflictsUnavailable and item lookups report no conflicts.
	ListConflicts(context.Context, ConflictQuery) ([]ConflictView, error)
	GetConflict(context.Context, string, string, string) (ConflictView, error)
	OpenConflictsForItems(context.Context, string, string, []string) (map[string][]string, error)
	StorageDiagnostics(context.Context) (StorageDiagnostics, error)
	Close() error
}

// ContextMetadata is the content-free subset exposed to bounded operator
// discovery surfaces such as shell completion.
type ContextMetadata struct {
	ID        string
	Kind      ContextKind
	Lifecycle ContextLifecycle
}

// PromotionMetadata excludes draft, target path, sources, and rejection text.
type PromotionMetadata struct {
	ID     string
	Type   PromotionType
	Status PromotionStatus
}

// StorageDiagnostics contains content-free, read-only SQLite health facts.
type StorageDiagnostics struct {
	PageCount         int64
	FreelistCount     int64
	PageSize          int64
	JournalMode       string
	WALAutoCheckpoint int64
	SchemaVersion     int64
	FTSRows           int64
	ContextRows       int64
}
