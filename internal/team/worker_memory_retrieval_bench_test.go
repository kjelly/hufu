package team

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/kjelly/hufu/internal/agent"
	contextstore "github.com/kjelly/hufu/internal/context"
)

func BenchmarkWorkerAncestorRecall(b *testing.B) {
	for _, count := range []int{1_000, 10_000, 100_000, 100_002} {
		b.Run(fmt.Sprintf("rows=%d", count), func(b *testing.B) {
			repo, scope, allowed := workerAncestorRetrievalFixture(b, count, false)
			scope.BranchID = "child"
			req := WorkerMemoryRecallRequest{WorkerID: "worker", Scope: scope, Query: `"anchor" migration`,
				Policy:         agent.WorkerMemoryPolicy{Mode: agent.WorkerMemorySession, MaxItems: 8, MaxTokens: 5000},
				SessionLineage: []WorkerMemoryLineageBranch{{BranchID: "main", RestrictItems: true, AllowedItemIDs: allowed}}}
			svc := NewWorkerMemoryService(repo, nil)
			warm, err := svc.Recall(b.Context(), req)
			if err != nil || len(warm.Items) != len(allowed) {
				b.Fatalf("warm recall: items=%d err=%v", len(warm.Items), err)
			}
			for sample := range 5 {
				b.Run(fmt.Sprintf("sample=%d", sample), func(b *testing.B) {
					var work contextstore.QueryWork
					ctx := contextstore.WithQueryWorkObserver(b.Context(), func(_ context.Context, observed contextstore.QueryWork) { work = observed })
					b.ReportAllocs()
					for b.Loop() {
						bundle, err := svc.Recall(ctx, req)
						if err != nil {
							b.Fatal(err)
						}
						if !slices.Equal(bundle.Trace.ItemIDs, warm.Trace.ItemIDs) {
							b.Fatal("recall order changed after warmup")
						}
					}
					b.ReportMetric(float64(work.CandidateRows), "source_rows/op")
					b.ReportMetric(float64(len(warm.Items)), "items_returned/op")
					b.Logf("ordered IDs: %v", warm.Trace.ItemIDs)
				})
			}

		})
	}
}

func workerAncestorRetrievalFixture(tb testing.TB, count int, allMatch bool) (*contextstore.SQLiteRepository, contextstore.Scope, map[string]bool) {
	tb.Helper()
	repo, err := contextstore.OpenSQLite(filepath.Join(tb.TempDir(), "context.sqlite"))
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { _ = repo.Close() })
	scope := contextstore.Scope{ProjectID: "p", TeamID: "team", SessionID: "sess", BranchID: "main", AgentID: "worker"}
	allowed := map[string]bool{}
	seedContext := contextstore.WithEvaluationTime(tb.Context(), time.Unix(200_000, 0))
	for start := 0; start < count; start += 500 {
		items := make([]contextstore.ContextItem, 0, min(500, count-start))
		for i := start; i < min(count, start+500); i++ {
			id := fmt.Sprintf("item-%06d", i)
			content := "unrelated background " + id
			if allMatch || i < 16 {
				content = "anchor migration " + id
			}
			if i < 8 {
				allowed[id] = true
			}
			items = append(items, contextstore.ContextItem{ID: id, Kind: contextstore.ContextObservation, Content: content, Scope: scope, CreatedAt: time.Unix(int64(i+1), 0), UpdatedAt: time.Unix(int64(i+1), 0)})
		}
		if err := repo.Append(seedContext, items...); err != nil {
			tb.Fatal(err)
		}
	}
	return repo, scope, allowed
}
