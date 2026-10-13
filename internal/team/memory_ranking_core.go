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
	// SemanticQuery is the text the semantic lane embeds (the request
	// goal); empty means the whole Query.
	SemanticQuery string
	// Semantic is the optional semantic lane. Nil keeps the legacy
	// exact+lexical retrieval byte for byte (mode off).
	Semantic *persistentSemanticLane
}

// persistentSemanticLane configures the semantic lane of shared persistent
// ranking. Shadow embeds and traces without changing the selection; active
// fuses the vector results and narrows every lane to the authorized IDs
// before the candidate limit (spec §7.3).
type persistentSemanticLane struct {
	Vector contextstore.VectorSearcher
	Mode   contextstore.RetrievalMode
	// Router is set in auto mode: a request it routes lexical runs without
	// the lane, exactly as mode off.
	Router semanticRouter
	// Fusion replaces the ranking policy's fusion for active requests;
	// empty keeps the policy's.
	Fusion      contextstore.FusionMode
	TraceHasher contextstore.TraceHasher
	TraceSink   func(contextstore.SemanticRetrievalTrace)
	// Attribution stamps this request's traces with its run and task.
	Attribution SemanticTraceAttribution
}

// requestLane routes one ranking request: it returns the lane to use (nil for
// lexical only), the fusion, and the route ("" without a lane). It runs once
// per request so auto mode counts one decision even when the goal fallback
// retrieves twice.
func (in persistentRankingInput) requestLane() (*persistentSemanticLane, contextstore.FusionMode, string) {
	lane := in.Semantic
	if lane == nil || lane.Vector == nil {
		return nil, in.Ranking.Fusion, ""
	}
	text := in.SemanticQuery
	if strings.TrimSpace(text) == "" {
		text = in.Query
	}
	route := resolveSemanticRoute(lane.Mode, lane.Router, lane.Fusion, in.Ranking.Fusion, text)
	if route.Mode == contextstore.RetrievalOff {
		writeLexicalRouteTrace(lane.TraceHasher, lane.TraceSink, lane.Attribution, in.Query, text)
		return nil, route.Fusion, contextstore.SemanticRouteLexical
	}
	routed := *lane
	routed.Mode = route.Mode
	routed.TraceSink = attributedTraceSink(lane.TraceSink, lane.Attribution, text)
	return &routed, route.Fusion, contextstore.SemanticRouteSemantic
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
	// Semantic is the active semantic lane's identity and fallback reason
	// for the manifest; nil when the lane is off or shadow.
	Semantic *SemanticRetrievalIdentity
	// Route is how the request was routed: semantic, lexical (auto kept it
	// lexical), or empty without a semantic lane. Fusion is the fusion the
	// retrieval used. SemanticFallback is the lane's reason in shadow and
	// active alike; empty when the lane did not run.
	Route            string
	Fusion           contextstore.FusionMode
	SemanticFallback contextstore.SemanticFallbackReason
}

// memoryReinforcementError marks a failure while reading experience
// aggregates; the coordinator then keeps its base selection.
type memoryReinforcementError struct{ err error }

func (e memoryReinforcementError) Error() string { return e.err.Error() }
func (e memoryReinforcementError) Unwrap() error { return e.err }

// rankPersistentMemory is the shared-persistent memory ranking used for prompt
// selection. It performs no writes.
func rankPersistentMemory(ctx context.Context, repo contextstore.RetrievalRepository, experience contextstore.ExperienceRepository, in persistentRankingInput) (out persistentRankingResult, err error) {
	defer func() {
		if err == nil {
			captureContextQueryPrimary(ctx, in, out)
			if strings.TrimSpace(in.Query) != "" {
				observeRetrievalCandidates(ctx, out.Results, false)
			}
		}
	}()
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
	lane, fusion, route := in.requestLane()
	out.Route, out.Fusion = route, fusion
	retrieve := func(query string) ([]contextstore.SearchResult, error) {
		req := contextstore.SearchRequest{Query: query, Scope: persistentContextScope(in.RequestScope), Limit: candidateLimit}
		options := contextstore.HybridRetrievalOptions{Mode: contextstore.RetrievalActive, UnavailableReason: contextstore.SemanticFallbackProjectionMissing, Observer: in.Observer, Fusion: fusion, FusionCarriedWeight: in.Ranking.FusionCarriedWeight}
		var tracker *trackingVectorSearcher
		if lane != nil && lane.Vector != nil {
			tracker = &trackingVectorSearcher{delegate: lane.Vector}
			options.Mode, options.Vector, options.UnavailableReason = lane.Mode, tracker, ""
			options.TraceHasher, options.TraceSink = lane.TraceHasher, lane.TraceSink
			req.SemanticQuery = in.SemanticQuery
			if lane.Mode == contextstore.RetrievalActive && in.Allowed != nil {
				req = req.WithAllowedItemIDs(allowedIDList(in.Allowed))
			}
		}
		results, _, err := contextstore.HybridRetrieveWithOptions(ctx, repo, req, options)
		if err == nil && tracker != nil {
			out.SemanticFallback = trackedSemanticFallback(req, tracker)
			out.Semantic = semanticRetrievalIdentity(lane.Mode, lane.Vector, out.SemanticFallback)
		}
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
	normalizeFusedRelevance(results, fusion)
	return finishPersistentRanking(ctx, experience, in, out, results)
}

// finishPersistentRanking reconnects already-normalized candidates to the
// existing reinforcement and injection policy without executing retrieval.
func finishPersistentRanking(ctx context.Context, experience contextstore.ExperienceRepository, in persistentRankingInput, out persistentRankingResult, results []contextstore.SearchResult) (persistentRankingResult, error) {
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
// or more unscaled (BUG-01). The normalized fusions are already on the unit
// scale, so they are only clamped; multiplying again would saturate every
// score to 1.
func normalizeFusedRelevance(results []contextstore.SearchResult, fusion contextstore.FusionMode) {
	for i := range results {
		if fusion != "" && fusion != contextstore.FusionLegacy {
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

// allowedIDList returns the IDs a ranking may consider.
func allowedIDList(allowed map[string]bool) []string {
	ids := make([]string, 0, len(allowed))
	for id, ok := range allowed {
		if ok {
			ids = append(ids, id)
		}
	}
	return ids
}

// trackedSemanticFallback is the fallback reason of one hybrid retrieval
// whose vector lane was observed by tracker.
func trackedSemanticFallback(req contextstore.SearchRequest, tracker *trackingVectorSearcher) contextstore.SemanticFallbackReason {
	switch {
	case req.SemanticQueryText() == "":
		return contextstore.SemanticFallbackEmptyRemainder
	case tracker.called && tracker.err != nil:
		return contextstore.SemanticFallbackReasonForError(tracker.err)
	default:
		return contextstore.SemanticFallbackNone
	}
}
