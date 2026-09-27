package team

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kjelly/hufu/internal/agent"
	contextstore "github.com/kjelly/hufu/internal/context"
)

// adoptExplainTestPolicy records an active policy snapshot so explain-memory
// and the coordinator resolve the same mode and ranking parameters.
func adoptExplainTestPolicy(t *testing.T, c *Coordinator, repo *contextstore.SQLiteRepository, mode agent.MemoryLearningMode, retrieval map[string]any) {
	t.Helper()
	learning := agent.DefaultMemoryLearningPolicy()
	learning.Mode = mode
	learning.PolicyVersion = "memory-policy-explain"
	if retrieval == nil {
		retrieval = map[string]any{}
	}
	for key, value := range map[string]any{"top_k": 20, "candidate_top_k": 20, "inject_top_k": 4, "minimum_relevance": 0.05, "utility_weight": 0.5, "freshness_weight": 1} {
		if _, ok := retrieval[key]; !ok {
			retrieval[key] = value
		}
	}
	data, err := json.Marshal(map[string]any{"id": learning.PolicyVersion, "revision_hash": "rev", "learning": learning, "retrieval": retrieval})
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.SaveMemoryPolicyVersion(context.Background(), learning.PolicyVersion, data, "rev", "active", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if err := c.loadAdoptedMemoryPolicy(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func explainInput(c *Coordinator, id, query string) MemoryExplainInput {
	return MemoryExplainInput{Workspace: c.session.Workspace, ItemID: id, ProjectID: "project", TeamID: "team", Query: query}
}

func TestExplainPersistentMemoryMatchesRuntimeSelection(t *testing.T) {
	queries := []string{"sqlite", "retry", "cache lint", "migration\nphase: implement", "duplicate guidance"}
	for _, mode := range []agent.MemoryLearningMode{agent.MemoryLearningOff, agent.MemoryLearningObserve, agent.MemoryLearningShadow, agent.MemoryLearningActive} {
		t.Run(string(mode), func(t *testing.T) {
			c := rankingLockFixture(t, mode)
			repo := c.contextRepo.(*contextstore.SQLiteRepository)
			adoptExplainTestPolicy(t, c, repo, mode, nil)
			base, err := repo.QuerySharedPersistentProjection(context.Background(), c.contextScope())
			if err != nil {
				t.Fatal(err)
			}
			eligible := make([]contextstore.ContextItem, 0, len(base))
			allowed := map[string]bool{}
			for _, item := range base {
				if evaluateLifecycleEligibility(item, "", time.Now()) == "" {
					eligible = append(eligible, item)
					allowed[item.ID] = true
				}
			}
			for _, query := range queries {
				selected, scores, finals, _, err := c.rankSharedPersistentMemoryAllowed(context.Background(), query, eligible, allowed)
				if err != nil {
					t.Fatal(err)
				}
				selectedIDs := map[string]bool{}
				for _, item := range selected {
					selectedIDs[item.ID] = true
				}
				for _, item := range base {
					got, err := ExplainPersistentMemory(context.Background(), repo, explainInput(c, item.ID, query))
					if err != nil {
						t.Fatal(err)
					}
					if got.Ranking.Selected != selectedIDs[item.ID] {
						t.Fatalf("query %q item %s: explain selected=%v, runtime selected=%v (reasons %v)", query, item.ID, got.Ranking.Selected, selectedIDs[item.ID], got.Reasons)
					}
					if part, ok := scores[item.ID]; ok && math.Abs(part.BaseRelevance-got.ScoreParts.BaseRelevance) > 1e-9 {
						t.Fatalf("query %q item %s: base relevance %v, runtime %v", query, item.ID, got.ScoreParts.BaseRelevance, part.BaseRelevance)
					}
					if mode == agent.MemoryLearningActive {
						if final, ok := finals[item.ID]; ok && math.Abs(final-got.FinalScore) > 1e-9 {
							t.Fatalf("query %q item %s: final %v, runtime %v", query, item.ID, got.FinalScore, final)
						}
					}
					if len(got.Reasons) == 0 {
						t.Fatalf("query %q item %s: no reason", query, item.ID)
					}
				}
			}
		})
	}
}

func TestExplainPersistentMemoryHidesOtherScopes(t *testing.T) {
	c, repo := rankingTestCoordinator(t, agent.MemoryLearningOff)
	if err := repo.Append(context.Background(), contextstore.ContextItem{ID: "other-project", Kind: contextstore.ContextPattern, Content: "procedure other", Scope: contextstore.Scope{ProjectID: "elsewhere", TeamID: "team"}, Lifecycle: contextstore.LifecycleConfirmed}); err != nil {
		t.Fatal(err)
	}
	if err := repo.Append(context.Background(), contextstore.ContextItem{ID: "other-team", Kind: contextstore.ContextPattern, Content: "procedure other team", Scope: contextstore.Scope{ProjectID: "project", TeamID: "rival"}, Lifecycle: contextstore.LifecycleConfirmed}); err != nil {
		t.Fatal(err)
	}
	var messages []string
	for _, id := range []string{"missing", "other-project", "other-team"} {
		_, err := ExplainPersistentMemory(context.Background(), repo, explainInput(c, id, "procedure"))
		if !errors.Is(err, ErrMemoryExplainNotFound) {
			t.Fatalf("%s err = %v", id, err)
		}
		messages = append(messages, strings.ReplaceAll(err.Error(), id, "<id>"))
	}
	if messages[0] != messages[1] || messages[0] != messages[2] {
		t.Fatalf("scope errors differ: %q", messages)
	}
}

func TestExplainPersistentMemoryReasons(t *testing.T) {
	past := time.Unix(1, 0).UTC()
	cases := []struct {
		name      string
		mode      agent.MemoryLearningMode
		retrieval map[string]any
		items     []contextstore.ContextItem
		harmful   string
		id        string
		query     string
		want      MemoryExplainReason
	}{
		{name: "selected", id: "a", query: "procedure a", want: MemoryExplainSelected},
		{name: "outside observed", id: "a", query: "unrelated", items: []contextstore.ContextItem{explainItem("unrelated", "unrelated words")}, want: MemoryExplainOutsideObserved},
		{name: "duplicate content", id: "dup-b", query: "duplicated guidance", items: []contextstore.ContextItem{explainItem("dup-a", "duplicated guidance"), withKind(explainItem("dup-b", "duplicated guidance"), contextstore.ContextDecision)}, want: MemoryExplainDuplicateContent},
		{name: "inject limit", id: "many-5", query: "shared", items: manyExplainItems(6), want: MemoryExplainInjectLimit},
		{name: "candidate limit", retrieval: map[string]any{"candidate_top_k": 2, "inject_top_k": 1}, id: "many-4", query: "shared", items: manyExplainItems(6), want: MemoryExplainCandidateLimit},
		{name: "below relevance", retrieval: map[string]any{"minimum_relevance": 1.0}, id: "many-4", query: "shared", items: manyExplainItems(6), want: MemoryExplainBelowRelevance},
		{name: "must keep forced", id: "keep", query: "unrelated", items: []contextstore.ContextItem{withMustKeep(explainItem("keep", "keep this")), explainItem("unrelated", "unrelated words")}, want: MemoryExplainMustKeepForced},
		{name: "candidate lifecycle", id: "pending", query: "pending", items: []contextstore.ContextItem{withLifecycle(explainItem("pending", "pending note"), contextstore.LifecycleCandidate)}, want: MemoryExplainLifecycleIneligible},
		{name: "expired", id: "old", query: "old", items: []contextstore.ContextItem{withExpiry(explainItem("old", "old note"), past)}, want: MemoryExplainExpired},
		{name: "session scoped", id: "session-note", query: "session", items: []contextstore.ContextItem{withSession(explainItem("session-note", "session note"))}, want: MemoryExplainOutsidePersistentScope},
		{name: "harmful use", mode: agent.MemoryLearningActive, id: "a", query: "procedure a", harmful: "a", want: MemoryExplainHarmfulUse},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mode := tc.mode
			if mode == "" {
				mode = agent.MemoryLearningOff
			}
			c, repo := rankingTestCoordinator(t, mode)
			if len(tc.items) > 0 {
				if err := repo.Append(context.Background(), tc.items...); err != nil {
					t.Fatal(err)
				}
			}
			if tc.harmful != "" {
				if _, err := repo.ApplyExperienceObservation(context.Background(), contextstore.ExperienceObservation{IdempotencyKey: "harm", ContextItemID: tc.harmful, PolicyVersion: "memory-policy-explain", ProjectID: "project", TaskID: "task", NegativeWeight: 2, PriorAlpha: 1, PriorBeta: 1, UtilityPercentile: 0.1}); err != nil {
					t.Fatal(err)
				}
			}
			adoptExplainTestPolicy(t, c, repo, mode, tc.retrieval)
			got, err := ExplainPersistentMemory(context.Background(), repo, explainInput(c, tc.id, tc.query))
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Contains(got.Reasons, tc.want) {
				t.Fatalf("reasons = %v, want %s (ranking %+v candidate %+v)", got.Reasons, tc.want, got.Ranking, got.Retrieval.Candidate)
			}
			if tc.want == MemoryExplainMustKeepForced && !got.Ranking.Injected {
				t.Fatalf("must-keep item not reported as injected: %+v", got.Ranking)
			}
		})
	}
}

func TestExplainPersistentMemoryHasNoSideEffects(t *testing.T) {
	c, repo := rankingTestCoordinator(t, agent.MemoryLearningActive)
	adoptExplainTestPolicy(t, c, repo, agent.MemoryLearningActive, nil)
	before, err := repo.Revision(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	got, err := ExplainPersistentMemory(context.Background(), repo, explainInput(c, "a", "procedure a"))
	if err != nil {
		t.Fatal(err)
	}
	if after, _ := repo.Revision(context.Background()); after != before {
		t.Fatalf("explain wrote context events: %d -> %d", before, after)
	}
	if _, err := os.Stat(filepath.Join(c.session.Workspace, "memory-ranking-traces.jsonl")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("explain persisted a ranking trace: %v", err)
	}
	if !got.Recomputed || got.SchemaVersion != 2 || len(got.NotEvaluated) != 2 || got.ModeSource != "policy_snapshot" {
		t.Fatalf("explanation header = %+v", got)
	}
	vector := got.Retrieval.Paths[len(got.Retrieval.Paths)-1]
	if vector.Path != "vector" || vector.Executed || vector.UnavailableReason != contextstore.SemanticFallbackProjectionMissing {
		t.Fatalf("vector path = %+v", vector)
	}
	encoded, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "procedure a") {
		t.Fatalf("explanation leaked query or content: %s", encoded)
	}
}

func explainItem(id, content string) contextstore.ContextItem {
	item := rankingItem(id, 10)
	item.Content = content
	return item
}

func manyExplainItems(n int) []contextstore.ContextItem {
	items := make([]contextstore.ContextItem, n)
	for i := range items {
		items[i] = explainItem(fmt.Sprintf("many-%d", i), fmt.Sprintf("shared topic %s", strings.Repeat("x", i+1)))
	}
	return items
}

func withKind(item contextstore.ContextItem, kind contextstore.ContextKind) contextstore.ContextItem {
	item.Kind = kind
	return item
}

func withMustKeep(item contextstore.ContextItem) contextstore.ContextItem {
	item.MustKeep = true
	return item
}

func withLifecycle(item contextstore.ContextItem, lifecycle contextstore.ContextLifecycle) contextstore.ContextItem {
	item.Lifecycle = lifecycle
	return item
}

func withExpiry(item contextstore.ContextItem, at time.Time) contextstore.ContextItem {
	item.ExpiresAt = &at
	return item
}

func withSession(item contextstore.ContextItem) contextstore.ContextItem {
	item.Scope.SessionID = "session-1"
	return item
}
