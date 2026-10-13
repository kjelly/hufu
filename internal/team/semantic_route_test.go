package team

import (
	"slices"
	"testing"

	"github.com/kjelly/hufu/internal/agent"
	contextstore "github.com/kjelly/hufu/internal/context"
	"github.com/kjelly/hufu/internal/semantic"
)

// stubSemanticRouter answers a fixed route and records the routed texts.
type stubSemanticRouter struct {
	semantic bool
	texts    []string
}

func (r *stubSemanticRouter) RouteSemantic(text string) bool {
	r.texts = append(r.texts, text)
	return r.semantic
}

func (f *conceptOllama) queryCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.queryInputs)
}

func TestResolveSemanticRoute(t *testing.T) {
	const (
		legacy = contextstore.FusionLegacy
		score  = contextstore.FusionScoreNormalized
		rrf    = contextstore.FusionRRFNormalized
	)
	tests := []struct {
		name       string
		mode       contextstore.RetrievalMode
		router     *stubSemanticRouter
		laneFusion contextstore.FusionMode
		want       semanticRequestRoute
	}{
		{name: "active uses lane fusion", mode: contextstore.RetrievalActive, laneFusion: score, want: semanticRequestRoute{Mode: contextstore.RetrievalActive, Fusion: score}},
		{name: "active without lane fusion keeps policy", mode: contextstore.RetrievalActive, want: semanticRequestRoute{Mode: contextstore.RetrievalActive, Fusion: legacy}},
		{name: "shadow keeps policy fusion", mode: contextstore.RetrievalShadow, laneFusion: score, want: semanticRequestRoute{Mode: contextstore.RetrievalShadow, Fusion: legacy}},
		{name: "auto routed semantic", mode: contextstore.RetrievalActive, router: &stubSemanticRouter{semantic: true}, laneFusion: rrf, want: semanticRequestRoute{Mode: contextstore.RetrievalActive, Fusion: rrf}},
		{name: "auto routed lexical", mode: contextstore.RetrievalActive, router: &stubSemanticRouter{}, laneFusion: score, want: semanticRequestRoute{Mode: contextstore.RetrievalOff, Fusion: legacy}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var router semanticRouter
			if tc.router != nil {
				router = tc.router
			}
			if got := resolveSemanticRoute(tc.mode, router, tc.laneFusion, legacy, "goal"); got != tc.want {
				t.Fatalf("route = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestPersistentRankingRoutesOnSemanticQuery(t *testing.T) {
	router := &stubSemanticRouter{}
	lane := &persistentSemanticLane{Vector: &trackingVectorSearcher{}, Mode: contextstore.RetrievalActive, Router: router, Fusion: contextstore.FusionScoreNormalized}
	in := persistentRankingInput{Query: "goal\nphase: execute", SemanticQuery: "goal", Semantic: lane, Ranking: MemoryRuntimeRankingPolicy{Fusion: contextstore.FusionLegacy}}
	if got, fusion := in.requestLane(); got != nil || fusion != contextstore.FusionLegacy {
		t.Fatalf("lexical route = %+v %q", got, fusion)
	}
	router.semantic = true
	got, fusion := in.requestLane()
	if got == nil || got.Mode != contextstore.RetrievalActive || fusion != contextstore.FusionScoreNormalized {
		t.Fatalf("semantic route = %+v %q", got, fusion)
	}
	if !slices.Equal(router.texts, []string{"goal", "goal"}) {
		t.Fatalf("routed texts = %q, want the semantic query", router.texts)
	}
}

func TestContextRouterAutoRoutesByQueryScript(t *testing.T) {
	const chineseGoal = "協調者恢復後要避免重送副作用"
	c, repo, fake := semanticRouterFixture(t)
	offEnglish, _ := routeSharedIDs(t, c)
	offChinese, _ := routeSharedIDsForGoal(t, c, chineseGoal)
	if slices.Contains(offChinese, "zh-receipt") {
		t.Fatalf("lexical retrieval reached the Chinese memory from a Chinese goal: %v", offChinese)
	}

	runtime := attachSemanticRuntime(t, c, repo, fake, semantic.ModeAuto)
	queries := fake.queryCount()
	englishIDs, englishSemantic := routeSharedIDs(t, c)
	if !slices.Equal(englishIDs, offEnglish) || englishSemantic != nil || fake.queryCount() != queries {
		t.Fatalf("english goal: ids=%v (off %v) semantic=%+v query embeddings=%d", englishIDs, offEnglish, englishSemantic, fake.queryCount()-queries)
	}
	chineseIDs, chineseSemantic := routeSharedIDsForGoal(t, c, chineseGoal)
	if len(chineseIDs) == 0 || chineseIDs[0] != "zh-receipt" {
		t.Fatalf("chinese goal selection = %v, want zh-receipt first", chineseIDs)
	}
	if chineseSemantic == nil || chineseSemantic.Mode != contextstore.RetrievalActive || chineseSemantic.FallbackReason != contextstore.SemanticFallbackNone {
		t.Fatalf("chinese goal semantic identity = %+v", chineseSemantic)
	}
	if status := runtime.Status(); status.RoutedLexical != 1 || status.RoutedSemantic != 1 || status.Mode != string(semantic.ModeAuto) {
		t.Fatalf("status = %+v, want one lexical and one semantic route", status)
	}
}

func TestWorkerRecallAutoRoutesOncePerRecall(t *testing.T) {
	fake := newConceptOllama(t)
	repo := openSemanticTestRepo(t)
	scope := contextstore.Scope{ProjectID: "project", TeamID: "team", SessionID: "session", BranchID: "main", AgentID: "worker"}
	if err := repo.Append(t.Context(), semanticItem("private-zh", "上次用收據檢查副作用避免重送", scope)); err != nil {
		t.Fatal(err)
	}
	runtime := readySemanticRuntime(t, conceptSettings(t, fake, semantic.ModeAuto, nil), repo)
	service := NewWorkerMemoryServiceWithOptions(repo, WorkerMemoryOptions{
		Semantic: runtime, Mode: runtime.Mode(), Router: runtime.router(), Fusion: runtime.Fusion(),
	})
	recall := func(query string) WorkerMemoryBundle {
		t.Helper()
		bundle, err := service.Recall(t.Context(), WorkerMemoryRecallRequest{
			WorkerID: "worker", BranchID: "main", Scope: scope, Query: query,
			Policy: agent.WorkerMemoryPolicy{Mode: agent.WorkerMemoryPersistent, MaxItems: 3, MaxTokens: 500},
		})
		if err != nil {
			t.Fatal(err)
		}
		return bundle
	}
	if english := recall("check duplicate side effect"); len(english.Items) != 0 || english.Semantic != nil {
		t.Fatalf("english recall used the semantic lane: items=%v semantic=%+v", english.Trace.ItemIDs, english.Semantic)
	}
	chinese := recall("檢查收據避免重送")
	if len(chinese.Items) != 1 || chinese.Items[0].ID != "private-zh" || chinese.Semantic == nil {
		t.Fatalf("chinese recall = %v semantic=%+v", chinese.Trace.ItemIDs, chinese.Semantic)
	}
	if status := runtime.Status(); status.RoutedLexical != 1 || status.RoutedSemantic != 1 {
		t.Fatalf("status = %+v, want one route per recall across session and persistent scopes", status)
	}
}
