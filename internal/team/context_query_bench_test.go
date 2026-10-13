package team

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/kjelly/hufu/internal/agent"
	contextstore "github.com/kjelly/hufu/internal/context"
)

// BenchmarkContextQueryRanking measures ranking only. Canonical seed and store
// creation are outside the timer. Primary has eight matching distractors
// regardless of store size; the separate supplemental benchmark matches all
// rows. This does not characterize the unbounded all-matching legacy MMR cost; the candidate arm includes primary ranking,
// source snapshot hashing, supplemental retrieval and the existing reranker.
func BenchmarkContextQueryRanking(b *testing.B) {
	for _, count := range []int{10, 1000, 10000} {
		b.Run(fmt.Sprintf("rows-%d", count), func(b *testing.B) {
			ctx, repo, scope, eligible := queryBenchmarkStore(b, count, false)
			request := ContextRequest{Goal: "inspect router", Constraints: "migration sentinel", Phase: PhaseExecute, Trigger: ContextTriggerTaskDispatch}
			plan := BuildContextQueryPlan(request, ContextQueryHints{})
			ranking, learning := defaultMemoryRuntimeRankingPolicy(), agent.DefaultMemoryLearningPolicy()
			for _, mode := range []string{"legacy", "candidate"} {
				b.Run(mode, func(b *testing.B) {
					samples := make([]float64, 0)
					var total contextstore.QueryWork
					var active, fallbacks int
					observed := contextstore.WithQueryWorkObserver(ctx, func(_ context.Context, work contextstore.QueryWork) {
						total.ExactCalls += work.ExactCalls
						total.LexicalCalls += work.LexicalCalls
						total.VectorCalls += work.VectorCalls
						total.CandidateRows += work.CandidateRows
					})
					b.ReportAllocs()
					for b.Loop() {
						started := time.Now()
						if mode == "legacy" {
							allowed := make(map[string]bool, len(eligible))
							for _, item := range eligible {
								allowed[item.ID] = true
							}
							_, err := rankPersistentMemory(observed, repo, repo, persistentRankingInput{Query: request.RetrievalQuery(), RequestScope: scope, Base: eligible, Allowed: allowed, Learning: learning, Ranking: ranking})
							if err != nil {
								b.Fatal(err)
							}
						} else {
							comparison, err := CompareContextQueryRanking(observed, repo, repo, request, plan, scope, eligible, learning, ranking)
							if err != nil {
								b.Fatal(err)
							}
							if comparison.Selection.Attribution.Mode == QueryModeActive {
								active++
							} else {
								fallbacks++
							}
							if len(comparison.Candidate.Selected) > ranking.InjectTopK || comparison.Selection.Sources[0].Work.CandidateRows > 64 {
								b.Fatal("query bounds violated")
							}
						}
						samples = append(samples, float64(time.Since(started).Nanoseconds())/1e6)
					}
					reportQueryBenchmark(b, samples, total)
					b.ReportMetric(float64(active)/float64(len(samples)), "active/op")
					b.ReportMetric(float64(fallbacks)/float64(len(samples)), "fallback/op")
				})
			}
		})
	}
}

// BenchmarkContextQuerySupplementalScopes measures ONE staged transaction
// across shared, active private session and private persistent sources. Strong
// matching distractors exercise the source-row allowance and SQL allow-set.
func BenchmarkContextQuerySupplementalScopes(b *testing.B) {
	for _, count := range []int{10, 1000, 10000} {
		b.Run(fmt.Sprintf("rows-%d", count), func(b *testing.B) {
			ctx, repo, scope, eligible := queryBenchmarkStore(b, count, true)
			revision, err := repo.Revision(ctx)
			if err != nil {
				b.Fatal(err)
			}
			plan := BuildContextQueryPlan(ContextRequest{Goal: "inspect router", Constraints: "migration sentinel", Phase: PhaseExecute, Trigger: ContextTriggerTaskDispatch}, ContextQueryHints{})
			scopes := []contextstore.Scope{scope, {ProjectID: scope.ProjectID, TeamID: scope.TeamID, SessionID: "session", BranchID: "main", AgentID: "worker"}, {ProjectID: scope.ProjectID, TeamID: scope.TeamID, AgentID: "worker"}}
			kinds := []ContextQuerySourceKind{QuerySharedPersistent, QueryPrivateSession, QueryPrivatePersistent}
			sources := make([]ContextQuerySource, 0, 3)
			for index, sourceScope := range scopes {
				canonical := make(map[string]contextstore.ContextItem)
				for _, item := range eligible {
					if item.Scope == sourceScope {
						canonical[item.ID] = item
					}
				}
				sources = append(sources, ContextQuerySource{Kind: kinds[index], Request: contextstore.SearchRequest{Scope: sourceScope, Limit: 20}, Eligible: canonical, Revision: strconv.FormatInt(revision, 10)})
			}
			readRevision := func(ctx context.Context, _ ContextQuerySource) (string, error) {
				current, err := repo.Revision(ctx)
				return strconv.FormatInt(current, 10), err
			}
			samples := make([]float64, 0)
			var total contextstore.QueryWork
			var active, fallbacks int
			b.ReportAllocs()
			for b.Loop() {
				started := time.Now()
				selection, err := SelectContextQuerySources(ctx, repo, plan, sources, readRevision)
				if err != nil {
					b.Fatal(err)
				}
				if selection.Attribution.Mode == QueryModeLegacy {
					fallbacks++
				} else {
					active++
				}
				for _, source := range selection.Sources {
					if source.Work.ExactCalls > 8 || source.Work.LexicalCalls > 1 || source.Work.CandidateRows > 64 || source.Work.VectorCalls != 0 {
						b.Fatal("supplemental bounds violated")
					}
					total.ExactCalls += source.Work.ExactCalls
					total.LexicalCalls += source.Work.LexicalCalls
					total.CandidateRows += source.Work.CandidateRows
				}
				samples = append(samples, float64(time.Since(started).Nanoseconds())/1e6)
			}
			reportQueryBenchmark(b, samples, total)
			b.ReportMetric(float64(active)/float64(len(samples)), "active/op")
			b.ReportMetric(float64(fallbacks)/float64(len(samples)), "fallback/op")
		})
	}
	b.Run("shared-deadline", func(b *testing.B) {
		plan, sources, row := querySelectionFixture()
		repo := &querySelectionRepo{delay: 60 * time.Millisecond, rows: []contextstore.SearchResult{row}}
		revision := func(context.Context, ContextQuerySource) (string, error) { return "stable", nil }
		samples := make([]float64, 0)
		var total contextstore.QueryWork
		var active, fallbacks int
		b.ReportAllocs()
		for b.Loop() {
			started := time.Now()
			selection, err := SelectContextQuerySources(b.Context(), repo, plan, sources, revision)
			if err != nil {
				b.Fatal(err)
			}
			if selection.Attribution.Mode != QueryModeLegacy {
				b.Fatal("partial selection survived timeout")
			}
			if selection.Attribution.Mode == QueryModeLegacy {
				fallbacks++
			} else {
				active++
			}
			for _, source := range selection.Sources {
				total.LexicalCalls += source.Work.LexicalCalls
				total.CandidateRows += source.Work.CandidateRows
			}
			samples = append(samples, float64(time.Since(started).Nanoseconds())/1e6)
		}
		reportQueryBenchmark(b, samples, total)
		b.ReportMetric(float64(active)/float64(len(samples)), "active/op")
		b.ReportMetric(float64(fallbacks)/float64(len(samples)), "fallback/op")
	})
}

func queryBenchmarkStore(b *testing.B, count int, private bool) (context.Context, *contextstore.SQLiteRepository, contextstore.Scope, []contextstore.ContextItem) {
	b.Helper()
	ctx := contextstore.WithEvaluationTime(b.Context(), time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC))
	repo, err := contextstore.OpenSQLite(filepath.Join(b.TempDir(), "context.sqlite"))
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = repo.Close() })
	scope := contextstore.Scope{ProjectID: "project", TeamID: "team"}
	items := make([]contextstore.ContextItem, 0, count)
	for index := range count {
		itemScope := scope
		content := fmt.Sprintf("unrelated canonical memory %d", index)
		if index < 9 {
			content = fmt.Sprintf("inspect router distractor %d", index)
		}
		if index == 0 {
			content = "migration sentinel relevant shared memory"
		}
		if private {
			content = fmt.Sprintf("migration sentinel strong distractor %d", index)
			switch index % 3 {
			case 1:
				itemScope.SessionID, itemScope.BranchID, itemScope.AgentID = "session", "main", "worker"
			case 2:
				itemScope.AgentID = "worker"
			}
		}
		items = append(items, contextstore.ContextItem{ID: fmt.Sprintf("row-%05d", index), Content: content, Kind: contextstore.ContextPattern, Scope: itemScope, Lifecycle: contextstore.LifecycleConfirmed, Confidence: 1, TrustLevel: contextstore.TrustTrusted})
	}
	if err := repo.Append(ctx, items...); err != nil {
		b.Fatal(err)
	}
	eligible, err := repo.Query(ctx, contextstore.RepositoryQuery{Scope: contextstore.Scope{ProjectID: scope.ProjectID, TeamID: scope.TeamID}, Visibility: contextstore.VisibilitySubtree, Limit: count})
	if err != nil {
		b.Fatal(err)
	}
	if len(eligible) != count {
		b.Fatalf("benchmark requires %d actual canonical rows, got %d", count, len(eligible))
	}
	return ctx, repo, scope, eligible
}

func reportQueryBenchmark(b *testing.B, samples []float64, total contextstore.QueryWork) {
	b.Helper()
	b.StopTimer()
	slices.Sort(samples)
	n := len(samples)
	if n == 0 {
		b.Fatal("no measured query samples")
	}
	b.ReportMetric(samples[(n-1)/2], "p50-ms")
	b.ReportMetric(samples[(95*n+99)/100-1], "p95-ms")
	b.ReportMetric(float64(total.ExactCalls)/float64(n), "exact/op")
	b.ReportMetric(float64(total.LexicalCalls)/float64(n), "lexical/op")
	b.ReportMetric(float64(total.VectorCalls)/float64(n), "vector/op")
	b.ReportMetric(float64(total.CandidateRows)/float64(n), "source-rows/op")
}
