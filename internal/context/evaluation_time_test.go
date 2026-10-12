package context

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestEvaluationTimeControlsCanonicalEligibilityAndSeedTimestamps(t *testing.T) {
	repo, err := OpenSQLite(filepath.Join(t.TempDir(), "context.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	instant := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	expires := instant.Add(time.Hour)
	ctx := WithEvaluationTime(t.Context(), instant)
	scope := Scope{ProjectID: "project", TeamID: "team"}
	if err := repo.Append(ctx, ContextItem{ID: "eligible", Content: "clock sentinel", Kind: ContextPattern, Scope: scope, Confidence: 1, ExpiresAt: &expires}); err != nil {
		t.Fatal(err)
	}
	item, err := repo.Get(ctx, "eligible")
	if err != nil || !item.CreatedAt.Equal(instant) || !item.UpdatedAt.Equal(instant) {
		t.Fatalf("seed timestamps = %v/%v, error %v", item.CreatedAt, item.UpdatedAt, err)
	}
	for name, search := range map[string]func(context.Context, SearchRequest) ([]SearchResult, error){"exact": repo.SearchExact, "lexical": repo.SearchLexical} {
		t.Run(name, func(t *testing.T) {
			found, err := search(ctx, SearchRequest{Query: "sentinel", Scope: scope})
			if err != nil || len(found) != 1 {
				t.Fatalf("replay search = %v, error %v", found, err)
			}
			found, err = search(WithEvaluationTime(t.Context(), expires), SearchRequest{Query: "sentinel", Scope: scope})
			if err != nil || len(found) != 0 {
				t.Fatalf("expiry boundary search = %v, error %v", found, err)
			}
		})
	}
	rows, err := repo.Query(ctx, RepositoryQuery{Scope: scope})
	if err != nil || len(rows) != 1 {
		t.Fatalf("projection query = %v, error %v", rows, err)
	}
	rows, err = repo.Query(WithEvaluationTime(t.Context(), expires), RepositoryQuery{Scope: scope})
	if err != nil || len(rows) != 0 {
		t.Fatalf("expired projection query = %v, error %v", rows, err)
	}
}

func TestEvaluationTimeDoesNotChangeDeadlineOrParentContext(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	deadline, _ := ctx.Deadline()
	instant := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	fixed := WithEvaluationTime(ctx, instant)
	got, ok := fixed.Deadline()
	if !ok || got != deadline || fixed.Done() != ctx.Done() || EvaluationTime(fixed) != instant {
		t.Fatal("evaluation time changed the process deadline or was not inherited")
	}
	if EvaluationTime(ctx).Year() == 2000 {
		t.Fatal("evaluation clock leaked into the parent context")
	}
	cancel()
	if fixed.Err() != context.Canceled {
		t.Fatalf("cancellation = %v", fixed.Err())
	}
}
