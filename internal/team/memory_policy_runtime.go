package team

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/kjelly/hufu/internal/agent"
	contextstore "github.com/kjelly/hufu/internal/context"
)

// MemoryRuntimeRankingPolicy is the runtime ranking parameter set that the
// coordinator adopts for active shared-memory ordering and token budgeting.
// It is deliberately separate from the learning policy: the learning policy
// controls observation/attribution, while these weights control the final
// score formula (spec §5.4).
type MemoryRuntimeRankingPolicy struct {
	CandidateTopK int
	InjectTopK    int
	// TopK is a read-only compatibility alias for policy snapshots created
	// before candidate and injection limits were separated.
	TopK             int
	MinimumRelevance float64
	UtilityWeight    float64
	FreshnessWeight  float64
}

func defaultMemoryRuntimeRankingPolicy() MemoryRuntimeRankingPolicy {
	return MemoryRuntimeRankingPolicy{CandidateTopK: 20, InjectTopK: 4, TopK: 20, MinimumRelevance: minimumMemoryRelevance, UtilityWeight: 0.50, FreshnessWeight: 1}
}

func (c *Coordinator) effectiveMemoryRankingPolicy() MemoryRuntimeRankingPolicy {
	policy := c.memoryRankingPolicy
	if policy.CandidateTopK <= 0 {
		policy.CandidateTopK = policy.TopK
	}
	if policy.InjectTopK <= 0 {
		policy.InjectTopK = policy.TopK
	}
	if policy.CandidateTopK <= 0 || policy.InjectTopK <= 0 || policy.InjectTopK > policy.CandidateTopK {
		return defaultMemoryRuntimeRankingPolicy()
	}
	policy.TopK = policy.CandidateTopK
	return policy
}

// LoadAdoptedMemoryPolicy reads the active memory policy snapshot from the
// canonical store and returns the learning policy plus the runtime ranking
// parameters it adopted. When no policy has been adopted (or the repository is
// not a SQLite store), it returns the defaults. The snapshot is validated the
// same way the coordinator validates it at load time, so a corrupt or
// inconsistent active policy is rejected rather than silently used. Callers
// that explain or display scores must use the returned runtime parameters so
// the displayed final score matches the score the runtime actually used for
// prompt selection (spec §7 HF-MEM4-005).
func LoadAdoptedMemoryPolicy(ctx context.Context, repo contextstore.Repository) (agent.MemoryLearningPolicy, MemoryRuntimeRankingPolicy, error) {
	return LoadMemoryPolicy(ctx, repo, "")
}

// LoadMemoryPolicy resolves a policy version to its own immutable snapshot and
// runtime ranking parameters. When policyVersion is empty, the active policy is
// used (falling back to defaults when none has been adopted). When a specific
// version is requested, it must be recorded in the canonical store; an unknown
// version is rejected so explain-memory can never mix an overridden version's
// aggregate with the active policy's weights (spec §7 HF-MEM4-005).
func LoadMemoryPolicy(ctx context.Context, repo contextstore.Repository, policyVersion string) (agent.MemoryLearningPolicy, MemoryRuntimeRankingPolicy, error) {
	learning := agent.DefaultMemoryLearningPolicy()
	runtime := defaultMemoryRuntimeRankingPolicy()
	sqlRepo, ok := repo.(*contextstore.SQLiteRepository)
	if !ok || sqlRepo == nil {
		return learning, runtime, nil
	}
	var record contextstore.MemoryPolicyVersionRecord
	var err error
	if policyVersion == "" {
		record, err = sqlRepo.ActiveMemoryPolicyVersion(ctx)
		if errors.Is(err, sql.ErrNoRows) {
			return learning, runtime, nil
		}
	} else {
		record, err = sqlRepo.MemoryPolicyVersion(ctx, policyVersion)
		if errors.Is(err, sql.ErrNoRows) {
			return learning, runtime, fmt.Errorf("memory policy version %q is not recorded", policyVersion)
		}
	}
	if err != nil {
		return learning, runtime, err
	}
	var snapshot struct {
		ID           string                     `json:"id"`
		RevisionHash string                     `json:"revision_hash"`
		Learning     agent.MemoryLearningPolicy `json:"learning"`
		Retrieval    struct {
			TopK             int     `json:"top_k"`
			CandidateTopK    int     `json:"candidate_top_k"`
			InjectTopK       int     `json:"inject_top_k"`
			MinimumRelevance float64 `json:"minimum_relevance"`
			UtilityWeight    float64 `json:"utility_weight"`
			FreshnessWeight  float64 `json:"freshness_weight"`
		} `json:"retrieval"`
	}
	if err := json.Unmarshal(record.Snapshot, &snapshot); err != nil {
		return learning, runtime, err
	}
	if snapshot.ID != record.PolicyVersion || snapshot.RevisionHash != record.RevisionHash {
		return learning, runtime, fmt.Errorf("memory policy %q revision identity mismatch", record.PolicyVersion)
	}
	validMode := false
	switch snapshot.Learning.Mode {
	case agent.MemoryLearningOff, agent.MemoryLearningObserve, agent.MemoryLearningShadow, agent.MemoryLearningActive:
		validMode = true
	}
	candidateTopK, injectTopK := snapshot.Retrieval.CandidateTopK, snapshot.Retrieval.InjectTopK
	if candidateTopK == 0 && injectTopK == 0 {
		candidateTopK, injectTopK = snapshot.Retrieval.TopK, snapshot.Retrieval.TopK
	}
	if !validMode || snapshot.Learning.PriorAlpha <= 0 || snapshot.Learning.PriorBeta <= 0 || snapshot.Learning.UtilityPercentile <= 0 || snapshot.Learning.UtilityPercentile >= 1 || snapshot.Learning.MaxCreditPerSignal <= 0 || snapshot.Learning.MinConfirmedSupport < 0 || snapshot.Learning.MinIndependentTasks < 0 || snapshot.Learning.MaxHarmRate < 0 || snapshot.Learning.MaxHarmRate > 1 || candidateTopK <= 0 || injectTopK <= 0 || injectTopK > candidateTopK || snapshot.Retrieval.MinimumRelevance < 0 || snapshot.Retrieval.MinimumRelevance > 1 || snapshot.Retrieval.UtilityWeight < 0 || snapshot.Retrieval.FreshnessWeight < 0 {
		return learning, runtime, fmt.Errorf("memory policy %q contains invalid runtime parameters", record.PolicyVersion)
	}
	snapshot.Learning.PolicyVersion = record.PolicyVersion
	return snapshot.Learning, MemoryRuntimeRankingPolicy{CandidateTopK: candidateTopK, InjectTopK: injectTopK, TopK: candidateTopK, MinimumRelevance: snapshot.Retrieval.MinimumRelevance, UtilityWeight: snapshot.Retrieval.UtilityWeight, FreshnessWeight: snapshot.Retrieval.FreshnessWeight}, nil
}

func (c *Coordinator) loadAdoptedMemoryPolicy(ctx context.Context) error {
	if c == nil || c.session == nil {
		return nil
	}
	learning, runtime, err := LoadAdoptedMemoryPolicy(ctx, c.contextRepo)
	if err != nil {
		return err
	}
	c.session.Config.MemoryLearning = learning
	c.memoryRankingPolicy = runtime
	return nil
}
