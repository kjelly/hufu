package team

import (
	"slices"
	"strings"
	"testing"

	"github.com/kjelly/hufu/internal/agent"
	contextstore "github.com/kjelly/hufu/internal/context"
	"github.com/kjelly/hufu/internal/semantic"
)

const semanticReplayChineseGoal = "協調者恢復後要避免重送副作用"

func openTestSemanticReplay(t *testing.T, fake *conceptOllama, repo *contextstore.SQLiteRepository, mode semantic.Mode) *SemanticReplay {
	t.Helper()
	replay, err := OpenSemanticReplay(t.Context(), conceptSettings(t, fake, mode, nil), repo, "project")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(replay.Close)
	return replay
}

func TestExplainMemoryMatchesRuntimeSemanticSelection(t *testing.T) {
	c, repo, fake := semanticRouterFixture(t)
	adoptExplainTestPolicy(t, c, repo, agent.MemoryLearningOff, nil)
	attachSemanticRuntime(t, c, repo, fake, semantic.ModeAuto)
	base, err := repo.QuerySharedPersistentProjection(t.Context(), c.contextScope())
	if err != nil {
		t.Fatal(err)
	}
	allowed := map[string]bool{}
	for _, item := range base {
		allowed[item.ID] = true
	}
	replay := openTestSemanticReplay(t, fake, repo, semantic.ModeAuto)
	tests := []struct {
		goal      string
		wantRoute string
	}{
		{goal: semanticRouteGoal, wantRoute: contextstore.SemanticRouteLexical},
		{goal: semanticReplayChineseGoal, wantRoute: contextstore.SemanticRouteSemantic},
	}
	for _, tc := range tests {
		selected, _, _, _, _, err := c.rankSharedPersistentMemoryWithLane(t.Context(), tc.goal, tc.goal, base, allowed, c.persistentSemanticLane(SemanticTraceAttribution{}))
		if err != nil {
			t.Fatal(err)
		}
		runtimeSelected := map[string]bool{}
		for _, item := range selected {
			runtimeSelected[item.ID] = true
		}
		for _, item := range base {
			got, err := ExplainPersistentMemory(t.Context(), repo, MemoryExplainInput{Workspace: c.session.Workspace, ItemID: item.ID, ProjectID: "project", TeamID: "team", Query: tc.goal, Semantic: replay})
			if err != nil {
				t.Fatal(err)
			}
			if got.Ranking.Selected != runtimeSelected[item.ID] {
				t.Fatalf("goal %q item %s: explain selected=%v, runtime selected=%v (reasons %v)", tc.goal, item.ID, got.Ranking.Selected, runtimeSelected[item.ID], got.Reasons)
			}
			// The generation is read only when a request embeds; a lexical route
			// never touches the index.
			if got.Semantic == nil || got.Semantic.Route != tc.wantRoute || got.Semantic.Mode != semantic.ModeAuto || got.Semantic.State != "ready" ||
				(tc.wantRoute == contextstore.SemanticRouteSemantic) != (got.Semantic.GenerationID != "") {
				t.Fatalf("goal %q item %s: semantic = %+v", tc.goal, item.ID, got.Semantic)
			}
		}
		if tc.wantRoute == contextstore.SemanticRouteSemantic && !runtimeSelected["zh-receipt"] {
			t.Fatalf("runtime did not select the Chinese memory for %q: %v", tc.goal, selected)
		}
	}
	explained, err := ExplainPersistentMemory(t.Context(), repo, MemoryExplainInput{Workspace: c.session.Workspace, ItemID: "zh-receipt", ProjectID: "project", TeamID: "team", Query: semanticReplayChineseGoal, Semantic: replay})
	if err != nil {
		t.Fatal(err)
	}
	if explained.Fusion != contextstore.FusionScoreNormalized || explained.Semantic.FallbackReason != contextstore.SemanticFallbackNone || explained.Retrieval.Candidate == nil || explained.Retrieval.Candidate.VectorRank != 1 {
		t.Fatalf("semantic explanation = fusion %q semantic %+v candidate %+v", explained.Fusion, explained.Semantic, explained.Retrieval.Candidate)
	}
}

func TestSemanticReplayIsReadOnly(t *testing.T) {
	c, repo, fake := semanticRouterFixture(t)
	adoptExplainTestPolicy(t, c, repo, agent.MemoryLearningOff, nil)
	runtime := attachSemanticRuntime(t, c, repo, fake, semantic.ModeActive)
	runtime.Close()
	before, err := repo.ListGenerations(t.Context(), "project")
	if err != nil {
		t.Fatal(err)
	}
	replay := openTestSemanticReplay(t, fake, repo, semantic.ModeActive)
	// A memory written after the last build stays uncovered: the replay
	// reports the fallback and never refreshes the index.
	if err := repo.Append(t.Context(), semanticItem("zh-new", "新的收據規則", contextstore.Scope{ProjectID: "project", TeamID: "team"})); err != nil {
		t.Fatal(err)
	}
	got, err := ExplainPersistentMemory(t.Context(), repo, MemoryExplainInput{Workspace: c.session.Workspace, ItemID: "zh-receipt", ProjectID: "project", TeamID: "team", Query: semanticReplayChineseGoal, Semantic: replay})
	if err != nil {
		t.Fatal(err)
	}
	if got.Semantic == nil || got.Semantic.FallbackReason != contextstore.SemanticFallbackProjectionIncomplete {
		t.Fatalf("semantic = %+v", got.Semantic)
	}
	after, err := repo.ListGenerations(t.Context(), "project")
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) || replay.Status().Refreshes != 0 {
		t.Fatalf("replay refreshed the index: generations %d -> %d, refreshes %d", len(before), len(after), replay.Status().Refreshes)
	}
}

func TestReplayPersistentRankingComparesSemanticModes(t *testing.T) {
	c, repo, fake := semanticRouterFixture(t)
	adoptExplainTestPolicy(t, c, repo, agent.MemoryLearningOff, nil)
	attachSemanticRuntime(t, c, repo, fake, semantic.ModeActive).Close()
	replay := openTestSemanticReplay(t, fake, repo, semantic.ModeActive)
	queries := fake.queryCount()
	report, err := ReplayPersistentRanking(t.Context(), repo, RankingReplayInput{
		ProjectID: "project", TeamID: "team", Queries: []RankingReplayQuery{{Text: semanticRouteGoal, Source: "flag"}},
		Semantic: replay, BaselineSemanticMode: semantic.ModeOff, CandidateSemanticMode: semantic.ModeActive,
	})
	if err != nil {
		t.Fatal(err)
	}
	if embedded := fake.queryCount() - queries; embedded != 1 {
		t.Fatalf("replay embedded the query %d times, want 1 (memoized across rankers)", embedded)
	}
	query := report.Queries[0]
	if slices.Contains(query.Relevance.BaselineSelected, "zh-receipt") || !slices.Contains(query.Relevance.CandidateSelected, "zh-receipt") {
		t.Fatalf("relevance comparison = %+v", query.Relevance)
	}
	if query.BaselineRoute != "" || query.CandidateRoute != contextstore.SemanticRouteSemantic || query.CandidateFallback != contextstore.SemanticFallbackNone {
		t.Fatalf("query routes = %+v", query)
	}
	semanticSummary := report.Semantic
	if semanticSummary == nil || semanticSummary.Mode != semantic.ModeOff || semanticSummary.CandidateMode != semantic.ModeActive ||
		semanticSummary.CandidateRoutes[contextstore.SemanticRouteSemantic] != 1 || len(semanticSummary.BaselineRoutes) != 0 ||
		semanticSummary.Fusion != "" || semanticSummary.CandidateFusion != contextstore.FusionScoreNormalized ||
		report.CandidateFusion != report.BaselineFusion {
		t.Fatalf("semantic summary = %+v report fusions %q/%q", semanticSummary, report.BaselineFusion, report.CandidateFusion)
	}

	_, err = ReplayPersistentRanking(t.Context(), repo, RankingReplayInput{
		ProjectID: "project", TeamID: "team", Queries: []RankingReplayQuery{{Text: "x"}}, Semantic: replay,
	})
	if err == nil || !strings.Contains(err.Error(), "differs from the baseline") {
		t.Fatalf("identical candidate err = %v", err)
	}
}

func TestOpenSemanticReplayOffIsNil(t *testing.T) {
	repo := openSemanticTestRepo(t)
	replay, err := OpenSemanticReplay(t.Context(), conceptSettings(t, newConceptOllama(t), semantic.ModeOff, nil), repo, "project")
	if err != nil || replay != nil {
		t.Fatalf("off replay = %v, %v", replay, err)
	}
	replay.Close()
	if replay.lane(semantic.ModeActive, "") != nil {
		t.Fatal("nil replay returned a lane")
	}
}
