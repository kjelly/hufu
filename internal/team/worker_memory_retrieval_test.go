package team

import (
	"context"
	"path/filepath"
	"slices"
	"testing"

	"github.com/kjelly/hufu/internal/agent"
	contextstore "github.com/kjelly/hufu/internal/context"
)

type lineageRequestRepository struct {
	contextstore.Repository
	requests []contextstore.SearchRequest
}

func (r *lineageRequestRepository) SearchExact(ctx context.Context, req contextstore.SearchRequest) ([]contextstore.SearchResult, error) {
	r.requests = append(r.requests, req)
	return r.Repository.SearchExact(ctx, req)
}

func (r *lineageRequestRepository) SearchLexical(ctx context.Context, req contextstore.SearchRequest) ([]contextstore.SearchResult, error) {
	r.requests = append(r.requests, req)
	return r.Repository.SearchLexical(ctx, req)
}

type lineageRequestVector struct {
	requests []contextstore.SearchRequest
	results  []contextstore.SearchResult
}

func (v *lineageRequestVector) SearchVector(_ context.Context, req contextstore.SearchRequest) ([]contextstore.SearchResult, error) {
	v.requests = append(v.requests, req)
	return v.results, nil
}

func TestWorkerMemoryPassesAncestorConstraintToEverySource(t *testing.T) {
	for _, empty := range []bool{false, true} {
		t.Run(map[bool]string{false: "authorized ancestor", true: "restricted empty"}[empty], func(t *testing.T) {
			repo, err := contextstore.OpenSQLite(filepath.Join(t.TempDir(), "context.sqlite"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = repo.Close() })
			scope := contextstore.Scope{ProjectID: "p", TeamID: "team", SessionID: "sess", BranchID: "main", AgentID: "worker"}
			before := contextstore.ContextItem{ID: "before", Kind: contextstore.ContextObservation, Content: "anchor migration before fork", Scope: scope}
			after := contextstore.ContextItem{ID: "after", Kind: contextstore.ContextObservation, Content: "anchor migration after fork", Scope: scope}
			if err := repo.Append(t.Context(), before, after); err != nil {
				t.Fatal(err)
			}
			wrapped := &lineageRequestRepository{Repository: repo}
			vector := &lineageRequestVector{results: []contextstore.SearchResult{{Item: after, Score: 1}}}
			allow := map[string]bool{"before": !empty, "after": false}
			scope.BranchID = "child"
			bundle, err := NewWorkerMemoryService(wrapped, vector).Recall(t.Context(), WorkerMemoryRecallRequest{
				WorkerID: "worker", Scope: scope, Query: `"anchor" migration`,
				Policy:         agent.WorkerMemoryPolicy{Mode: agent.WorkerMemorySession, MaxItems: 5, MaxTokens: 1000},
				SessionLineage: []WorkerMemoryLineageBranch{{BranchID: "main", RestrictItems: true, AllowedItemIDs: allow}},
			})
			if err != nil {
				t.Fatal(err)
			}
			want := []string{}
			if !empty {
				want = []string{"before"}
			}
			if len(wrapped.requests) != 2 || len(vector.requests) != 1 {
				t.Fatalf("source calls: lexical/exact=%d vector=%d", len(wrapped.requests), len(vector.requests))
			}
			for _, req := range append(wrapped.requests, vector.requests...) {
				if req.AllowedItemIDs == nil || !slices.Equal(req.AllowedItemIDs, want) || req.Scope.BranchID != "main" || req.Limit != 1 {
					t.Fatalf("source lost canonical restriction: %+v", req)
				}
			}
			if len(bundle.Items) != len(want) || (!empty && bundle.Items[0].ID != "before") {
				t.Fatalf("recall items = %v", bundle.Trace.ItemIDs)
			}
		})
	}
}

func TestWorkerMemoryRecallsAncestorBeyondPostForkCandidateCeiling(t *testing.T) {
	repo, scope, allowed := workerAncestorRetrievalFixture(t, 100_010, true)
	scope.BranchID = "child"
	var work contextstore.QueryWork
	ctx := contextstore.WithQueryWorkObserver(t.Context(), func(_ context.Context, observed contextstore.QueryWork) { work = observed })
	request := WorkerMemoryRecallRequest{
		WorkerID: "worker", Scope: scope, Query: `"anchor" migration`,
		Policy:         agent.WorkerMemoryPolicy{Mode: agent.WorkerMemorySession, MaxItems: 8, MaxTokens: 5000},
		SessionLineage: []WorkerMemoryLineageBranch{{BranchID: "main", RestrictItems: true, AllowedItemIDs: allowed}},
	}
	bundle, err := NewWorkerMemoryService(repo, nil).Recall(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	request.SessionLineage = []WorkerMemoryLineageBranch{{BranchID: "main"}}
	oracle, err := NewWorkerMemoryService(completeWorkerLineageOracle{Repository: repo, allowed: allowed}, nil).Recall(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	want := oracle.Trace.ItemIDs
	if len(want) != len(allowed) {
		t.Fatalf("incomplete oracle: %v", want)
	}
	if !slices.Equal(bundle.Trace.ItemIDs, want) {
		t.Fatalf("authorized ancestor order %v, want %v", bundle.Trace.ItemIDs, want)
	}
	if work.CandidateRows != 2*len(allowed) {
		t.Fatalf("hydrated %d source rows, want only the %d authorized exact/FTS rows", work.CandidateRows, 2*len(allowed))
	}
}

type completeWorkerLineageOracle struct {
	contextstore.Repository
	allowed map[string]bool
}

func (o completeWorkerLineageOracle) search(ctx context.Context, req contextstore.SearchRequest, lexical bool) ([]contextstore.SearchResult, error) {
	// This fixture has 100,010 rows. Fetch all scoped matches without an
	// allowed-ID SQL predicate, then authorize before hybrid ranking.
	req.Limit, req.AllowedItemIDs = 100_011, nil
	search := o.Repository.SearchExact
	if lexical {
		search = o.Repository.SearchLexical
	}
	rows, err := search(ctx, req)
	var out []contextstore.SearchResult
	for _, row := range rows {
		if o.allowed[row.Item.ID] {
			out = append(out, row)
		}
	}
	return out, err
}

func (o completeWorkerLineageOracle) SearchExact(ctx context.Context, req contextstore.SearchRequest) ([]contextstore.SearchResult, error) {
	return o.search(ctx, req, false)
}

func (o completeWorkerLineageOracle) SearchLexical(ctx context.Context, req contextstore.SearchRequest) ([]contextstore.SearchResult, error) {
	return o.search(ctx, req, true)
}
