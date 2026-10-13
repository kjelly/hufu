package context

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type boundedSupplementalRepo struct {
	exact, lexical, rows int
	violate              bool
	content              string
}

func (r *boundedSupplementalRepo) SearchExact(ctx context.Context, req SearchRequest) ([]SearchResult, error) {
	r.exact++
	return r.result(ctx, req, min(2, req.Limit))
}
func (r *boundedSupplementalRepo) SearchLexical(ctx context.Context, req SearchRequest) ([]SearchResult, error) {
	r.lexical++
	return r.result(ctx, req, req.Limit)
}
func (r *boundedSupplementalRepo) result(ctx context.Context, req SearchRequest, n int) ([]SearchResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if r.violate {
		n = req.Limit + 1
	}
	var out []SearchResult
	for range n {
		r.rows++
		id := fmt.Sprint(r.rows)
		out = append(out, SearchResult{Item: ContextItem{ID: id, Content: r.content + "sentinel " + id, ContentHash: id, Scope: req.Scope, Lifecycle: LifecycleConfirmed}, Score: 1})
	}
	return out, nil
}

func TestSupplementalRetrievalBoundsActualCallsAndSourceRows(t *testing.T) {
	query := strings.Builder{}
	for index := range 30 {
		fmt.Fprintf(&query, "path%d/file.go ", index)
	}
	repo := &boundedSupplementalRepo{}
	out, err := RetrieveSupplemental(t.Context(), repo, SearchRequest{Query: query.String(), Scope: Scope{ProjectID: "p"}, Limit: 4})
	if err != nil {
		t.Fatal(err)
	}
	if repo.exact != 8 || repo.lexical != 1 || repo.rows != 64 || out.Work.CandidateRows != 64 || len(out.Results) != 4 || out.TruncatedTerms < 22 {
		t.Fatalf("unbounded supplementary work: repo=%+v work=%+v results=%d truncated=%d", repo, out.Work, len(out.Results), out.TruncatedTerms)
	}
	violating := &boundedSupplementalRepo{violate: true}
	if _, err := RetrieveSupplemental(t.Context(), violating, SearchRequest{Query: "hint", Scope: Scope{ProjectID: "p"}, Limit: 4}); !errors.Is(err, ErrSupplementalSourceLimit) {
		t.Fatalf("oversized source accepted: %v", err)
	}
}

func TestSupplementalRetrievalUsesSQLAuthorizationBeforeLimit(t *testing.T) {
	repo, err := OpenSQLite(filepath.Join(t.TempDir(), "context.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	scope := Scope{ProjectID: "p", TeamID: "team"}
	var items []ContextItem
	for index := range 100 {
		items = append(items, ContextItem{ID: fmt.Sprint(index), Content: "hint distractor", Kind: ContextPattern, Scope: scope, Lifecycle: LifecycleConfirmed})
	}
	items = append(items, ContextItem{ID: "allowed", Content: "hint positive sentinel", Kind: ContextPattern, Scope: scope, Lifecycle: LifecycleConfirmed})
	if err := repo.Append(t.Context(), items...); err != nil {
		t.Fatal(err)
	}
	out, err := RetrieveSupplemental(t.Context(), repo, SearchRequest{Query: "hint", Scope: scope, Limit: 1, AllowedItemIDs: []string{"allowed"}})
	if err != nil || len(out.Results) != 1 || out.Results[0].Item.ID != "allowed" || out.Work.CandidateRows != 1 {
		t.Fatalf("allowed item lost behind distractors: %+v, %v", out, err)
	}
}

func TestSupplementalRetrievalCancelsDuringMMRWithoutPublishingPartialResults(t *testing.T) {
	// Large canonical content makes the existing pairwise MMR work exceed the
	// deadline, even though all source I/O returned synchronously and on time.
	repo := &boundedSupplementalRepo{content: strings.Repeat("canonical memory background ", 2048)}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Millisecond)
	defer cancel()
	out, err := RetrieveSupplemental(ctx, repo, SearchRequest{Query: "hint", Scope: Scope{ProjectID: "p"}, Limit: 20})
	if !errors.Is(err, context.DeadlineExceeded) || len(out.Results) != 0 {
		t.Fatalf("CPU ranking ignored cancellation or published a partial selection: results=%d err=%v", len(out.Results), err)
	}
	if repo.lexical != 1 || repo.rows != 64 || out.Work.CandidateRows != 64 {
		t.Fatal("ranking deadline lost actual completed source-work accounting")
	}
}
