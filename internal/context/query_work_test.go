package context

import (
	"context"
	"path/filepath"
	"testing"
)

func TestQueryWorkObserverMeasuresCallsWithoutChangingSelection(t *testing.T) {
	repo, err := OpenSQLite(filepath.Join(t.TempDir(), "context.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	scope := Scope{ProjectID: "project", TeamID: "team"}
	if err := repo.Append(t.Context(), ContextItem{ID: "path", Content: "internal/team/router.go sentinel", Scope: scope, Kind: ContextPattern, Confidence: 1}); err != nil {
		t.Fatal(err)
	}
	request := SearchRequest{Query: "internal/team/router.go", Scope: scope, Limit: 5}
	baseline, _, err := HybridRetrieve(t.Context(), repo, nil, request)
	if err != nil {
		t.Fatal(err)
	}
	var observed []QueryWork
	ctx := WithQueryWorkObserver(t.Context(), func(_ context.Context, work QueryWork) { observed = append(observed, work) })
	candidate, _, err := HybridRetrieve(ctx, repo, nil, request)
	if err != nil || len(observed) != 1 || len(candidate) != len(baseline) || len(candidate) != 1 || candidate[0].Item.ID != baseline[0].Item.ID {
		t.Fatalf("observed selection differs: %v/%v, work %v, error %v", candidate, baseline, observed, err)
	}
	exact, err := repo.SearchExact(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	lexical, err := repo.SearchLexical(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	if observed[0].ExactCalls != 1 || observed[0].LexicalCalls != 1 || observed[0].VectorCalls != 0 || observed[0].CandidateRows != len(exact)+len(lexical) || observed[0].Elapsed <= 0 {
		t.Fatalf("retrieval work = %+v", observed[0])
	}
}

func TestQueryWorkCountsSourceRowsBeforeFilteringAndFinalLimit(t *testing.T) {
	repo, err := OpenSQLite(filepath.Join(t.TempDir(), "context.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	vector := staticVectorSearcher{results: []SearchResult{
		{Item: ContextItem{ID: "allowed", Content: "allowed sentinel", ContentHash: "one", Scope: Scope{ProjectID: "p"}}, Score: 1},
		{Item: ContextItem{ID: "foreign", Content: "foreign sentinel", ContentHash: "two", Scope: Scope{ProjectID: "q"}}, Score: 1},
	}}
	var work QueryWork
	ctx := WithQueryWorkObserver(t.Context(), func(_ context.Context, observed QueryWork) { work = observed })
	results, _, err := HybridRetrieve(ctx, repo, vector, SearchRequest{Query: "unrelated", Scope: Scope{ProjectID: "p"}, Limit: 1})
	if err != nil || len(results) != 1 || results[0].Item.ID != "allowed" {
		t.Fatalf("authorized result: %v, %v", results, err)
	}
	if work.CandidateRows != 2 || work.VectorCalls != 1 {
		t.Fatalf("work was reduced to final Top-K: %+v", work)
	}
}
