package team

import (
	"context"
	"encoding/json"
	"math"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/kjelly/hufu/internal/agent"
	contextstore "github.com/kjelly/hufu/internal/context"
)

func TestUtilityCannotOverrideLowRelevance(t *testing.T) {
	parts := MemoryScoreParts{BaseRelevance: 0.001, Applicability: memoryApplicability(0.001), UtilityLowerBound: 1, Freshness: 1, TrustFactor: 1}
	if score := reinforcedFinalScore(parts); score != 0 {
		t.Fatalf("low-relevance score = %f", score)
	}
}

func TestActiveRankingIsDeterministic(t *testing.T) {
	c, repo := rankingTestCoordinator(t, agent.MemoryLearningActive)
	results := []contextstore.SearchResult{
		{Item: rankingItem("a", 10), Score: .5},
		{Item: rankingItem("b", 20), Score: .5},
	}
	first, _, err := c.reinforceSearchResults(context.Background(), results, c.session.Config.MemoryLearning)
	if err != nil {
		t.Fatal(err)
	}
	second, _, err := c.reinforceSearchResults(context.Background(), results, c.session.Config.MemoryLearning)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("ranking differs:\n%+v\n%+v", first, second)
	}
	_ = repo
}

func TestCandidateExpiredSupersededRejectedAreNeverInjected(t *testing.T) {
	expiredAt := time.Unix(50, 0)
	items := []contextstore.ContextItem{
		rankingItem("confirmed", 10),
		{ID: "candidate", Kind: contextstore.ContextPattern, Content: "candidate", Lifecycle: contextstore.LifecycleCandidate},
		{ID: "rejected", Kind: contextstore.ContextPattern, Content: "rejected", Lifecycle: contextstore.LifecycleRejected},
		{ID: "superseded", Kind: contextstore.ContextPattern, Content: "superseded", Lifecycle: contextstore.LifecycleConfirmed, SupersededBy: "confirmed"},
		{ID: "expired", Kind: contextstore.ContextPattern, Content: "expired", Lifecycle: contextstore.LifecycleConfirmed, ExpiresAt: &expiredAt},
	}
	got := canonicalCompilerItems(items, PriorityRelevantLTM, "shared_persistent", false)
	if len(got) != 1 || got[0].ID != "context:confirmed" {
		t.Fatalf("eligible compiler items = %+v", got)
	}
}

func TestUntrustedMemoryCannotOverrideNormativeContext(t *testing.T) {
	normative := ContextItem{ID: "constraint", Kind: "hard_constraints", Content: "never deploy", Priority: PriorityHardConstraints, Authority: ContextAuthorityNormative}
	memory := ContextItem{ID: "context:memory", Kind: "pattern", Content: "deploy", Priority: PriorityRelevantLTM, Authority: ContextAuthorityHistorical, FinalScore: 100, ScoreParts: MemoryScoreParts{Applicability: 1}, Freshness: time.Now(), Confidence: 1}
	ranked := RankContextItems([]ContextItem{memory, normative})
	if ranked[0].ID != normative.ID {
		t.Fatalf("ranked[0] = %s, want normative context first", ranked[0].ID)
	}
}

func TestVerifiedEvidenceRaisesComparableMemoryRank(t *testing.T) {
	c, repo := rankingTestCoordinator(t, agent.MemoryLearningActive)
	_, err := repo.ApplyExperienceObservation(context.Background(), contextstore.ExperienceObservation{IdempotencyKey: "support-a", ContextItemID: "a", PolicyVersion: "memory-policy-v1", TaskID: "task", PositiveWeight: 1, VerifiedSupportDelta: 1})
	if err != nil {
		t.Fatal(err)
	}
	results := []contextstore.SearchResult{{Item: rankingItem("b", 10), Score: .5}, {Item: rankingItem("a", 10), Score: .5}}
	entries, _, err := c.reinforceSearchResults(context.Background(), results, c.session.Config.MemoryLearning)
	if err != nil {
		t.Fatal(err)
	}
	if entries[0].ContextItemID != "a" {
		t.Fatalf("reinforced rank = %+v", entries)
	}
}

func TestActiveRankingExcludesStaleAndCausallyHarmfulMemory(t *testing.T) {
	c, repo := rankingTestCoordinator(t, agent.MemoryLearningActive)
	_, err := repo.ApplyExperienceObservation(context.Background(), contextstore.ExperienceObservation{IdempotencyKey: "harm-b", ContextItemID: "b", PolicyVersion: "memory-policy-v1", TaskID: "task", NegativeWeight: 1, CausalFailureDelta: 1})
	if err != nil {
		t.Fatal(err)
	}
	stale := rankingItem("a", 10)
	stale.Metadata = map[string]string{"stale_environment": "true"}
	entries, _, err := c.reinforceSearchResults(context.Background(), []contextstore.SearchResult{{Item: stale, Score: .8}, {Item: rankingItem("b", 10), Score: .8}}, c.session.Config.MemoryLearning)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.Selected {
			t.Fatalf("unsafe memory remained selectable: %+v", entry)
		}
	}
}

func TestActiveRankingPolicyMatchesCompilerSelection(t *testing.T) {
	c, repo := rankingTestCoordinator(t, agent.MemoryLearningActive)
	// Adopt a non-default runtime policy: utility and freshness are ignored, so
	// ordering is pure base relevance. Under the default policy (utility 0.5,
	// freshness 1) the fresh high-utility item would outrank the stale one.
	c.memoryRankingPolicy = MemoryRuntimeRankingPolicy{TopK: 20, MinimumRelevance: minimumMemoryRelevance, UtilityWeight: 0, FreshnessWeight: 0}
	// Item "a": high base relevance, low utility, stale.
	// Item "b": lower base relevance, high utility, fresh.
	if _, err := repo.ApplyExperienceObservation(context.Background(), contextstore.ExperienceObservation{IdempotencyKey: "support-b", ContextItemID: "b", PolicyVersion: "memory-policy-v1", TaskID: "task", PositiveWeight: 20, VerifiedSupportDelta: 1}); err != nil {
		t.Fatal(err)
	}
	stale := rankingItem("a", 10)
	stale.UpdatedAt = time.Unix(0, 0)
	fresh := rankingItem("b", 20)
	fresh.UpdatedAt = time.Unix(100000000, 0)
	results := []contextstore.SearchResult{
		{Item: stale, Score: 0.8},
		{Item: fresh, Score: 0.6},
	}
	entries, scores, err := c.reinforceSearchResults(context.Background(), results, c.session.Config.MemoryLearning)
	if err != nil {
		t.Fatal(err)
	}
	if entries[0].ContextItemID != "a" {
		t.Fatalf("active trace ranks %s first, want a: %+v", entries[0].ContextItemID, entries)
	}
	// Sanity: the default policy would order b first, proving the adopted
	// policy actually changes the ordering.
	if defaultScoreA, defaultScoreB := reinforcedFinalScore(scores["a"]), reinforcedFinalScore(scores["b"]); defaultScoreB <= defaultScoreA {
		t.Fatalf("test setup: default policy should rank b above a (a=%f b=%f)", defaultScoreA, defaultScoreB)
	}
	// Build the bundle exactly as canonicalContextBundleForQuery would.
	finalScores := make(map[string]float64, len(entries))
	for _, entry := range entries {
		finalScores[entry.ContextItemID] = entry.FinalScore
	}
	bundle := &CanonicalContextBundle{
		SharedPersistent:            []contextstore.ContextItem{stale, fresh},
		SharedPersistentScores:      scores,
		SharedPersistentFinalScores: finalScores,
	}
	compiled, err := CompileWorkerContext(context.Background(), WorkerContextInput{
		TaskGoal:        "goal",
		CanonicalMemory: bundle,
		ModelContext:    ModelContextSpec{ModelID: "test", ContextWindow: 10000, MaxOutputTokens: 100, SafetyMarginTokens: 100},
	})
	if err != nil {
		t.Fatal(err)
	}
	var included []string
	for _, item := range compiled.IncludedItems {
		if item.Source == "shared_persistent" {
			included = append(included, strings.TrimPrefix(item.ID, "context:"))
		}
	}
	if len(included) != 2 || included[0] != "a" || included[1] != "b" {
		t.Fatalf("compiler shared memory order = %v, want [a b]", included)
	}
}

func TestShadowModeDoesNotChangePrompt(t *testing.T) {
	c, _ := rankingTestCoordinator(t, agent.MemoryLearningShadow)
	base := []contextstore.ContextItem{rankingItem("base-a", 10), rankingItem("base-b", 20)}
	got, scores, finalScores, err := c.rankSharedPersistentMemory(context.Background(), "procedure", base)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, base) || scores != nil || finalScores != nil {
		t.Fatalf("shadow changed selection: got=%+v scores=%+v finalScores=%+v", got, scores, finalScores)
	}
}

func TestExplainMemoryScoreUsesAdoptedRuntimePolicy(t *testing.T) {
	workspace := t.TempDir()
	repo, err := contextstore.OpenSQLite(filepath.Join(workspace, "context.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = repo.Close() }()
	item := rankingItem("explain-a", 10)
	if err := repo.Append(context.Background(), item); err != nil {
		t.Fatal(err)
	}
	// Adopt a non-default active policy: utility and freshness are ignored, so
	// the final score is pure base relevance * applicability * trust.
	learning := agent.DefaultMemoryLearningPolicy()
	learning.Mode = agent.MemoryLearningActive
	learning.PolicyVersion = "memory-policy-v2"
	snapshot := map[string]any{
		"id": learning.PolicyVersion, "revision_hash": "rev-2", "learning": learning,
		"retrieval": map[string]any{"top_k": 20, "minimum_relevance": 0.05, "utility_weight": 0.0, "freshness_weight": 0.0},
	}
	data, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.SaveMemoryPolicyVersion(context.Background(), learning.PolicyVersion, data, "rev-2", "active", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	gotLearning, gotRuntime, err := LoadAdoptedMemoryPolicy(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	if gotLearning.PolicyVersion != "memory-policy-v2" || gotRuntime.UtilityWeight != 0 || gotRuntime.FreshnessWeight != 0 {
		t.Fatalf("adopted policy = %+v / %+v", gotLearning, gotRuntime)
	}
	aggregate := contextstore.ExperienceAggregate{ContextItemID: item.ID, PolicyVersion: learning.PolicyVersion, PositiveWeight: 1, UtilityLowerBound: 0.5, LastObservedAt: time.Unix(100, 0)}
	explanation := ExplainMemoryScoreWithPolicy(item, 0.8, aggregate, gotLearning, gotRuntime)
	parts := explanation.ScoreParts
	want := reinforcedFinalScoreWithPolicy(parts, gotRuntime)
	if math.Abs(explanation.FinalScore-want) > 1e-9 {
		t.Fatalf("final score = %f, want %f", explanation.FinalScore, want)
	}
	// The default policy would produce a different score, proving the adopted
	// policy actually changed the result.
	if defaultScore := reinforcedFinalScore(parts); math.Abs(defaultScore-want) < 1e-9 {
		t.Fatalf("test setup: default and adopted policies should differ (both %f)", want)
	}
}

func TestRetrievalIDForItemBindsPolicyAndQuery(t *testing.T) {
	workspace := t.TempDir()
	now := time.Now().UTC()
	manifestV1 := MemoryInjectionManifest{
		RetrievalID: "retrieval-v1", RunID: "run-1", TaskID: "task-1", Attempt: 1, Agent: "worker",
		PolicyVersion: "memory-policy-v1", QueryHash: QueryHash("goal v1"),
		Items:     []MemoryInjectionItem{{ContextItemID: "explain-a", Source: "shared_persistent", Rank: 1}},
		CreatedAt: now,
	}
	manifestV2 := MemoryInjectionManifest{
		RetrievalID: "retrieval-v2", RunID: "run-2", TaskID: "task-2", Attempt: 1, Agent: "worker",
		PolicyVersion: "memory-policy-v2", QueryHash: QueryHash("goal v2"),
		Items:     []MemoryInjectionItem{{ContextItemID: "explain-a", Source: "shared_persistent", Rank: 1}},
		CreatedAt: now.Add(time.Second),
	}
	session := NewSession()
	session.Tasks = []*TodoItem{
		{ID: "task-1", MemoryManifests: []MemoryInjectionManifest{manifestV1}},
		{ID: "task-2", MemoryManifests: []MemoryInjectionManifest{manifestV2}},
	}
	if err := SaveSession(workspace, session); err != nil {
		t.Fatal(err)
	}
	// Exact policy + query match returns the bound retrieval ID.
	if got := RetrievalIDForItem(workspace, "memory-policy-v1", QueryHash("goal v1"), "explain-a"); got != "retrieval-v1" {
		t.Fatalf("v1 retrieval id = %q, want retrieval-v1", got)
	}
	if got := RetrievalIDForItem(workspace, "memory-policy-v2", QueryHash("goal v2"), "explain-a"); got != "retrieval-v2" {
		t.Fatalf("v2 retrieval id = %q, want retrieval-v2", got)
	}
	// A v1 request must never return v2's retrieval ID even though v2 is newer.
	if got := RetrievalIDForItem(workspace, "memory-policy-v1", QueryHash("goal v1"), "explain-a"); got == "retrieval-v2" {
		t.Fatal("v1 request returned v2 retrieval id")
	}
	// A different query under the same policy is a different retrieval.
	if got := RetrievalIDForItem(workspace, "memory-policy-v1", QueryHash("other query"), "explain-a"); got != "" {
		t.Fatalf("mismatched query retrieval id = %q, want empty", got)
	}
	// An unbound item has no retrieval id.
	if got := RetrievalIDForItem(workspace, "memory-policy-v1", QueryHash("goal v1"), "missing"); got != "" {
		t.Fatalf("retrieval id for missing item = %q, want empty", got)
	}
}

func TestLoadMemoryPolicyResolvesRequestedVersion(t *testing.T) {
	workspace := t.TempDir()
	repo, err := contextstore.OpenSQLite(filepath.Join(workspace, "context.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = repo.Close() }()
	// v1 is active with utility=0; v2 is recorded (not active) with utility=0.5.
	learningV1 := agent.DefaultMemoryLearningPolicy()
	learningV1.Mode = agent.MemoryLearningActive
	learningV1.PolicyVersion = "memory-policy-v1"
	snapshotV1 := map[string]any{
		"id": learningV1.PolicyVersion, "revision_hash": "rev-1", "learning": learningV1,
		"retrieval": map[string]any{"top_k": 20, "minimum_relevance": 0.05, "utility_weight": 0.0, "freshness_weight": 0.0},
	}
	dataV1, err := json.Marshal(snapshotV1)
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.SaveMemoryPolicyVersion(context.Background(), "memory-policy-v1", dataV1, "rev-1", "active", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	learningV2 := agent.DefaultMemoryLearningPolicy()
	learningV2.Mode = agent.MemoryLearningActive
	learningV2.PolicyVersion = "memory-policy-v2"
	snapshotV2 := map[string]any{
		"id": learningV2.PolicyVersion, "revision_hash": "rev-2", "learning": learningV2,
		"retrieval": map[string]any{"top_k": 20, "minimum_relevance": 0.05, "utility_weight": 0.5, "freshness_weight": 1},
	}
	dataV2, err := json.Marshal(snapshotV2)
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.SaveMemoryPolicyVersion(context.Background(), "memory-policy-v2", dataV2, "rev-2", "candidate", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	// Active resolves to v1.
	active, activeRuntime, err := LoadAdoptedMemoryPolicy(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	if active.PolicyVersion != "memory-policy-v1" || activeRuntime.UtilityWeight != 0 {
		t.Fatalf("active policy = %+v / %+v", active, activeRuntime)
	}
	// Explicit v2 resolves to v2's own weights, not the active v1 weights.
	v2, v2Runtime, err := LoadMemoryPolicy(context.Background(), repo, "memory-policy-v2")
	if err != nil {
		t.Fatal(err)
	}
	if v2.PolicyVersion != "memory-policy-v2" || v2Runtime.UtilityWeight != 0.5 || v2Runtime.FreshnessWeight != 1 {
		t.Fatalf("v2 policy = %+v / %+v", v2, v2Runtime)
	}
	// An unknown version is rejected.
	if _, _, err := LoadMemoryPolicy(context.Background(), repo, "memory-policy-unknown"); err == nil {
		t.Fatal("expected error for unknown policy version")
	}
}

func rankingTestCoordinator(t *testing.T, mode agent.MemoryLearningMode) (*Coordinator, *contextstore.SQLiteRepository) {
	t.Helper()
	workspace := t.TempDir()
	repo, err := contextstore.OpenSQLite(filepath.Join(workspace, "context.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range []contextstore.ContextItem{rankingItem("a", 10), rankingItem("b", 20), rankingItem("base-a", 10), rankingItem("base-b", 20)} {
		if err := repo.Append(context.Background(), item); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { _ = repo.Close() })
	policy := agent.DefaultMemoryLearningPolicy()
	policy.Mode = mode
	return &Coordinator{session: &TeamSession{Workspace: workspace, Config: agent.TeamConfig{Name: "team", MemoryLearning: policy}}, contextRepo: repo, projectDir: "project"}, repo
}

func rankingItem(id string, priority contextstore.Priority) contextstore.ContextItem {
	return contextstore.ContextItem{ID: id, Kind: contextstore.ContextPattern, Content: "procedure " + id, Scope: contextstore.Scope{ProjectID: "project", TeamID: "team"}, Lifecycle: contextstore.LifecycleConfirmed, TrustLevel: contextstore.TrustTrusted, Priority: priority, Confidence: 1, UpdatedAt: time.Unix(100, 0)}
}
