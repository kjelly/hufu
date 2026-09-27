package team

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/kjelly/hufu/internal/agent"
	contextstore "github.com/kjelly/hufu/internal/context"
)

// persistentRankingInput is everything the shared-persistent ranking reads.
// It carries no coordinator so explain-memory can recompute the exact runtime
// ranking without side effects.
type persistentRankingInput struct {
	Query string
	// RequestScope is the coordinator scope; retrieval widens it to the
	// persistent project/team scope and tie-breaks use its distance.
	RequestScope contextstore.Scope
	// Base holds the eligible persistent items; the empty-query path selects
	// must-keep and pinned items from it.
	Base     []contextstore.ContextItem
	Allowed  map[string]bool
	Learning agent.MemoryLearningPolicy
	Ranking  MemoryRuntimeRankingPolicy
	Observer *contextstore.RetrievalObservation
}

type persistentRankingResult struct {
	// Results are the retrieval candidates after allowed-set filtering, the
	// CandidateTopK cut, and fused-score normalization.
	Results           []contextstore.SearchResult
	RelevanceEntries  []MemoryRankingEntry
	ReinforcedEntries []MemoryRankingEntry
	Aggregates        map[string]*contextstore.ExperienceAggregate
	Selected          []contextstore.ContextItem
	Scores            map[string]MemoryScoreParts
	FinalScores       map[string]float64
	UsedGoalFallback  bool
	// CandidateCutIDs were allowed but fell beyond CandidateTopK.
	CandidateCutIDs []string
}

// memoryReinforcementError marks a failure while reading experience
// aggregates; the coordinator then keeps its base selection.
type memoryReinforcementError struct{ err error }

func (e memoryReinforcementError) Error() string { return e.err.Error() }
func (e memoryReinforcementError) Unwrap() error { return e.err }

// rankPersistentMemory is the shared-persistent memory ranking used for prompt
// selection. It performs no writes.
func rankPersistentMemory(ctx context.Context, repo contextstore.RetrievalRepository, experience contextstore.ExperienceRepository, in persistentRankingInput) (persistentRankingResult, error) {
	var out persistentRankingResult
	if strings.TrimSpace(in.Query) == "" {
		mustKeep := make([]contextstore.ContextItem, 0)
		pinned := make([]contextstore.ContextItem, 0)
		for _, item := range in.Base {
			if item.MustKeep {
				mustKeep = append(mustKeep, item)
			} else if item.Pinned {
				pinned = append(pinned, item)
			}
		}
		out.Selected = append([]contextstore.ContextItem(nil), mustKeep...)
		remaining := in.Ranking.InjectTopK - len(out.Selected)
		if remaining > 0 {
			if len(pinned) > remaining {
				pinned = pinned[:remaining]
			}
			out.Selected = append(out.Selected, pinned...)
		}
		return out, nil
	}
	candidateLimit := in.Ranking.CandidateTopK
	if in.Allowed != nil && len(in.Base) > candidateLimit {
		candidateLimit = len(in.Base)
	}
	retrieve := func(query string) ([]contextstore.SearchResult, error) {
		results, _, err := contextstore.HybridRetrieveWithOptions(ctx, repo, contextstore.SearchRequest{
			Query: query, Scope: persistentContextScope(in.RequestScope), Limit: candidateLimit,
		}, contextstore.HybridRetrievalOptions{Mode: contextstore.RetrievalActive, UnavailableReason: contextstore.SemanticFallbackProjectionMissing, Observer: in.Observer, Fusion: in.Ranking.Fusion})
		return results, err
	}
	results, err := retrieve(in.Query)
	if err != nil {
		return out, err
	}
	if len(results) == 0 {
		// ContextRequest queries deliberately carry structured state on separate
		// lines. Some lexical backends interpret the whole string conjunctively;
		// fall back to the goal line so state labels cannot suppress an otherwise
		// relevant candidate. Activation gates still enforce the state contract.
		if goal, _, found := strings.Cut(in.Query, "\n"); found && strings.TrimSpace(goal) != "" {
			out.UsedGoalFallback = true
			if results, err = retrieve(goal); err != nil {
				return out, err
			}
		}
	}
	if in.Allowed != nil {
		filtered := results[:0]
		for _, result := range results {
			if in.Allowed[result.Item.ID] {
				filtered = append(filtered, result)
			}
		}
		results = filtered
		if len(results) > in.Ranking.CandidateTopK {
			for _, result := range results[in.Ranking.CandidateTopK:] {
				out.CandidateCutIDs = append(out.CandidateCutIDs, result.Item.ID)
			}
			results = results[:in.Ranking.CandidateTopK]
		}
	}
	normalizeFusedRelevance(results, in.Ranking.Fusion)
	out.Results = results
	relevanceEntries, relevanceScores, relevanceFinal := relevanceMemoryEntries(results, in.Ranking)
	out.RelevanceEntries = relevanceEntries
	if in.Learning.Mode == agent.MemoryLearningOff || in.Learning.Mode == agent.MemoryLearningObserve {
		out.Selected, out.Scores, out.FinalScores = selectedMemoryResults(results, relevanceEntries), relevanceScores, relevanceFinal
		return out, nil
	}
	entries, scores, aggregates, err := reinforceSearchResultsWith(ctx, experience, results, in.Learning, in.Ranking, in.RequestScope)
	if err != nil {
		return out, memoryReinforcementError{err: err}
	}
	out.ReinforcedEntries, out.Aggregates = entries, aggregates
	if in.Learning.Mode == agent.MemoryLearningShadow {
		out.Selected, out.Scores, out.FinalScores = selectedMemoryResults(results, relevanceEntries), relevanceScores, relevanceFinal
		return out, nil
	}
	finalScores := make(map[string]float64, len(entries))
	for _, entry := range entries {
		finalScores[entry.ContextItemID] = entry.FinalScore
	}
	out.Selected, out.Scores, out.FinalScores = selectedMemoryResults(results, entries), scores, finalScores
	return out, nil
}

// normalizeFusedRelevance maps fused scores onto the policy's [0,1]
// relevance scale. Legacy fusion multiplies sub-unit scores by 61, which
// normalizes a pure reciprocal rank but leaves a carried raw BM25 score of 1
// or more unscaled (BUG-01). Normalized RRF is already on the unit scale, so
// it is only clamped; multiplying it again would saturate every score to 1.
func normalizeFusedRelevance(results []contextstore.SearchResult, fusion contextstore.FusionMode) {
	for i := range results {
		if fusion == contextstore.FusionRRFNormalized {
			results[i].Score = math.Max(0, math.Min(1, results[i].Score))
			continue
		}
		if results[i].Score > 0 && results[i].Score < 1 {
			results[i].Score = math.Min(1, results[i].Score*61)
		}
	}
}

// reinforceSearchResultsWith applies the reinforced memory score to retrieval
// results; experience may be nil, which uses the prior utility for every item.
func reinforceSearchResultsWith(ctx context.Context, experience contextstore.ExperienceRepository, results []contextstore.SearchResult, policy agent.MemoryLearningPolicy, rankingPolicy MemoryRuntimeRankingPolicy, requestScope contextstore.Scope) ([]MemoryRankingEntry, map[string]MemoryScoreParts, map[string]*contextstore.ExperienceAggregate, error) {
	asOf := rankingReferenceTime(results)
	entries := make([]MemoryRankingEntry, 0, len(results))
	scores := make(map[string]MemoryScoreParts, len(results))
	aggregates := make(map[string]*contextstore.ExperienceAggregate, len(results))
	for rank, result := range results {
		utility := contextstore.BetaQuantile(policy.PriorAlpha, policy.PriorBeta, policy.UtilityPercentile)
		positive, negative := 0.0, 0.0
		if experience != nil {
			if aggregate, err := experience.ExperienceAggregate(ctx, result.Item.ID, policy.PolicyVersion); err == nil {
				utility, positive, negative = aggregate.UtilityLowerBound, aggregate.PositiveWeight, aggregate.NegativeWeight
				aggregates[result.Item.ID] = new(aggregate)
			} else if !errors.Is(err, sql.ErrNoRows) {
				return nil, nil, nil, fmt.Errorf("load experience aggregate for %s: %w", result.Item.ID, err)
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
		leftDistance, rightDistance := memoryScopeDistance(left.Scope, requestScope), memoryScopeDistance(right.Scope, requestScope)
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
	return entries, scores, aggregates, nil
}
