package team

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/kjelly/hufu/internal/agent"
	contextstore "github.com/kjelly/hufu/internal/context"
)

func TestApprovedMemoryPolicyLoadsIntoRuntime(t *testing.T) {
	repo, err := contextstore.OpenSQLite(filepath.Join(t.TempDir(), "context.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = repo.Close() }()
	policy := agent.DefaultMemoryLearningPolicy()
	policy.Mode = agent.MemoryLearningActive
	snapshot := map[string]any{
		"id": "candidate", "revision_hash": "revision-candidate", "learning": policy,
		"retrieval": map[string]any{"top_k": 7, "minimum_relevance": 0.2, "utility_weight": 0.7, "freshness_weight": 0.8},
	}
	data, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	baseline, _ := json.Marshal(map[string]any{"id": "baseline", "revision_hash": "revision-baseline"})
	if err := repo.SaveMemoryPolicyVersion(context.Background(), "baseline", baseline, "revision-baseline", "baseline", nowUTC()); err != nil {
		t.Fatal(err)
	}
	if err := repo.SaveMemoryPolicyVersion(context.Background(), "candidate", data, "revision-candidate", "eligible_for_review", nowUTC()); err != nil {
		t.Fatal(err)
	}
	if err := repo.ActivateMemoryPolicy(context.Background(), "candidate", "baseline", "superseded"); err != nil {
		t.Fatal(err)
	}
	c := &Coordinator{contextRepo: repo, session: &TeamSession{Config: agent.TeamConfig{MemoryLearning: agent.DefaultMemoryLearningPolicy()}}}
	if err := c.loadAdoptedMemoryPolicy(context.Background()); err != nil {
		t.Fatal(err)
	}
	if c.session.Config.MemoryLearning.Mode != agent.MemoryLearningActive || c.session.Config.MemoryLearning.PolicyVersion != "candidate" {
		t.Fatalf("runtime learning policy = %+v", c.session.Config.MemoryLearning)
	}
	if c.memoryRankingPolicy.TopK != 7 || c.memoryRankingPolicy.CandidateTopK != 7 || c.memoryRankingPolicy.InjectTopK != 7 || c.memoryRankingPolicy.MinimumRelevance != 0.2 || c.memoryRankingPolicy.UtilityWeight != 0.7 || c.memoryRankingPolicy.FreshnessWeight != 0.8 {
		t.Fatalf("runtime ranking policy = %+v", c.memoryRankingPolicy)
	}
}

func TestNewMemoryPolicySeparatesCandidateAndInjectLimits(t *testing.T) {
	repo, err := contextstore.OpenSQLite(filepath.Join(t.TempDir(), "context.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = repo.Close() }()
	policy := agent.DefaultMemoryLearningPolicy()
	snapshot := map[string]any{"id": "split", "revision_hash": "rev-split", "learning": policy, "retrieval": map[string]any{"top_k": 99, "candidate_top_k": 20, "inject_top_k": 4, "minimum_relevance": 0.05, "utility_weight": 0.5, "freshness_weight": 1.0}}
	data, _ := json.Marshal(snapshot)
	baseline, _ := json.Marshal(map[string]any{"id": "baseline", "revision_hash": "rev-baseline"})
	if err := repo.SaveMemoryPolicyVersion(context.Background(), "baseline", baseline, "rev-baseline", "baseline", nowUTC()); err != nil {
		t.Fatal(err)
	}
	if err := repo.SaveMemoryPolicyVersion(context.Background(), "split", data, "rev-split", "eligible_for_review", nowUTC()); err != nil {
		t.Fatal(err)
	}
	if err := repo.ActivateMemoryPolicy(context.Background(), "split", "baseline", "superseded"); err != nil {
		t.Fatal(err)
	}
	_, runtime, err := LoadAdoptedMemoryPolicy(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.CandidateTopK != 20 || runtime.InjectTopK != 4 {
		t.Fatalf("runtime limits = %+v", runtime)
	}
}
