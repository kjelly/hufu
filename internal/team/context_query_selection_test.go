package team

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	contextstore "github.com/kjelly/hufu/internal/context"
)

type querySelectionRepo struct {
	calls  int
	delay  time.Duration
	failAt int
	rows   []contextstore.SearchResult
}

func (r *querySelectionRepo) SearchExact(ctx context.Context, req contextstore.SearchRequest) ([]contextstore.SearchResult, error) {
	return r.SearchLexical(ctx, req)
}
func (r *querySelectionRepo) SearchLexical(ctx context.Context, req contextstore.SearchRequest) ([]contextstore.SearchResult, error) {
	r.calls++
	if r.delay > 0 {
		select {
		case <-time.After(r.delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if r.failAt == r.calls {
		return nil, errors.New("injected unavailable source")
	}
	return contextstore.CloneSearchResults(r.rows), nil
}

func querySelectionFixture() (ContextQueryPlan, []ContextQuerySource, contextstore.SearchResult) {
	plan := BuildContextQueryPlan(ContextRequest{Goal: "normal", Constraints: "hint", Phase: PhaseExecute, Trigger: ContextTriggerTaskDispatch}, ContextQueryHints{})
	row := contextstore.SearchResult{Item: contextstore.ContextItem{ID: "positive", Content: "hint positive", ContentHash: "positive-hash", Lifecycle: contextstore.LifecycleConfirmed, Scope: contextstore.Scope{ProjectID: "p", TeamID: "team"}}, Score: 1}
	var sources []ContextQuerySource
	for _, kind := range []ContextQuerySourceKind{QuerySharedPersistent, QueryPrivateSession, QueryPrivatePersistent} {
		primary := contextstore.SearchResult{Item: contextstore.ContextItem{ID: "legacy", ContentHash: "legacy-hash", Scope: row.Item.Scope, Metadata: map[string]string{"immutable": "yes"}}, Score: .01}
		sources = append(sources, ContextQuerySource{Kind: kind, Request: contextstore.SearchRequest{Scope: row.Item.Scope, Limit: 4}, Primary: []contextstore.SearchResult{primary}, Eligible: map[string]contextstore.ContextItem{row.Item.ID: row.Item}, Revision: "stable"})
	}
	return plan, sources, row
}

func TestContextQuerySelectionRollsBackAllScopesOnFailure(t *testing.T) {
	for _, test := range []struct {
		name     string
		delay    time.Duration
		fail     int
		revision bool
		reason   ContextQueryFallback
	}{
		{name: "search", fail: 2, reason: QueryFallbackSearchFailed},
		{name: "shared_deadline", delay: 60 * time.Millisecond, reason: QueryFallbackTimeout},
		{name: "revision", revision: true, reason: QueryFallbackRevisionChanged},
	} {
		t.Run(test.name, func(t *testing.T) {
			plan, sources, row := querySelectionFixture()
			repo := &querySelectionRepo{delay: test.delay, failAt: test.fail, rows: []contextstore.SearchResult{row}}
			revisionCalls := 0
			revision := func(context.Context, ContextQuerySource) (string, error) {
				revisionCalls++
				if test.revision && revisionCalls == 4 {
					return "changed", nil
				}
				return "stable", nil
			}
			started := time.Now()
			selection, err := SelectContextQuerySources(t.Context(), repo, plan, sources, revision)
			if err != nil {
				t.Fatal(err)
			}
			for index, result := range selection.Sources {
				if !reflect.DeepEqual(result.Results, sources[index].Primary) {
					t.Fatal("partial supplemental result survived rollback")
				}
			}
			for _, attribution := range selection.Attribution.Sources {
				if attribution.Mode != QueryModeLegacy || attribution.Fallback != test.reason || attribution.SupplementalHash != "" {
					t.Fatalf("false active attribution: %+v", attribution)
				}
			}
			if test.delay > 0 && (repo.calls != 2 || time.Since(started) > 300*time.Millisecond) {
				t.Fatalf("deadline was reset across scopes: calls=%d elapsed=%s", repo.calls, time.Since(started))
			}
		})
	}
}

func TestContextQuerySelectionOuterCancelAndRestrictedPrivateParity(t *testing.T) {
	plan, sources, row := querySelectionFixture()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	repo := &querySelectionRepo{rows: []contextstore.SearchResult{row}}
	if _, err := SelectContextQuerySources(ctx, repo, plan, sources, stableQueryRevision); !errors.Is(err, context.Canceled) || repo.calls != 0 {
		t.Fatal("outer cancellation continued retrieval")
	}
	sources[1].RestrictedLineage = true
	selection, err := SelectContextQuerySources(t.Context(), repo, plan, sources, stableQueryRevision)
	if err != nil {
		t.Fatal(err)
	}
	if repo.calls != 1 || selection.Attribution.Mode != QueryModeMixed {
		t.Fatalf("restricted lineage performed private supplemental I/O: %+v calls=%d", selection.Attribution, repo.calls)
	}
	for index := range []int{0, 1} {
		if !reflect.DeepEqual(selection.Sources[index+1].Results, sources[index+1].Primary) {
			t.Fatal("restricted ancestor changed a private bundle")
		}
	}
	selection.Sources[1].Results[0].Item.Metadata["immutable"] = "changed"
	if sources[1].Primary[0].Item.Metadata["immutable"] != "yes" {
		t.Fatal("arm metadata shared a mutable map")
	}
	if err := ValidateContextQueryAttribution(selection.Attribution); err != nil {
		t.Fatal(err)
	}
}

func TestContextQueryMergeRecallsSupplementWithoutGrowingK(t *testing.T) {
	var primary []contextstore.SearchResult
	for index := range 4 {
		primary = append(primary, contextstore.SearchResult{Item: contextstore.ContextItem{ID: strings.Repeat("x", index+1)}, Score: .1})
	}
	supplemental := []contextstore.SearchResult{{Item: contextstore.ContextItem{ID: "relevant"}, Score: 100}}
	merged := MergeContextQueryLanes(primary, supplemental, contextstore.Scope{}, 4)
	if len(merged) != 4 || merged[3].Item.ID != "relevant" || merged[3].Score != .4 || primary[0].Score != .1 {
		t.Fatalf("independent recall or immutable primary failed: %+v", merged)
	}
	shared := MergeContextQueryLanes(primary, primary[:1], contextstore.Scope{}, 4)
	if shared[0].Score != 1 {
		t.Fatal("two rank1 lanes did not normalize to one")
	}
}

func stableQueryRevision(context.Context, ContextQuerySource) (string, error) { return "stable", nil }

func TestContextQuerySelectionWithoutRevisionNeverReadsSupplement(t *testing.T) {
	plan, sources, row := querySelectionFixture()
	repo := &querySelectionRepo{rows: []contextstore.SearchResult{row}}
	selection, err := SelectContextQuerySources(t.Context(), repo, plan, sources, nil)
	if err != nil {
		t.Fatal(err)
	}
	if repo.calls != 0 || selection.Attribution.Mode != QueryModeLegacy {
		t.Fatal("unversioned source was queried or declared active")
	}
	for index, source := range selection.Sources {
		if !reflect.DeepEqual(source.Results, sources[index].Primary) || selection.Attribution.Sources[index].Fallback != QueryFallbackHintUnavailable {
			t.Fatal("unavailable revision did not retain exact primary")
		}
	}
}
