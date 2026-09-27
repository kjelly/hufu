package team

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/kjelly/hufu/internal/agent"
	contextstore "github.com/kjelly/hufu/internal/context"
)

// fusionRankingCoordinator seeds a corpus large enough that legacy fusion
// carries raw BM25 scores above 1 (BUG-01).
func fusionRankingCoordinator(t *testing.T, mode agent.MemoryLearningMode, fusion string) (*Coordinator, *contextstore.SQLiteRepository) {
	t.Helper()
	c, repo := rankingTestCoordinator(t, mode)
	var items []contextstore.ContextItem
	for i := range 300 {
		item := rankingItem(fmt.Sprintf("filler-%03d", i), 10)
		item.Content = fmt.Sprintf("generic filler note %d about coordinator workers", i)
		items = append(items, item)
	}
	for i, content := range []string{"sqlite schema version table", "back up the sqlite schema before migrating", "sqlite schema checksum mismatch aborts the open"} {
		item := rankingItem(fmt.Sprintf("hit-%d", i), 10)
		item.Content = content
		items = append(items, item)
	}
	if err := repo.Append(context.Background(), items...); err != nil {
		t.Fatal(err)
	}
	retrieval := map[string]any{}
	if fusion != "" {
		retrieval["fusion"] = fusion
	}
	adoptExplainTestPolicy(t, c, repo, mode, retrieval)
	return c, repo
}

func TestRuntimeFusionPolicyControlsRelevanceScale(t *testing.T) {
	cases := []struct {
		fusion       string
		wantFusion   contextstore.FusionMode
		wantAboveOne bool
	}{
		{fusion: "", wantFusion: "", wantAboveOne: true},
		{fusion: "legacy", wantFusion: contextstore.FusionLegacy, wantAboveOne: true},
		{fusion: "rrf_normalized", wantFusion: contextstore.FusionRRFNormalized},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("fusion=%q", tc.fusion), func(t *testing.T) {
			c, _ := fusionRankingCoordinator(t, agent.MemoryLearningActive, tc.fusion)
			if got := c.effectiveMemoryRankingPolicy().Fusion; got != tc.wantFusion {
				t.Fatalf("adopted fusion = %q, want %q", got, tc.wantFusion)
			}
			_, scores, _, _, err := c.rankSharedPersistentMemoryAllowed(context.Background(), "sqlite schema", nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			maxBase := 0.0
			for _, parts := range scores {
				maxBase = max(maxBase, parts.BaseRelevance)
			}
			if tc.wantAboveOne != (maxBase > 1) || maxBase <= 0 {
				t.Fatalf("max base relevance = %v, want above one = %v", maxBase, tc.wantAboveOne)
			}
			if !tc.wantAboveOne && maxBase != 1 {
				t.Fatalf("normalized top base relevance = %v, want 1", maxBase)
			}
		})
	}
}

func TestLoadMemoryPolicyRejectsUnknownFusion(t *testing.T) {
	_, repo := rankingTestCoordinator(t, agent.MemoryLearningActive)
	learning := agent.DefaultMemoryLearningPolicy()
	learning.PolicyVersion = "memory-policy-bad-fusion"
	data, err := json.Marshal(map[string]any{"id": learning.PolicyVersion, "revision_hash": "rev", "learning": learning,
		"retrieval": map[string]any{"candidate_top_k": 20, "inject_top_k": 4, "minimum_relevance": 0.05, "utility_weight": 0.5, "freshness_weight": 1, "fusion": "rrf"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.SaveMemoryPolicyVersion(context.Background(), learning.PolicyVersion, data, "rev", "active", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if _, _, err := LoadMemoryPolicy(context.Background(), repo, ""); err == nil || !strings.Contains(err.Error(), "invalid runtime parameters") {
		t.Fatalf("LoadMemoryPolicy err = %v, want unknown fusion rejected", err)
	}
}
