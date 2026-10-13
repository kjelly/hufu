package context

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"

	"github.com/kjelly/hufu/internal/embedding"
)

type RetrievalMode string

const (
	RetrievalOff    RetrievalMode = "off"
	RetrievalShadow RetrievalMode = "shadow"
	RetrievalActive RetrievalMode = "active"
)

type SemanticFallbackReason string

const (
	SemanticFallbackNone                    SemanticFallbackReason = "none"
	SemanticFallbackEmptyRemainder          SemanticFallbackReason = "empty_remainder"
	SemanticFallbackAssetMissing            SemanticFallbackReason = "asset_missing"
	SemanticFallbackAssetInvalid            SemanticFallbackReason = "asset_invalid"
	SemanticFallbackProjectionMissing       SemanticFallbackReason = "projection_missing"
	SemanticFallbackProjectionStale         SemanticFallbackReason = "projection_stale"
	SemanticFallbackProjectionRefreshFailed SemanticFallbackReason = "projection_refresh_failed"
	SemanticFallbackProjectionCorrupt       SemanticFallbackReason = "projection_corrupt"
	SemanticFallbackEmbeddingFailed         SemanticFallbackReason = "embedding_failed"
	SemanticFallbackInvalidVector           SemanticFallbackReason = "invalid_vector"
	// Reasons added with the Ollama runtime (spec D1, D3, B1).
	SemanticFallbackProjectionIncomplete SemanticFallbackReason = "projection_incomplete"
	SemanticFallbackEmbeddingWarming     SemanticFallbackReason = "embedding_warming"
	SemanticFallbackEmbeddingBusy        SemanticFallbackReason = "embedding_busy"
	SemanticFallbackEmbeddingTimeout     SemanticFallbackReason = "embedding_timeout"
	SemanticFallbackEmbeddingUnavailable SemanticFallbackReason = "embedding_unavailable"
	SemanticFallbackInputTooLong         SemanticFallbackReason = "embedding_input_too_long"
	SemanticFallbackProfileMismatch      SemanticFallbackReason = "embedding_profile_mismatch"
)

// Component-local failures raised by the semantic runtime. They are
// distinct from the caller's context cancellation, which is never a fallback.
var (
	ErrSemanticWarming = errors.New("semantic encoder is not ready")
	ErrSemanticBusy    = errors.New("semantic encoder is busy past the query deadline")
	ErrSemanticTimeout = errors.New("semantic query exceeded its deadline")
)

type TraceHasher interface {
	HashString(string) string
}

type HybridRetrievalOptions struct {
	Vector            VectorSearcher
	Mode              RetrievalMode
	UnavailableReason SemanticFallbackReason
	TraceHasher       TraceHasher
	TraceSink         func(SemanticRetrievalTrace)
	// Observer, when set, records every path's ranks and the fusion steps
	// for an explanation. It never changes the results and is never persisted.
	Observer *RetrievalObservation
	// Fusion selects how lexical and vector lists are fused; empty means
	// FusionLegacy.
	Fusion FusionMode
	// FusionCarriedWeight is FusionScoreNormalized's carried-score weight in
	// (0,1]; 0 means DefaultScoreFusionCarriedWeight.
	FusionCarriedWeight float64
}

// Semantic routes recorded in traces: the semantic lane ran (shadow, active,
// or auto routed to it), or auto kept the request lexical without embedding.
const (
	SemanticRouteSemantic = "semantic"
	SemanticRouteLexical  = "lexical"
)

type SemanticRetrievalTrace struct {
	TraceID   string        `json:"trace_id"`
	QueryHMAC string        `json:"query_hmac"`
	Mode      RetrievalMode `json:"mode"`
	// Attribution: the run, task, and context request that issued the query,
	// and which memory ranking ran it (shared_persistent or worker_private).
	RunID            string `json:"run_id,omitempty"`
	TaskID           string `json:"task_id,omitempty"`
	Attempt          int    `json:"attempt,omitempty"`
	ContextRequestID string `json:"context_request_id,omitempty"`
	Phase            string `json:"phase,omitempty"`
	Trigger          string `json:"trigger,omitempty"`
	Role             string `json:"role,omitempty"`
	Agent            string `json:"agent,omitempty"`
	Source           string `json:"source,omitempty"`
	// Route is SemanticRouteSemantic or SemanticRouteLexical; RouteShare is
	// the routed text's share of non-ASCII letters and digits.
	Route                  string                 `json:"route,omitempty"`
	RouteShare             float64                `json:"route_share,omitempty"`
	ModelID                string                 `json:"model_id,omitempty"`
	ModelRevision          string                 `json:"model_revision,omitempty"`
	ModelHash              string                 `json:"model_hash,omitempty"`
	GenerationID           string                 `json:"generation_id,omitempty"`
	SourceRevision         int64                  `json:"source_revision,omitempty"`
	LexicalResultHashes    []string               `json:"lexical_result_hashes,omitempty"`
	SemanticResultHashes   []string               `json:"semantic_result_hashes,omitempty"`
	SelectedResultHashes   []string               `json:"selected_result_hashes,omitempty"`
	HashesTruncated        bool                   `json:"hashes_truncated"`
	IntersectionCount      int                    `json:"intersection_count"`
	SemanticCandidateCount int                    `json:"semantic_candidate_count"`
	LexicalResultCount     int                    `json:"lexical_result_count"`
	SemanticResultCount    int                    `json:"semantic_result_count"`
	SelectedResultCount    int                    `json:"selected_result_count"`
	LatencyMillis          int64                  `json:"latency_millis"`
	FallbackReason         SemanticFallbackReason `json:"fallback_reason"`
	// Details the vector searcher reported through SemanticQueryProbe.
	CoveredCandidateCount   int   `json:"covered_candidate_count,omitempty"`
	UncoveredCandidateCount int   `json:"uncovered_candidate_count,omitempty"`
	QueryTokens             int   `json:"query_tokens,omitempty"`
	QueryTruncated          bool  `json:"query_truncated,omitempty"`
	QueueWaitMillis         int64 `json:"queue_wait_millis,omitempty"`
	EmbedMillis             int64 `json:"embed_millis,omitempty"`
}

// NewLexicalRouteTrace is the trace of a request auto routing kept lexical:
// no embedding request was sent, so it carries only its identity.
func NewLexicalRouteTrace(hasher TraceHasher, query string) (SemanticRetrievalTrace, error) {
	traceID, err := newSemanticTraceID()
	if err != nil {
		return SemanticRetrievalTrace{}, err
	}
	return SemanticRetrievalTrace{TraceID: traceID, QueryHMAC: hasher.HashString(query), Mode: RetrievalOff, Route: SemanticRouteLexical}, nil
}

type semanticTraceIdentityProvider interface {
	SemanticTraceIdentity() (embedding.ModelIdentity, string, int64)
}

type hmacTraceHasher struct{ key []byte }

func NewHMACTraceHasher(key []byte) (TraceHasher, error) {
	if len(key) < 32 {
		return nil, errors.New("semantic trace HMAC key must be at least 32 bytes")
	}
	return &hmacTraceHasher{key: slices.Clone(key)}, nil
}

func (h *hmacTraceHasher) HashString(value string) string {
	hash := hmac.New(sha256.New, h.key)
	_, _ = hash.Write([]byte(value))
	return hex.EncodeToString(hash.Sum(nil))
}

func validateHybridRetrievalOptions(options HybridRetrievalOptions) error {
	if options.Mode != RetrievalOff && options.Mode != RetrievalShadow && options.Mode != RetrievalActive {
		return fmt.Errorf("unknown retrieval mode %q", options.Mode)
	}
	if (options.TraceHasher == nil) != (options.TraceSink == nil) {
		return errors.New("semantic trace hasher and sink must be configured together")
	}
	if !ValidFusionMode(options.Fusion) {
		return fmt.Errorf("unknown retrieval fusion %q", options.Fusion)
	}
	if !ValidFusionCarriedWeight(options.FusionCarriedWeight) {
		return fmt.Errorf("retrieval fusion carried weight %v is outside [0,1]", options.FusionCarriedWeight)
	}
	if options.UnavailableReason != "" && !validSemanticFallbackReason(options.UnavailableReason) {
		return fmt.Errorf("unknown semantic fallback reason %q", options.UnavailableReason)
	}
	if options.Mode == RetrievalOff {
		if options.Vector != nil || options.UnavailableReason != "" ||
			options.TraceHasher != nil || options.TraceSink != nil {
			return errors.New("off retrieval mode cannot configure semantic dependencies or tracing")
		}
		return nil
	}
	if options.Vector == nil && (options.UnavailableReason == "" || options.UnavailableReason == SemanticFallbackNone) {
		return errors.New("shadow or active retrieval without a vector searcher requires an unavailable reason")
	}
	return nil
}

func validSemanticFallbackReason(reason SemanticFallbackReason) bool {
	switch reason {
	case SemanticFallbackNone, SemanticFallbackEmptyRemainder,
		SemanticFallbackAssetMissing, SemanticFallbackAssetInvalid,
		SemanticFallbackProjectionMissing, SemanticFallbackProjectionStale,
		SemanticFallbackProjectionRefreshFailed, SemanticFallbackProjectionCorrupt,
		SemanticFallbackEmbeddingFailed, SemanticFallbackInvalidVector,
		SemanticFallbackProjectionIncomplete, SemanticFallbackEmbeddingWarming, SemanticFallbackEmbeddingBusy,
		SemanticFallbackEmbeddingTimeout, SemanticFallbackEmbeddingUnavailable, SemanticFallbackInputTooLong,
		SemanticFallbackProfileMismatch:
		return true
	default:
		return false
	}
}

// SemanticFallbackReasonForError maps component-local semantic failures onto
// the fixed content-free enum shared by retrieval callers and manifests.
func SemanticFallbackReasonForError(err error) SemanticFallbackReason {
	switch {
	case errors.Is(err, ErrSemanticProjectionIncomplete):
		return SemanticFallbackProjectionIncomplete
	case errors.Is(err, ErrSemanticWarming):
		return SemanticFallbackEmbeddingWarming
	case errors.Is(err, ErrSemanticBusy):
		return SemanticFallbackEmbeddingBusy
	case errors.Is(err, ErrSemanticTimeout):
		return SemanticFallbackEmbeddingTimeout
	case errors.Is(err, embedding.ErrEmbeddingInputTooLong):
		return SemanticFallbackInputTooLong
	case errors.Is(err, embedding.ErrModelProfileMismatch):
		return SemanticFallbackProfileMismatch
	case errors.Is(err, embedding.ErrOllamaUnavailable), errors.Is(err, embedding.ErrModelNotFound):
		return SemanticFallbackEmbeddingUnavailable
	case errors.Is(err, ErrSemanticInvalidVector), errors.Is(err, embedding.ErrInvalidEmbedding), errors.Is(err, embedding.ErrEmbeddingResponse):
		return SemanticFallbackInvalidVector
	case errors.Is(err, ErrSemanticSnapshotCorrupt):
		return SemanticFallbackProjectionCorrupt
	case errors.Is(err, ErrNoActiveEmbeddingGeneration):
		return SemanticFallbackProjectionMissing
	case errors.Is(err, ErrSemanticIndexStale), errors.Is(err, ErrEmbeddingInventoryDrift):
		return SemanticFallbackProjectionStale
	case errors.Is(err, ErrSemanticProjectionRefresh):
		return SemanticFallbackProjectionRefreshFailed
	default:
		return SemanticFallbackEmbeddingFailed
	}
}

func newSemanticTraceID() (string, error) {
	random := make([]byte, 16)
	if _, err := rand.Read(random); err != nil {
		return "", fmt.Errorf("create semantic trace ID: %w", err)
	}
	return hex.EncodeToString(random), nil
}

func hashRankedResultIDs(hasher TraceHasher, results []SearchResult) ([]string, bool) {
	limit := min(len(results), 32)
	hashes := make([]string, limit)
	for i := range limit {
		hashes[i] = hasher.HashString(results[i].Item.ID)
	}
	return hashes, len(results) > limit
}

func semanticIntersectionCount(left, right []SearchResult) int {
	seen := make(map[string]struct{}, len(left))
	for _, result := range left {
		seen[result.Item.ID] = struct{}{}
	}
	count := 0
	for _, result := range right {
		if _, ok := seen[result.Item.ID]; ok {
			count++
		}
	}
	return count
}
