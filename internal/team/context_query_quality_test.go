package team

import (
	json "encoding/json/v2"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/kjelly/hufu/internal/agent"
	contextstore "github.com/kjelly/hufu/internal/context"
)

type contextQuerySignalCase struct {
	ID                   string   `json:"id"`
	Signal               string   `json:"signal"`
	Hint                 string   `json:"hint"`
	Content              string   `json:"content"`
	LegacyExpectedIDs    []string `json:"legacy_expected_ids"`
	CandidateExpectedIDs []string `json:"candidate_expected_ids"`
}

func TestContextQueryQualityFixesFourSignalMissesAtSameBudget(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "evals/context-routing/fixtures/query-signals-baseline.json"))
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		SchemaVersion int                      `json:"schema_version"`
		OracleSource  string                   `json:"oracle_source"`
		Cases         []contextQuerySignalCase `json:"cases"`
	}
	if err := json.Unmarshal(data, &fixture, json.RejectUnknownMembers(true)); err != nil {
		t.Fatal(err)
	}
	for _, test := range fixture.Cases {
		t.Run(test.ID, func(t *testing.T) {
			ctx := contextstore.WithEvaluationTime(t.Context(), time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC))
			repo, err := contextstore.OpenSQLite(filepath.Join(t.TempDir(), "context.sqlite"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = repo.Close() })
			scope := contextstore.Scope{ProjectID: "project", TeamID: "team"}
			items := []contextstore.ContextItem{{ID: test.ID, Content: test.Content, Kind: contextstore.ContextPattern, Scope: scope, Lifecycle: contextstore.LifecycleConfirmed, Confidence: 1, TrustLevel: contextstore.TrustTrusted}}
			for index := range 8 {
				items = append(items, contextstore.ContextItem{ID: fmt.Sprintf("noise-%d", index), Content: fmt.Sprintf("inspect router distractor %d", index), Kind: contextstore.ContextPattern, Scope: scope, Lifecycle: contextstore.LifecycleConfirmed, Confidence: 1, TrustLevel: contextstore.TrustTrusted})
			}
			if err := repo.Append(ctx, items...); err != nil {
				t.Fatal(err)
			}
			eligible, err := repo.QuerySharedPersistentProjection(ctx, scope)
			if err != nil {
				t.Fatal(err)
			}
			request := ContextRequest{Goal: "inspect router", Phase: PhaseExecute, Trigger: ContextTriggerTaskDispatch, AgentRole: "worker"}
			hints := ContextQueryHints{}
			switch test.Signal {
			case "constraints":
				request.Constraints = test.Hint
			case "verification":
				request.VerificationCriteria = test.Hint
			case "paths":
				hints.Paths = []string{test.Hint}
				hints.PathSourceID = "workset:item"
				hints.PathSourceHash = contextQueryDigest("bound fixture")
			case "dependency":
				hints.Dependencies = []ContextQueryDependencyHint{{TaskID: "accepted", Summary: test.Hint}}
			default:
				t.Fatal("invalid fixed signal fixture")
			}
			plan := BuildContextQueryPlan(request, hints)
			ranking := defaultMemoryRuntimeRankingPolicy()
			comparison, err := CompareContextQueryRanking(ctx, repo, repo, request, plan, scope, eligible, agent.DefaultMemoryLearningPolicy(), ranking)
			if err != nil {
				t.Fatal(err)
			}
			hasTarget := func(items []contextstore.ContextItem) bool {
				return slices.ContainsFunc(items, func(item contextstore.ContextItem) bool { return item.ID == test.ID })
			}
			if hasTarget(comparison.Legacy.Selected) || !hasTarget(comparison.Candidate.Selected) {
				t.Fatalf("legacy miss not fixed: legacy=%v candidate=%v", queryQualityIDs(comparison.Legacy.Selected), queryQualityIDs(comparison.Candidate.Selected))
			}
			if len(comparison.Candidate.Selected) > ranking.InjectTopK || len(comparison.Candidate.Candidates) > ranking.CandidateTopK {
				t.Fatal("quality improved by increasing K")
			}
			var irrelevantTokens []int
			for _, arm := range []ContextQueryRankingArm{comparison.Legacy, comparison.Candidate} {
				compiled, err := CompileWorkerContext(ctx, WorkerContextInput{Goal: request.Goal, CanonicalMemory: &CanonicalContextBundle{SharedPersistent: arm.Selected, SharedPersistentScores: arm.Scores, SharedPersistentFinalScores: arm.FinalScores, SharedPersistentAggregates: arm.Aggregates}, ModelContext: ModelContextSpec{ContextWindow: 8192, MaxOutputTokens: 128, SafetyMarginTokens: 32}})
				if err != nil {
					t.Fatal(err)
				}
				noise := 0
				for _, item := range compiled.IncludedItems {
					if item.Source == "shared_persistent" && item.ID != "context:"+test.ID {
						noise += item.TokenCount
					}
				}
				irrelevantTokens = append(irrelevantTokens, noise)
			}
			if irrelevantTokens[1] > irrelevantTokens[0] {
				t.Fatalf("irrelevant token cost grew: %v", irrelevantTokens)
			}
			work := comparison.Selection.Sources[0].Work
			if work.VectorCalls != 0 || work.ExactCalls > 8 || work.LexicalCalls > 1 || work.CandidateRows > 64 {
				t.Fatalf("unbounded quality cost: %+v", work)
			}
			t.Logf("legacy_recall=0 candidate_recall=1 inject_k=%d candidate_k=%d irrelevant_tokens=%v supplemental_calls=%d rows=%d", ranking.InjectTopK, ranking.CandidateTopK, irrelevantTokens, work.ExactCalls+work.LexicalCalls, work.CandidateRows)
		})
	}
}

func queryQualityIDs(items []contextstore.ContextItem) []string {
	ids := make([]string, len(items))
	for index, item := range items {
		ids[index] = item.ID
	}
	return ids
}

func TestContextQueryComparisonFallbackKeepsRankingAndWritesNothing(t *testing.T) {
	_, repo := rankingTestCoordinator(t, agent.MemoryLearningActive)
	ctx := t.Context()
	scope := contextstore.Scope{ProjectID: "project", TeamID: "team"}
	eligible, err := repo.QuerySharedPersistentProjection(ctx, scope)
	if err != nil {
		t.Fatal(err)
	}
	beforeRevision, err := repo.Revision(ctx)
	if err != nil {
		t.Fatal(err)
	}
	learning := agent.DefaultMemoryLearningPolicy()
	learning.Mode = agent.MemoryLearningActive
	for _, test := range []struct {
		name   string
		hint   string
		reason ContextQueryFallback
	}{
		{"no_hints", "", QueryFallbackNoHints},
		{"no_matches", "nonexistent migration sentinel", QueryFallbackNoMatches},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := ContextRequest{Goal: "procedure", Constraints: test.hint, Phase: PhaseExecute, Trigger: ContextTriggerTaskDispatch}
			comparison, err := CompareContextQueryRanking(ctx, repo, repo, request, BuildContextQueryPlan(request, ContextQueryHints{}), scope, eligible, learning, defaultMemoryRuntimeRankingPolicy())
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(comparison.Legacy, comparison.Candidate) || comparison.Selection.Attribution.Sources[0].Fallback != test.reason {
				t.Fatal("fallback changed legacy scores, selection or aggregates")
			}
			if len(comparison.Legacy.Selected) == 0 {
				t.Fatal("positive primary control did not retrieve")
			}
		})
	}
	afterRevision, err := repo.Revision(ctx)
	if err != nil || beforeRevision != afterRevision {
		t.Fatal("pure comparison changed canonical memory revision")
	}
	aggregates, err := repo.ListExperienceAggregates(ctx, learning.PolicyVersion)
	if err != nil || len(aggregates) != 0 {
		t.Fatal("pure comparison created exposure or outcome credit")
	}
}

func TestContextQuerySupplementalRankingRetainsHarmfulAndStalePenalties(t *testing.T) {
	c, repo := rankingTestCoordinator(t, agent.MemoryLearningActive)
	ctx := t.Context()
	safe, harmful, stale := rankingItem("safe-supplement", 1), rankingItem("harmful-supplement", 100), rankingItem("stale-supplement", 100)
	for _, item := range []*contextstore.ContextItem{&safe, &harmful, &stale} {
		item.Content = "migration sentinel " + item.ID
	}
	stale.Metadata = map[string]string{"stale_environment": "true"}
	if err := repo.Append(ctx, safe, harmful, stale); err != nil {
		t.Fatal(err)
	}
	learning := c.session.Config.MemoryLearning
	if _, err := repo.ApplyExperienceObservation(ctx, contextstore.ExperienceObservation{IdempotencyKey: "harmful-supplement", ContextItemID: harmful.ID, PolicyVersion: learning.PolicyVersion, TaskID: "failed-source", NegativeWeight: 1, CausalFailureDelta: 1}); err != nil {
		t.Fatal(err)
	}
	scope := contextstore.Scope{ProjectID: "project", TeamID: "team"}
	eligible, err := repo.QuerySharedPersistentProjection(ctx, scope)
	if err != nil {
		t.Fatal(err)
	}
	request := ContextRequest{Goal: "procedure", Constraints: "migration sentinel", Phase: PhaseExecute, Trigger: ContextTriggerTaskDispatch}
	comparison, err := CompareContextQueryRanking(ctx, repo, repo, request, BuildContextQueryPlan(request, ContextQueryHints{}), scope, eligible, learning, defaultMemoryRuntimeRankingPolicy())
	if err != nil {
		t.Fatal(err)
	}
	ids := queryQualityIDs(comparison.Candidate.Selected)
	if !slices.Contains(ids, safe.ID) || slices.Contains(ids, harmful.ID) || slices.Contains(ids, stale.ID) {
		t.Fatalf("supplement bypassed existing safety penalties: selected=%v", ids)
	}
	parts, ok := comparison.Candidate.Scores[harmful.ID]
	if !ok || parts.HarmfulUsePenalty == 0 || comparison.Candidate.Aggregates[harmful.ID] == nil {
		t.Fatal("supplement dropped existing reinforcement evidence")
	}
}
