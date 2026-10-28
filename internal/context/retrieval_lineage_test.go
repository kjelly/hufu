package context

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
)

func TestSearchLineageAllowsLargeSetsBeforeLimit(t *testing.T) {
	repo := lineageRetrievalFixture(t, 100_002, true)
	scope := lineageRetrievalScope()
	foreign := scope
	foreign.BranchID = "sibling"
	if err := repo.Append(t.Context(), ContextItem{ID: "sibling", Kind: ContextObservation, Content: "anchor", Scope: foreign}); err != nil {
		t.Fatal(err)
	}
	allowed := make([]string, 40_000)
	for i := range allowed {
		allowed[i] = fmt.Sprintf("missing-%06d", i)
	}
	allowed = append(allowed, "item-000000", "item-000000")
	cases := []struct {
		name string
		ids  []string
		want []string
	}{
		{name: "unrestricted", want: []string{"item-100001"}},
		{name: "restricted empty", ids: []string{}, want: nil},
		{name: "larger than SQLite variable limit", ids: allowed, want: []string{"item-000000"}},
		{name: "opaque IDs are not trimmed or folded", ids: []string{" ITEM-000000 "}, want: nil},
		{name: "SQL text is only data", ids: []string{"') OR 1=1 --"}, want: nil},
		{name: "allowance cannot widen canonical scope", ids: []string{"sibling"}, want: nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := SearchRequest{Query: "anchor", Scope: scope, Limit: 1}.WithAllowedItemIDs(tc.ids)
			for name, search := range map[string]func(context.Context, SearchRequest) ([]SearchResult, error){
				"exact": repo.SearchExact, "FTS": repo.SearchLexical,
			} {
				rows, err := search(t.Context(), req)
				if err != nil {
					t.Fatalf("%s: %v", name, err)
				}
				if tc.ids == nil && name == "FTS" {
					if len(rows) != 1 {
						t.Fatalf("unrestricted FTS returned %d rows", len(rows))
					}
					continue
				}
				if ids := searchResultIDs(rows); !slices.Equal(ids, tc.want) {
					t.Fatalf("%s IDs = %v, want %v", name, ids, tc.want)
				}
			}
		})
	}
}

// The oracle asks every source for its complete scoped list, then filters by
// lineage before handing candidates to the unchanged hybrid ranking pipeline.
type completeLineageOracle struct {
	repo  *SQLiteRepository
	allow map[string]bool
}

func (o completeLineageOracle) search(ctx context.Context, req SearchRequest, lexical bool) ([]SearchResult, error) {
	req.AllowedItemIDs, req.Limit = nil, 100
	search := o.repo.SearchExact
	if lexical {
		search = o.repo.SearchLexical
	}
	rows, err := search(ctx, req)
	var allowed []SearchResult
	for _, row := range rows {
		if o.allow[row.Item.ID] {
			allowed = append(allowed, row)
		}
	}
	return allowed, err
}

func (o completeLineageOracle) SearchExact(ctx context.Context, req SearchRequest) ([]SearchResult, error) {
	return o.search(ctx, req, false)
}

func (o completeLineageOracle) SearchLexical(ctx context.Context, req SearchRequest) ([]SearchResult, error) {
	return o.search(ctx, req, true)
}

func TestHybridLineageMatchesCompleteAuthorizedOracle(t *testing.T) {
	repo := lineageRetrievalFixture(t, 32, false)
	allowed := []string{"item-000000", "item-000002", "item-000004", "item-000006"}
	allow := map[string]bool{}
	for _, id := range allowed {
		allow[id] = true
	}
	items, err := repo.GetMany(t.Context(), []string{"item-000000", "item-000006", "item-000009"})
	if err != nil {
		t.Fatal(err)
	}
	vectorRows := []SearchResult{{Item: items[1], Score: .9}, {Item: items[0], Score: .8}}
	// A deleted ID and an out-of-lineage row returned by a stale backend must
	// be removed before fusion, even when the backend ignores its constraint.
	stale := append(slices.Clone(vectorRows), SearchResult{Item: items[2], Score: 1}, SearchResult{Item: ContextItem{ID: "deleted"}, Score: 1})
	for _, fusion := range []FusionMode{FusionLegacy, FusionRRFNormalized} {
		req := SearchRequest{Query: `"anchor" migration`, Scope: lineageRetrievalScope(), Limit: len(allowed), FilePaths: []string{"internal/target.go"}}.WithAllowedItemIDs(allowed)
		got, trace, err := HybridRetrieveWithOptions(t.Context(), repo, req, HybridRetrievalOptions{Mode: RetrievalActive, Vector: staticVectorSearcher{stale}, Fusion: fusion})
		if err != nil {
			t.Fatal(err)
		}
		want, _, err := HybridRetrieveWithOptions(t.Context(), completeLineageOracle{repo, allow}, req, HybridRetrievalOptions{Mode: RetrievalActive, Vector: staticVectorSearcher{vectorRows}, Fusion: fusion})
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(searchResultIDs(got), searchResultIDs(want)) {
			t.Fatalf("%s order %v, oracle %v", fusion, searchResultIDs(got), searchResultIDs(want))
		}
		for _, rows := range [][]SearchResult{trace.ExactResults, trace.LexicalResults, trace.VectorResults, trace.FusedResults} {
			for _, row := range rows {
				if !allow[row.Item.ID] {
					t.Fatalf("unauthorized trace row %s", row.Item.ID)
				}
			}
		}
	}
}

func TestLineageConstraintRejectsInvalidIDsAndCancellation(t *testing.T) {
	repo := lineageRetrievalFixture(t, 16, false)
	invalid := SearchRequest{Query: "anchor", Scope: lineageRetrievalScope()}.WithAllowedItemIDs([]string{"item-000000", ""})
	for name, search := range map[string]func(context.Context, SearchRequest) ([]SearchResult, error){"exact": repo.SearchExact, "lexical": repo.SearchLexical} {
		if _, err := search(t.Context(), invalid); !errors.Is(err, ErrInvalidAllowedItemID) {
			t.Fatalf("%s accepted an empty ID: %v", name, err)
		}
	}
	if _, err := repo.Query(t.Context(), RepositoryQuery{Scope: invalid.Scope, IDs: invalid.AllowedItemIDs}); !errors.Is(err, ErrInvalidAllowedItemID) {
		t.Fatalf("Query accepted an empty ID: %v", err)
	}
	index := lineageBenchmarkIndex(t, repo)
	if _, err := index.SearchVector(t.Context(), invalid); !errors.Is(err, ErrInvalidAllowedItemID) {
		t.Fatalf("vector search accepted an empty ID: %v", err)
	}
	if err := repo.IterateRetrievable(t.Context(), invalid, func(ContextItem) error { t.Error("visited an invalid allowance"); return nil }); !errors.Is(err, ErrInvalidAllowedItemID) {
		t.Fatalf("canonical iteration accepted an empty ID: %v", err)
	}
	counter := &countingRepository{}
	if _, _, err := HybridRetrieve(t.Context(), counter, nil, invalid); !errors.Is(err, ErrInvalidAllowedItemID) || counter.calls != 0 {
		t.Fatalf("hybrid invalid-ID admission: calls=%d err=%v", counter.calls, err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, _, err := HybridRetrieve(ctx, counter, nil, SearchRequest{Query: "anchor"}); !errors.Is(err, context.Canceled) || counter.calls != 0 {
		t.Fatalf("hybrid cancellation: calls=%d err=%v", counter.calls, err)
	}
	if err := repo.IterateRetrievable(ctx, SearchRequest{Scope: invalid.Scope}, func(ContextItem) error { t.Error("visited after cancellation"); return nil }); !errors.Is(err, context.Canceled) {
		t.Fatalf("canonical iteration cancellation: %v", err)
	}
}

func TestAllowedItemIDsCopyOpaqueIdentity(t *testing.T) {
	ids := []string{"b", " A ", "b", "a"}
	req := (SearchRequest{}).WithAllowedItemIDs(ids)
	ids[0] = "changed"
	if !slices.Equal(req.AllowedItemIDs, []string{" A ", "a", "b"}) {
		t.Fatalf("normalized identities = %v", req.AllowedItemIDs)
	}
	if (SearchRequest{}).WithAllowedItemIDs(nil).AllowedItemIDs != nil || (SearchRequest{}).WithAllowedItemIDs([]string{}).AllowedItemIDs == nil {
		t.Fatal("lost unrestricted versus restricted empty distinction")
	}
}

type cancelAfterLexicalRepository struct {
	*SQLiteRepository
	cancel context.CancelFunc
}

func (r cancelAfterLexicalRepository) SearchLexical(ctx context.Context, req SearchRequest) ([]SearchResult, error) {
	rows, err := r.SQLiteRepository.SearchLexical(ctx, req)
	r.cancel()
	return rows, err
}

func TestHybridLineageCancelsBeforeFusion(t *testing.T) {
	repo := lineageRetrievalFixture(t, 16, false)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	req := SearchRequest{Query: "anchor", Scope: lineageRetrievalScope()}.WithAllowedItemIDs([]string{"item-000000", "item-000002"})
	rows, _, err := HybridRetrieveWithOptions(ctx, cancelAfterLexicalRepository{repo, cancel}, req, HybridRetrievalOptions{Mode: RetrievalOff})
	if !errors.Is(err, context.Canceled) || len(rows) != 0 {
		t.Fatalf("cancellation published a partial ranking: rows=%v err=%v", searchResultIDs(rows), err)
	}
}

type mutatingLineageRepository struct{ *SQLiteRepository }

func (r mutatingLineageRepository) SearchExact(ctx context.Context, req SearchRequest) ([]SearchResult, error) {
	req.AllowedItemIDs[0] = "item-000001"
	return r.SQLiteRepository.SearchExact(ctx, req)
}

func TestHybridLineageSourceCannotChangeFrozenAuthorization(t *testing.T) {
	repo := lineageRetrievalFixture(t, 16, false)
	req := SearchRequest{Query: `"anchor"`, Scope: lineageRetrievalScope(), Limit: 1}.WithAllowedItemIDs([]string{"item-000000"})
	rows, trace, err := HybridRetrieveWithOptions(t.Context(), mutatingLineageRepository{repo}, req, HybridRetrievalOptions{Mode: RetrievalOff})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(searchResultIDs(rows), []string{"item-000000"}) || len(trace.ExactResults) != 0 || req.AllowedItemIDs[0] != "item-000000" {
		t.Fatalf("source changed authorization: rows=%v exact=%v caller=%v", searchResultIDs(rows), searchResultIDs(trace.ExactResults), req.AllowedItemIDs)
	}
}
