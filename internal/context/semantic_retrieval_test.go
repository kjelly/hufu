package context

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/kjelly/hufu/internal/embedding"
)

func TestHybridRetrieveOptionsValidateBeforeRepositoryOrVectorCalls(t *testing.T) {
	hasher := semanticTestTraceHasher(t)
	vector := &recordingVectorSearcher{}
	tests := []HybridRetrievalOptions{
		{Mode: "unknown"},
		{Mode: RetrievalOff, Vector: vector},
		{Mode: RetrievalOff, UnavailableReason: SemanticFallbackNone},
		{Mode: RetrievalOff, TraceHasher: hasher, TraceSink: func(SemanticRetrievalTrace) {}},
		{Mode: RetrievalActive},
		{Mode: RetrievalShadow, TraceHasher: hasher, Vector: vector},
		{Mode: RetrievalActive, Vector: vector, UnavailableReason: "unknown"},
	}
	for i, options := range tests {
		repository := &countingRepository{}
		vector.calls = 0
		if _, _, err := HybridRetrieveWithOptions(t.Context(), repository, SearchRequest{}, options); err == nil {
			t.Fatalf("case %d accepted invalid options: %#v", i, options)
		}
		if repository.calls != 0 || vector.calls != 0 {
			t.Fatalf("case %d performed work before validation: repo=%d vector=%d", i, repository.calls, vector.calls)
		}
	}
}

func TestHybridRetrieveShadowMatchesOffAndEmitsContentFreeTrace(t *testing.T) {
	repository := openProjectionTestRepository(t)
	item := ContextItem{ID: "raw-item-id", Kind: ContextDecision, Content: "repair safely without leaking", Scope: Scope{ProjectID: "project"}, Confidence: 1}
	if err := repository.Append(t.Context(), item); err != nil {
		t.Fatal(err)
	}
	request := SearchRequest{Query: "repair internal/context/store.txt safely", Scope: Scope{ProjectID: "project"}, Limit: 10}
	offResults, offTrace, err := HybridRetrieveWithOptions(t.Context(), repository, request, HybridRetrievalOptions{Mode: RetrievalOff})
	if err != nil {
		t.Fatal(err)
	}

	model := projectionTestModel("trace-model", "1")
	vector := &recordingVectorSearcher{
		results: []SearchResult{{Item: item, Score: 0.9}},
		model:   model, generationID: "generation-secret", sourceRevision: 7,
	}
	hasher := semanticTestTraceHasher(t)
	var semanticTrace SemanticRetrievalTrace
	shadowResults, shadowTrace, err := HybridRetrieveWithOptions(t.Context(), repository, request, HybridRetrievalOptions{
		Mode: RetrievalShadow, Vector: vector, TraceHasher: hasher,
		TraceSink: func(trace SemanticRetrievalTrace) { semanticTrace = trace },
	})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(shadowResults, offResults) || !reflect.DeepEqual(shadowTrace, offTrace) {
		t.Fatalf("shadow changed official results\noff=%#v %#v\nshadow=%#v %#v", offResults, offTrace, shadowResults, shadowTrace)
	}
	if len(shadowTrace.VectorResults) != 0 || shadowTrace.RetrievalInsufficient != offTrace.RetrievalInsufficient {
		t.Fatal("shadow semantic results affected official retrieval trace")
	}
	if vector.calls != 1 || len(vector.queries) != 1 || vector.queries[0] != "repair safely" {
		t.Fatalf("semantic query calls=%d queries=%v", vector.calls, vector.queries)
	}
	if semanticTrace.Mode != RetrievalShadow || semanticTrace.FallbackReason != SemanticFallbackNone ||
		semanticTrace.ModelID != model.ID || semanticTrace.ModelRevision != model.Revision ||
		semanticTrace.ModelHash != model.ManifestSHA256 || semanticTrace.GenerationID != "generation-secret" ||
		semanticTrace.SourceRevision != 7 {
		t.Fatalf("semantic trace identity = %#v", semanticTrace)
	}
	if len(semanticTrace.TraceID) != 32 {
		t.Fatalf("trace ID = %q", semanticTrace.TraceID)
	}
	if _, err = hex.DecodeString(semanticTrace.TraceID); err != nil {
		t.Fatalf("trace ID is not hex: %v", err)
	}
	encoded, err := json.Marshal(semanticTrace)
	if err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{request.Query, item.ID, item.Content, "generation-secret"} {
		if strings.Contains(string(encoded), raw) && raw != "generation-secret" {
			t.Fatalf("content-free trace leaked %q: %s", raw, encoded)
		}
	}
	if semanticTrace.QueryHMAC != hasher.HashString(request.Query) || len(semanticTrace.SemanticResultHashes) != 1 {
		t.Fatalf("semantic trace hashes = %#v", semanticTrace)
	}
}

func TestHybridRetrieveComponentFailureFallsBackButCancellationAndCanonicalErrorsPropagate(t *testing.T) {
	repository := openProjectionTestRepository(t)
	if err := repository.Append(t.Context(), ContextItem{
		ID: "lexical", Kind: ContextDecision, Content: "semantic fallback guidance",
		Scope: Scope{ProjectID: "project"}, Confidence: 1,
	}); err != nil {
		t.Fatal(err)
	}
	request := SearchRequest{Query: "semantic fallback", Scope: Scope{ProjectID: "project"}}
	offResults, offTrace, err := HybridRetrieveWithOptions(t.Context(), repository, request, HybridRetrievalOptions{Mode: RetrievalOff})
	if err != nil {
		t.Fatal(err)
	}
	hasher := semanticTestTraceHasher(t)
	var fallbackTrace SemanticRetrievalTrace
	failed := &recordingVectorSearcher{err: errors.New("embedding unavailable")}
	activeResults, activeTrace, err := HybridRetrieveWithOptions(t.Context(), repository, request, HybridRetrievalOptions{
		Mode: RetrievalActive, Vector: failed, TraceHasher: hasher,
		TraceSink: func(trace SemanticRetrievalTrace) { fallbackTrace = trace },
	})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(activeResults, offResults) || !reflect.DeepEqual(activeTrace, offTrace) {
		t.Fatal("component failure did not preserve exact + FTS result")
	}
	if fallbackTrace.FallbackReason != SemanticFallbackEmbeddingFailed {
		t.Fatalf("fallback reason = %s", fallbackTrace.FallbackReason)
	}

	for _, propagated := range []error{context.Canceled, context.DeadlineExceeded, ErrSemanticCanonicalRepo} {
		failed.err = propagated
		results, _, retrieveErr := HybridRetrieveWithOptions(t.Context(), repository, request, HybridRetrievalOptions{
			Mode: RetrievalActive, Vector: failed,
		})
		if !errors.Is(retrieveErr, propagated) {
			t.Fatalf("error %v became %v", propagated, retrieveErr)
		}
		if results != nil {
			t.Fatalf("fatal semantic error returned results: %v", searchResultIDs(results))
		}
	}
}

func TestHybridRetrieveEmptyRemainderAndUnavailableVectorHaveTypedFallbacks(t *testing.T) {
	repository := openProjectionTestRepository(t)
	hasher := semanticTestTraceHasher(t)
	vector := &recordingVectorSearcher{}
	var traces []SemanticRetrievalTrace
	sink := func(trace SemanticRetrievalTrace) { traces = append(traces, trace) }

	_, _, err := HybridRetrieveWithOptions(t.Context(), repository, SearchRequest{
		Query: "internal/context/store.go", Scope: Scope{ProjectID: "project"},
	}, HybridRetrievalOptions{Mode: RetrievalShadow, Vector: vector, TraceHasher: hasher, TraceSink: sink})
	if err != nil {
		t.Fatal(err)
	}
	if vector.calls != 0 || len(traces) != 1 || traces[0].FallbackReason != SemanticFallbackEmptyRemainder {
		t.Fatalf("empty remainder trace=%#v calls=%d", traces, vector.calls)
	}

	_, _, err = HybridRetrieveWithOptions(t.Context(), repository, SearchRequest{
		Query: "semantic query", Scope: Scope{ProjectID: "project"},
	}, HybridRetrievalOptions{
		Mode: RetrievalActive, UnavailableReason: SemanticFallbackAssetMissing,
		TraceHasher: hasher, TraceSink: sink,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(traces) != 2 || traces[1].FallbackReason != SemanticFallbackAssetMissing {
		t.Fatalf("unavailable trace=%#v", traces)
	}
}

func TestSemanticTraceCapsRankedHashes(t *testing.T) {
	repository := openProjectionTestRepository(t)
	results := make([]SearchResult, 40)
	for i := range results {
		results[i] = SearchResult{Item: ContextItem{ID: "semantic-id-" + string(rune('A'+i)), Scope: Scope{ProjectID: "project"}}, Score: float64(40 - i)}
	}
	vector := &recordingVectorSearcher{results: results}
	hasher := semanticTestTraceHasher(t)
	var trace SemanticRetrievalTrace
	_, _, err := HybridRetrieveWithOptions(t.Context(), repository, SearchRequest{
		Query: "semantic query", Scope: Scope{ProjectID: "project"},
	}, HybridRetrievalOptions{
		Mode: RetrievalShadow, Vector: vector, TraceHasher: hasher,
		TraceSink: func(got SemanticRetrievalTrace) { trace = got },
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(trace.SemanticResultHashes) != 32 || !trace.HashesTruncated || trace.SemanticResultCount != 40 || trace.SemanticCandidateCount != 40 {
		t.Fatalf("bounded trace = %#v", trace)
	}
}

func TestHMACTraceHasherRequiresStrongCopiedKey(t *testing.T) {
	if _, err := NewHMACTraceHasher([]byte("short")); err == nil {
		t.Fatal("short HMAC key was accepted")
	}
	key := []byte("0123456789abcdef0123456789abcdef")
	hasher, err := NewHMACTraceHasher(key)
	if err != nil {
		t.Fatal(err)
	}
	want := hasher.HashString("query")
	clear(key)
	if got := hasher.HashString("query"); got != want || got != strings.ToLower(got) || len(got) != 64 {
		t.Fatalf("HMAC after caller key mutation = %q, want %q", got, want)
	}
}

type recordingVectorSearcher struct {
	results        []SearchResult
	err            error
	queries        []string
	calls          int
	model          embedding.ModelIdentity
	generationID   string
	sourceRevision int64
}

func (s *recordingVectorSearcher) SearchVector(_ context.Context, req SearchRequest) ([]SearchResult, error) {
	s.calls++
	s.queries = append(s.queries, req.Query)
	return slices.Clone(s.results), s.err
}

func (s *recordingVectorSearcher) SemanticTraceIdentity() (embedding.ModelIdentity, string, int64) {
	return s.model, s.generationID, s.sourceRevision
}

type countingRepository struct {
	Repository
	calls int
}

func (r *countingRepository) SearchExact(context.Context, SearchRequest) ([]SearchResult, error) {
	r.calls++
	return nil, nil
}

func (r *countingRepository) SearchLexical(context.Context, SearchRequest) ([]SearchResult, error) {
	r.calls++
	return nil, nil
}

func semanticTestTraceHasher(t *testing.T) TraceHasher {
	t.Helper()
	hasher, err := NewHMACTraceHasher([]byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	return hasher
}
