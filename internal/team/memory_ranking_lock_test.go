package team

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/kjelly/hufu/internal/agent"
	contextstore "github.com/kjelly/hufu/internal/context"
)

// rankingLockFixture seeds a store that exercises duplicates, priorities,
// must-keep, a harmful aggregate, and a stale environment marker.
func rankingLockFixture(t *testing.T, mode agent.MemoryLearningMode) *Coordinator {
	t.Helper()
	c, repo := rankingTestCoordinator(t, mode)
	words := []string{"sqlite", "schema", "migration", "worker", "retry", "budget", "vector", "cache", "lint", "release"}
	var items []contextstore.ContextItem
	for i := range 36 {
		content := fmt.Sprintf("%s %s %s note %d", words[i%len(words)], words[(i*3+1)%len(words)], words[(i*7+2)%len(words)], i)
		item := rankingItem(fmt.Sprintf("lock-%02d", i), contextstore.Priority(10*(i%5)))
		// Preserve insertion order explicitly: the projection sorts equal
		// priorities by creation time, which determines the allowed subset.
		item.CreatedAt = time.Unix(200+int64(i), 0)
		if i%11 == 0 {
			// Same content under different kinds keeps separate rows, so
			// retrieval's content-hash deduplication has work to do.
			content = "sqlite schema migration duplicate guidance"
			item.Kind = []contextstore.ContextKind{contextstore.ContextPattern, contextstore.ContextDecision, contextstore.ContextConvention, contextstore.ContextArchitecture}[i/11]
		}
		item.Content = content
		item.TrustLevel = []contextstore.TrustLevel{contextstore.TrustTrusted, contextstore.TrustInternal, contextstore.TrustUntrusted}[i%3]
		if i == 4 {
			item.MustKeep = true
		}
		if i == 6 {
			item.Metadata = map[string]string{"stale_environment": "true"}
		}
		items = append(items, item)
	}
	if err := repo.Append(context.Background(), items...); err != nil {
		t.Fatal(err)
	}
	for i, id := range []string{"lock-01", "lock-02", "lock-12"} {
		if _, err := repo.ApplyExperienceObservation(context.Background(), contextstore.ExperienceObservation{
			IdempotencyKey: "lock-" + id, ContextItemID: id, PolicyVersion: c.session.Config.MemoryLearning.PolicyVersion, ProjectID: "project", TaskID: fmt.Sprintf("task-%d", i),
			AppliedDelta: 1, PositiveWeight: float64(i), NegativeWeight: float64(2 - i), PriorAlpha: 1, PriorBeta: 1, UtilityPercentile: 0.1,
		}); err != nil {
			t.Fatal(err)
		}
	}
	return c
}

func rankingLockSummary(t *testing.T) string {
	t.Helper()
	queries := []string{"sqlite", "retry", "cache lint", "migration\nphase: implement", "duplicate guidance", "nothing-matches-at-all"}
	var lines []string
	for _, mode := range []agent.MemoryLearningMode{agent.MemoryLearningOff, agent.MemoryLearningObserve, agent.MemoryLearningShadow, agent.MemoryLearningActive} {
		c := rankingLockFixture(t, mode)
		base, err := c.contextRepo.QuerySharedPersistentProjection(context.Background(), c.contextScope())
		if err != nil {
			t.Fatal(err)
		}
		subset := map[string]bool{}
		for i, item := range base {
			if i%2 == 0 {
				subset[item.ID] = true
			}
		}
		for _, query := range queries {
			for name, allowed := range map[string]map[string]bool{"all": nil, "subset": subset} {
				selected, scores, finals, _, err := c.rankSharedPersistentMemoryAllowed(context.Background(), query, base, allowed)
				if err != nil {
					t.Fatal(err)
				}
				parts := make([]string, 0, len(selected))
				for _, item := range selected {
					parts = append(parts, fmt.Sprintf("%s(base=%.6f final=%.6f)", item.ID, scores[item.ID].BaseRelevance, finals[item.ID]))
				}
				scored := make([]string, 0, len(scores))
				for id, part := range scores {
					scored = append(scored, fmt.Sprintf("%s=%.6f", id, part.BaseRelevance))
				}
				sort.Strings(scored)
				lines = append(lines, fmt.Sprintf("%s|%q|%s selected=[%s] scored=[%s]", mode, query, name, strings.Join(parts, " "), strings.Join(scored, " ")))
			}
		}
	}
	sort.Strings(lines)
	return strings.Join(lines, "\n") + "\n"
}

// TestPersistentRankingBehaviourIsLocked pins the runtime shared-persistent
// ranking so refactors that share it with explain-memory cannot change which
// memories reach the prompt. Regenerate only for an intended ranking change:
// UPDATE_RANKING_LOCK=1 go test ./internal/team -run TestPersistentRankingBehaviourIsLocked
func TestPersistentRankingBehaviourIsLocked(t *testing.T) {
	got := rankingLockSummary(t)
	path := filepath.Join("testdata", "memory_ranking_lock.golden")
	if os.Getenv("UPDATE_RANKING_LOCK") == "1" {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got != string(want) {
		t.Fatalf("persistent ranking changed:\n--- got\n%s\n--- want\n%s", got, want)
	}
}
