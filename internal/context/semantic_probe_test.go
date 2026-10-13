package context

import (
	"context"
	"reflect"
	"testing"
	"time"
)

// probingVectorSearcher reports coverage and query details through the
// probe the retrieval installs, as the semantic runtime does.
type probingVectorSearcher struct {
	recordingVectorSearcher
	sawProbe bool
}

func (s *probingVectorSearcher) SearchVector(ctx context.Context, req SearchRequest) ([]SearchResult, error) {
	probe := SemanticQueryProbeFrom(ctx)
	s.sawProbe = probe != nil
	probe.RecordCoverage(3, 1)
	probe.RecordQuery(42, true, 15*time.Millisecond, 80*time.Millisecond)
	return s.recordingVectorSearcher.SearchVector(ctx, req)
}

func TestHybridRetrieveCopiesProbeIntoTrace(t *testing.T) {
	repository := openProjectionTestRepository(t)
	item := ContextItem{ID: "item", Kind: ContextDecision, Content: "repair safely", Scope: Scope{ProjectID: "project"}, Confidence: 1}
	if err := repository.Append(t.Context(), item); err != nil {
		t.Fatal(err)
	}
	request := SearchRequest{Query: "repair safely", Scope: Scope{ProjectID: "project"}, Limit: 10}
	tests := []struct {
		name      string
		traced    bool
		wantProbe bool
	}{
		{name: "traced query gets a probe", traced: true, wantProbe: true},
		{name: "untraced query gets none", traced: false, wantProbe: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			vector := &probingVectorSearcher{recordingVectorSearcher: recordingVectorSearcher{results: []SearchResult{{Item: item, Score: 0.9}}}}
			options := HybridRetrievalOptions{Mode: RetrievalActive, Vector: vector}
			var trace SemanticRetrievalTrace
			if tc.traced {
				options.TraceHasher = semanticTestTraceHasher(t)
				options.TraceSink = func(got SemanticRetrievalTrace) { trace = got }
			}
			if _, _, err := HybridRetrieveWithOptions(t.Context(), repository, request, options); err != nil {
				t.Fatal(err)
			}
			if vector.sawProbe != tc.wantProbe {
				t.Fatalf("probe installed = %v, want %v", vector.sawProbe, tc.wantProbe)
			}
			if !tc.traced {
				return
			}
			if trace.CoveredCandidateCount != 3 || trace.UncoveredCandidateCount != 1 || trace.QueryTokens != 42 ||
				!trace.QueryTruncated || trace.QueueWaitMillis != 15 || trace.EmbedMillis != 80 {
				t.Fatalf("trace probe fields = %+v", trace)
			}
		})
	}
}

func TestNewLexicalRouteTrace(t *testing.T) {
	hasher := semanticTestTraceHasher(t)
	trace, err := NewLexicalRouteTrace(hasher, "goal text")
	if err != nil {
		t.Fatal(err)
	}
	if trace.Mode != RetrievalOff || trace.Route != SemanticRouteLexical || trace.QueryHMAC != hasher.HashString("goal text") || len(trace.TraceID) != 32 {
		t.Fatalf("lexical route trace = %+v", trace)
	}
}

func TestSemanticQueryProbeNilSafe(t *testing.T) {
	var probe *SemanticQueryProbe
	probe.RecordCoverage(1, 1)
	probe.RecordQuery(1, true, time.Second, time.Second)
	var trace SemanticRetrievalTrace
	probe.apply(&trace)
	if !reflect.DeepEqual(trace, SemanticRetrievalTrace{}) {
		t.Fatalf("nil probe changed the trace: %+v", trace)
	}
	if SemanticQueryProbeFrom(t.Context()) != nil {
		t.Fatal("empty context returned a probe")
	}
}
