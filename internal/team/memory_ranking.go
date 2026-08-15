package team

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/kjelly/hufu/internal/agent"
	contextstore "github.com/kjelly/hufu/internal/context"
)

const minimumMemoryRelevance = 0.05

type MemoryRankingEntry struct {
	ContextItemID string           `json:"context_item_id"`
	BaseRank      int              `json:"base_rank"`
	FinalRank     int              `json:"final_rank"`
	Selected      bool             `json:"selected"`
	ScoreParts    MemoryScoreParts `json:"score_parts"`
	FinalScore    float64          `json:"final_score"`
}

type MemoryRankingTrace struct {
	CreatedAt     time.Time                `json:"created_at"`
	Mode          agent.MemoryLearningMode `json:"mode"`
	PolicyVersion string                   `json:"policy_version"`
	QueryHash     string                   `json:"query_hash"`
	Entries       []MemoryRankingEntry     `json:"entries"`
	Error         string                   `json:"error,omitempty"`
}

type MemoryScoreExplanation struct {
	ContextItemID        string           `json:"context_item_id"`
	PolicyVersion        string           `json:"policy_version"`
	RetrievalID          string           `json:"retrieval_id,omitempty"`
	ScoreParts           MemoryScoreParts `json:"score_parts"`
	FinalScore           float64          `json:"final_score"`
	PositiveWeight       float64          `json:"positive_weight"`
	NegativeWeight       float64          `json:"negative_weight"`
	ExposureCount        int              `json:"exposure_count"`
	AppliedCount         int              `json:"applied_count"`
	VerifiedSupportCount int              `json:"verified_support_count"`
	CausalFailureCount   int              `json:"causal_failure_count"`
}

// ExplainMemoryScore computes the explanation with the default runtime ranking
// policy. Callers that have access to the adopted policy snapshot must use
// ExplainMemoryScoreWithPolicy instead so the displayed final score matches the
// score the runtime actually used for prompt selection (spec §7 HF-MEM4-005).
func ExplainMemoryScore(item contextstore.ContextItem, baseRelevance float64, aggregate contextstore.ExperienceAggregate, policy agent.MemoryLearningPolicy) MemoryScoreExplanation {
	return ExplainMemoryScoreWithPolicy(item, baseRelevance, aggregate, policy, defaultMemoryRuntimeRankingPolicy())
}

// ExplainMemoryScoreWithPolicy computes the explanation with the exact runtime
// ranking parameters the coordinator adopted. The final score is derived with
// those weights, so a non-default adopted policy produces the same score the
// reranker used when it selected the prompt memory.
func ExplainMemoryScoreWithPolicy(item contextstore.ContextItem, baseRelevance float64, aggregate contextstore.ExperienceAggregate, policy agent.MemoryLearningPolicy, runtime MemoryRuntimeRankingPolicy) MemoryScoreExplanation {
	utility := aggregate.UtilityLowerBound
	if aggregate.ContextItemID == "" {
		utility = contextstore.BetaQuantile(policy.PriorAlpha, policy.PriorBeta, policy.UtilityPercentile)
	}
	asOf := aggregate.LastObservedAt
	if asOf.IsZero() {
		asOf = contextItemUpdatedAt(item)
	}
	parts := MemoryScoreParts{BaseRelevance: baseRelevance, Applicability: 1, UtilityLowerBound: utility, Freshness: memoryFreshnessAt(item, asOf), TrustFactor: memoryTrustFactor(item.TrustLevel), HarmfulUsePenalty: aggregate.NegativeWeight / (aggregate.PositiveWeight + aggregate.NegativeWeight + 1), StaleEnvironmentPenalty: staleEnvironmentPenalty(item)}
	return MemoryScoreExplanation{ContextItemID: item.ID, PolicyVersion: policy.PolicyVersion, ScoreParts: parts, FinalScore: reinforcedFinalScoreWithPolicy(parts, runtime), PositiveWeight: aggregate.PositiveWeight, NegativeWeight: aggregate.NegativeWeight, ExposureCount: aggregate.ExposureCount, AppliedCount: aggregate.AppliedCount, VerifiedSupportCount: aggregate.VerifiedSupportCount, CausalFailureCount: aggregate.CausalFailureCount}
}

func reinforcedFinalScore(parts MemoryScoreParts) float64 {
	return reinforcedFinalScoreWithPolicy(parts, defaultMemoryRuntimeRankingPolicy())
}

func reinforcedFinalScoreWithPolicy(parts MemoryScoreParts, policy MemoryRuntimeRankingPolicy) float64 {
	return parts.BaseRelevance*parts.Applicability*(0.75+policy.UtilityWeight*parts.UtilityLowerBound)*math.Pow(parts.Freshness, policy.FreshnessWeight)*parts.TrustFactor - parts.HarmfulUsePenalty - parts.StaleEnvironmentPenalty
}

func (c *Coordinator) rankSharedPersistentMemory(ctx context.Context, query string, base []contextstore.ContextItem) ([]contextstore.ContextItem, map[string]MemoryScoreParts, map[string]float64, error) {
	return c.rankSharedPersistentMemoryAllowed(ctx, query, base, nil)
}

func (c *Coordinator) rankSharedPersistentMemoryAllowed(ctx context.Context, query string, base []contextstore.ContextItem, allowed map[string]bool) ([]contextstore.ContextItem, map[string]MemoryScoreParts, map[string]float64, error) {
	policy := c.session.Config.MemoryLearning
	rankingPolicy := c.effectiveMemoryRankingPolicy()
	if strings.TrimSpace(query) == "" {
		mustKeep := make([]contextstore.ContextItem, 0)
		pinned := make([]contextstore.ContextItem, 0)
		for _, item := range base {
			if item.MustKeep {
				mustKeep = append(mustKeep, item)
			} else if item.Pinned {
				pinned = append(pinned, item)
			}
		}
		selected := append([]contextstore.ContextItem(nil), mustKeep...)
		remaining := rankingPolicy.InjectTopK - len(selected)
		if remaining > 0 {
			if len(pinned) > remaining {
				pinned = pinned[:remaining]
			}
			selected = append(selected, pinned...)
		}
		return selected, nil, nil, nil
	}
	candidateLimit := rankingPolicy.CandidateTopK
	if allowed != nil && len(base) > candidateLimit {
		candidateLimit = len(base)
	}
	results, _, err := contextstore.HybridRetrieve(ctx, c.contextRepo, nil, contextstore.SearchRequest{
		Query: query, Scope: persistentContextScope(c.contextScope()), Limit: candidateLimit,
	})
	if err != nil {
		return nil, nil, nil, err
	}
	if len(results) == 0 {
		// ContextRequest queries deliberately carry structured state on separate
		// lines. Some lexical backends interpret the whole string conjunctively;
		// fall back to the goal line so state labels cannot suppress an otherwise
		// relevant candidate. Activation gates still enforce the state contract.
		if goal, _, found := strings.Cut(query, "\n"); found && strings.TrimSpace(goal) != "" {
			results, _, err = contextstore.HybridRetrieve(ctx, c.contextRepo, nil, contextstore.SearchRequest{Query: goal, Scope: persistentContextScope(c.contextScope()), Limit: candidateLimit})
			if err != nil {
				return nil, nil, nil, err
			}
		}
	}
	if allowed != nil {
		filtered := results[:0]
		for _, result := range results {
			if allowed[result.Item.ID] {
				filtered = append(filtered, result)
			}
		}
		results = filtered
		if len(results) > rankingPolicy.CandidateTopK {
			results = results[:rankingPolicy.CandidateTopK]
		}
	}
	// HybridRetrieve's RRF scores are reciprocal ranks (the first lexical hit
	// is about 1/61), while runtime policy relevance is defined on [0,1].
	// Normalize that fused scale before applying the policy threshold.
	for i := range results {
		if results[i].Score > 0 && results[i].Score < 1 {
			results[i].Score = math.Min(1, results[i].Score*61)
		}
	}
	relevanceEntries, relevanceScores, relevanceFinal := relevanceMemoryEntries(results, rankingPolicy)
	if policy.Mode == agent.MemoryLearningOff || policy.Mode == agent.MemoryLearningObserve {
		if policy.Mode == agent.MemoryLearningObserve {
			c.persistMemoryRankingTrace(memoryRankingTrace(policy, query, results, relevanceEntries))
		}
		return selectedMemoryResults(results, relevanceEntries), relevanceScores, relevanceFinal, nil
	}
	entries, scores, err := c.reinforceSearchResults(ctx, results, policy)
	if err != nil {
		return base, nil, nil, err
	}
	trace := memoryRankingTrace(policy, query, results, entries)
	c.persistMemoryRankingTrace(trace)
	if policy.Mode == agent.MemoryLearningShadow {
		return selectedMemoryResults(results, relevanceEntries), relevanceScores, relevanceFinal, nil
	}
	finalScores := make(map[string]float64, len(entries))
	for _, entry := range entries {
		finalScores[entry.ContextItemID] = entry.FinalScore
	}
	return selectedMemoryResults(results, entries), scores, finalScores, nil
}

func relevanceMemoryEntries(results []contextstore.SearchResult, policy MemoryRuntimeRankingPolicy) ([]MemoryRankingEntry, map[string]MemoryScoreParts, map[string]float64) {
	entries := make([]MemoryRankingEntry, 0, len(results))
	scores := make(map[string]MemoryScoreParts, len(results))
	finalScores := make(map[string]float64, len(results))
	selected := 0
	for i, result := range results {
		parts := MemoryScoreParts{BaseRelevance: result.Score, Applicability: 1, Freshness: 1, TrustFactor: memoryTrustFactor(result.Item.TrustLevel), StaleEnvironmentPenalty: staleEnvironmentPenalty(result.Item)}
		include := selected < policy.InjectTopK && result.Score >= policy.MinimumRelevance && parts.StaleEnvironmentPenalty == 0
		entry := MemoryRankingEntry{ContextItemID: result.Item.ID, BaseRank: i + 1, FinalRank: i + 1, Selected: include, ScoreParts: parts, FinalScore: result.Score}
		if include {
			selected++
		}
		entries = append(entries, entry)
		scores[result.Item.ID] = parts
		finalScores[result.Item.ID] = result.Score
	}
	return entries, scores, finalScores
}

func selectedMemoryResults(results []contextstore.SearchResult, entries []MemoryRankingEntry) []contextstore.ContextItem {
	byID := make(map[string]contextstore.ContextItem, len(results))
	for _, result := range results {
		byID[result.Item.ID] = result.Item
	}
	selected := make([]contextstore.ContextItem, 0, len(entries))
	for _, entry := range entries {
		if entry.Selected {
			selected = append(selected, byID[entry.ContextItemID])
		}
	}
	return selected
}

func persistentContextScope(scope contextstore.Scope) contextstore.Scope {
	scope.SessionID, scope.BranchID, scope.AgentID, scope.TaskID, scope.AttemptID = "", "", "", "", ""
	return scope
}

func (c *Coordinator) reinforceSearchResults(ctx context.Context, results []contextstore.SearchResult, policy agent.MemoryLearningPolicy) ([]MemoryRankingEntry, map[string]MemoryScoreParts, error) {
	repo, _ := c.contextRepo.(contextstore.ExperienceRepository)
	rankingPolicy := c.effectiveMemoryRankingPolicy()
	asOf := rankingReferenceTime(results)
	entries := make([]MemoryRankingEntry, 0, len(results))
	scores := make(map[string]MemoryScoreParts, len(results))
	for rank, result := range results {
		utility := contextstore.BetaQuantile(policy.PriorAlpha, policy.PriorBeta, policy.UtilityPercentile)
		positive, negative := 0.0, 0.0
		if repo != nil {
			if aggregate, err := repo.ExperienceAggregate(ctx, result.Item.ID, policy.PolicyVersion); err == nil {
				utility, positive, negative = aggregate.UtilityLowerBound, aggregate.PositiveWeight, aggregate.NegativeWeight
			} else if !errors.Is(err, sql.ErrNoRows) {
				return nil, nil, fmt.Errorf("load experience aggregate for %s: %w", result.Item.ID, err)
			}
		}
		parts := MemoryScoreParts{
			BaseRelevance: result.Score, Applicability: 1,
			UtilityLowerBound: utility, Freshness: memoryFreshnessAt(result.Item, asOf),
			TrustFactor:             memoryTrustFactor(result.Item.TrustLevel),
			HarmfulUsePenalty:       negative / (positive + negative + 1),
			StaleEnvironmentPenalty: staleEnvironmentPenalty(result.Item),
		}
		entry := MemoryRankingEntry{ContextItemID: result.Item.ID, BaseRank: rank + 1, ScoreParts: parts, FinalScore: reinforcedFinalScoreWithPolicy(parts, rankingPolicy)}
		entries = append(entries, entry)
		scores[result.Item.ID] = parts
	}
	sort.SliceStable(entries, func(i, j int) bool {
		if entries[i].FinalScore != entries[j].FinalScore {
			return entries[i].FinalScore > entries[j].FinalScore
		}
		if entries[i].ScoreParts.BaseRelevance != entries[j].ScoreParts.BaseRelevance {
			return entries[i].ScoreParts.BaseRelevance > entries[j].ScoreParts.BaseRelevance
		}
		left, right := searchItem(results, entries[i].ContextItemID), searchItem(results, entries[j].ContextItemID)
		leftDistance, rightDistance := memoryScopeDistance(left.Scope, c.contextScope()), memoryScopeDistance(right.Scope, c.contextScope())
		if leftDistance != rightDistance {
			return leftDistance < rightDistance
		}
		if left.Priority != right.Priority {
			return left.Priority > right.Priority
		}
		if left.Confidence != right.Confidence {
			return left.Confidence > right.Confidence
		}
		if !left.UpdatedAt.Equal(right.UpdatedAt) {
			return left.UpdatedAt.After(right.UpdatedAt)
		}
		return entries[i].ContextItemID < entries[j].ContextItemID
	})
	selected := 0
	for i := range entries {
		entries[i].FinalRank = i + 1
		entries[i].Selected = selected < rankingPolicy.InjectTopK && entries[i].ScoreParts.BaseRelevance >= rankingPolicy.MinimumRelevance && entries[i].ScoreParts.HarmfulUsePenalty == 0 && entries[i].ScoreParts.StaleEnvironmentPenalty == 0 && entries[i].FinalScore > 0
		if entries[i].Selected {
			selected++
		}
	}
	return entries, scores, nil
}

func searchItem(results []contextstore.SearchResult, id string) contextstore.ContextItem {
	for _, result := range results {
		if result.Item.ID == id {
			return result.Item
		}
	}
	return contextstore.ContextItem{}
}

func contextItemUpdatedAt(item contextstore.ContextItem) time.Time {
	if !item.UpdatedAt.IsZero() {
		return item.UpdatedAt
	}
	return item.CreatedAt
}

func rankingReferenceTime(results []contextstore.SearchResult) time.Time {
	var asOf time.Time
	for _, result := range results {
		if updated := contextItemUpdatedAt(result.Item); updated.After(asOf) {
			asOf = updated
		}
	}
	return asOf
}

func memoryFreshnessAt(item contextstore.ContextItem, asOf time.Time) float64 {
	updated := contextItemUpdatedAt(item)
	if updated.IsZero() || asOf.IsZero() {
		return 1
	}
	ageDays := asOf.Sub(updated).Hours() / 24
	if ageDays < 0 {
		ageDays = 0
	}
	return math.Max(0.5, math.Exp(-ageDays/365))
}

func memoryScopeDistance(item, requested contextstore.Scope) int {
	distance := 0
	fields := [][2]string{{item.ProjectID, requested.ProjectID}, {item.TeamID, requested.TeamID}, {item.SessionID, requested.SessionID}, {item.BranchID, requested.BranchID}, {item.AgentID, requested.AgentID}, {item.TaskID, requested.TaskID}, {item.AttemptID, requested.AttemptID}}
	for _, field := range fields {
		if field[0] == "" {
			distance++
		} else if field[1] != "" && field[0] != field[1] {
			return 1 << 20
		}
	}
	return distance
}

func memoryTrustFactor(trust contextstore.TrustLevel) float64 {
	switch trust {
	case contextstore.TrustTrusted:
		return 1
	case contextstore.TrustInternal:
		return 0.9
	default:
		return 0.5
	}
}

func staleEnvironmentPenalty(item contextstore.ContextItem) float64 {
	if strings.EqualFold(strings.TrimSpace(item.Metadata["stale_environment"]), "true") || strings.EqualFold(strings.TrimSpace(item.Metadata["environment_status"]), "stale") {
		return 1
	}
	return 0
}

func memoryRankingTrace(policy agent.MemoryLearningPolicy, query string, base []contextstore.SearchResult, entries []MemoryRankingEntry) MemoryRankingTrace {
	return MemoryRankingTrace{CreatedAt: time.Now().UTC(), Mode: policy.Mode, PolicyVersion: policy.PolicyVersion, QueryHash: hashContentKey(query), Entries: entries}
}

func (c *Coordinator) persistMemoryRankingTrace(trace MemoryRankingTrace) {
	if c == nil || c.session == nil || c.session.Workspace == "" {
		return
	}
	data, err := json.Marshal(trace)
	if err != nil {
		return
	}
	shadowTraceMu.Lock()
	defer shadowTraceMu.Unlock()
	path := filepath.Join(c.session.Workspace, "memory-ranking-traces.jsonl")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer func() { _ = f.Close() }()
	_, _ = f.Write(append(data, '\n'))
}

// QueryHash returns the privacy-safe identity of a retrieval query. It is the
// same hash the runtime persists in ranking traces and injection manifests, so
// explain-memory can bind a retrieval ID to the exact query being explained.
func QueryHash(query string) string {
	return hashContentKey(query)
}

// RetrievalIDForItem returns the retrieval ID of the most recent durable memory
// injection manifest that bound the given context item under the exact policy
// version and query hash being explained, or "" when none exists. The manifest
// is the attempt-scoped attribution boundary checkpointed in session.json; spec
// §7 HF-MEM4-005 requires explain-memory to report the retrieval ID that bound
// the item to the prompt, and §5.1 requires that binding to be attempt-scoped.
// Filtering by policy version and query hash prevents an explain request for
// one policy/query from returning a retrieval ID from a different injection.
func RetrievalIDForItem(workspace, policyVersion, queryHash, contextItemID string) string {
	session := LoadSession(workspace)
	if session == nil {
		return ""
	}
	var latest time.Time
	var retrievalID string
	for _, task := range session.Tasks {
		if task == nil {
			continue
		}
		for i := range task.MemoryManifests {
			manifest := &task.MemoryManifests[i]
			if manifest.PolicyVersion != policyVersion || manifest.QueryHash != queryHash {
				continue
			}
			includes := false
			for _, item := range manifest.Items {
				if item.ContextItemID == contextItemID {
					includes = true
					break
				}
			}
			if !includes {
				continue
			}
			if manifest.CreatedAt.After(latest) {
				latest = manifest.CreatedAt
				retrievalID = manifest.RetrievalID
			}
		}
	}
	return retrievalID
}

func (c *Coordinator) rerankWorkerMemory(ctx context.Context, bundle *WorkerMemoryBundle, query string) {
	if c == nil || bundle == nil || c.session == nil {
		return
	}
	policy := c.session.Config.MemoryLearning
	if policy.Mode != agent.MemoryLearningShadow && policy.Mode != agent.MemoryLearningActive {
		return
	}
	results := make([]contextstore.SearchResult, 0, len(bundle.Items))
	for _, item := range bundle.Items {
		results = append(results, contextstore.SearchResult{Item: item.ContextItem, Score: item.BaseScore})
	}
	entries, _, err := c.reinforceSearchResults(ctx, results, policy)
	if err != nil {
		redactedErr := contextstore.RedactSecrets(err.Error())
		c.persistMemoryRankingTrace(MemoryRankingTrace{CreatedAt: time.Now().UTC(), Mode: policy.Mode, PolicyVersion: policy.PolicyVersion, QueryHash: hashContentKey(query), Error: redactedErr})
		_ = c.emitEvent("observability_degraded", "memory_ranker", "", map[string]any{"component": "worker_memory_learning", "mode": policy.Mode, "policy_version": policy.PolicyVersion, "error": redactedErr})
		return
	}
	c.persistMemoryRankingTrace(memoryRankingTrace(policy, query, results, entries))
	if policy.Mode != agent.MemoryLearningActive {
		return
	}
	byID := make(map[string]WorkerMemoryItem, len(bundle.Items))
	for _, item := range bundle.Items {
		byID[item.ID] = item
	}
	ranked := make([]WorkerMemoryItem, 0, len(entries))
	for _, entry := range entries {
		if len(ranked) >= c.effectiveMemoryRankingPolicy().InjectTopK {
			break
		}
		if !entry.Selected {
			continue
		}
		item := byID[entry.ContextItemID]
		item.FinalScore = entry.FinalScore
		item.ScoreParts = entry.ScoreParts
		ranked = append(ranked, item)
	}
	bundle.Items = ranked
}
