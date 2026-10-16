package context

import (
	"encoding/json/v2"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/kjelly/hufu/internal/embedding"
)

// Existing tests map errors and exercise one generic fallback separately.
// Pin the observable lexical result and content-free reason for each rollout
// failure in both serving and shadow mode, including wrapped encoder errors.
func TestHybridRetrieveTypedFailuresPreserveLexicalResults(t *testing.T) {
	repository := openProjectionTestRepository(t)
	item := ContextItem{ID: "private-lexical-fixture-id", Kind: ContextDecision, Content: "semantic fallback guidance", Scope: Scope{ProjectID: "project"}, Confidence: 1}
	if err := repository.Append(t.Context(), item); err != nil {
		t.Fatal(err)
	}
	request := SearchRequest{Query: "semantic fallback guidance", Scope: item.Scope}
	baseline, baselineTrace, err := HybridRetrieveWithOptions(t.Context(), repository, request, HybridRetrievalOptions{Mode: RetrievalOff})
	if err != nil || len(baseline) != 1 {
		t.Fatalf("invalid lexical baseline: %v %v", baseline, err)
	}
	for _, tc := range []struct {
		err    error
		reason SemanticFallbackReason
	}{
		{ErrSemanticWarming, SemanticFallbackEmbeddingWarming},
		{ErrSemanticBusy, SemanticFallbackEmbeddingBusy},
		{ErrSemanticTimeout, SemanticFallbackEmbeddingTimeout},
		{ErrSemanticProjectionIncomplete, SemanticFallbackProjectionIncomplete},
		{embedding.ErrModelProfileMismatch, SemanticFallbackProfileMismatch},
	} {
		for _, mode := range []RetrievalMode{RetrievalActive, RetrievalShadow} {
			t.Run(string(tc.reason)+"/"+string(mode), func(t *testing.T) {
				const privateError = "private encoder failure text"
				vector := &recordingVectorSearcher{err: fmt.Errorf("%s: %w", privateError, tc.err)}
				var captured SemanticRetrievalTrace
				traces := 0
				results, trace, err := HybridRetrieveWithOptions(t.Context(), repository, request, HybridRetrievalOptions{
					Mode: mode, Vector: vector, TraceHasher: semanticTestTraceHasher(t),
					TraceSink: func(value SemanticRetrievalTrace) { captured = value; traces++ },
				})
				if err != nil || !reflect.DeepEqual(results, baseline) || !reflect.DeepEqual(trace, baselineTrace) {
					t.Fatalf("typed failure changed lexical retrieval: %v", err)
				}
				if traces != 1 || vector.calls != 1 || captured.FallbackReason != tc.reason || captured.Mode != mode {
					t.Fatalf("wrong observed fallback: calls=%d traces=%d reason=%s mode=%s", vector.calls, traces, captured.FallbackReason, captured.Mode)
				}
				encoded, err := json.Marshal(captured)
				if err != nil {
					t.Fatal(err)
				}
				for _, raw := range []string{item.ID, item.Content, request.Query, privateError} {
					if strings.Contains(string(encoded), raw) {
						t.Fatalf("trace leaked source text: %s", encoded)
					}
				}
			})
		}
	}
}
