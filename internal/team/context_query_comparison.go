package team

import (
	"context"
	json "encoding/json/v2"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/kjelly/hufu/internal/agent"
	contextstore "github.com/kjelly/hufu/internal/context"
)

type ContextQueryRankingArm struct {
	Candidates  []contextstore.SearchResult
	Selected    []contextstore.ContextItem
	Scores      map[string]MemoryScoreParts
	FinalScores map[string]float64
	Aggregates  map[string]*contextstore.ExperienceAggregate
}

type ContextQueryRankingComparison struct {
	SnapshotHash string
	Legacy       ContextQueryRankingArm
	Candidate    ContextQueryRankingArm
	Selection    ContextQuerySelection
}

// CompareContextQueryRanking is offline and read-only. Callers supply the same
// canonical eligible snapshot used by routing. Both arms share ONE immutable
// primary retrieval, while the supplement independently queries that universe.
func CompareContextQueryRanking(ctx context.Context, repo contextstore.RetrievalRepository, experience contextstore.ExperienceRepository, request ContextRequest, plan ContextQueryPlan, scope contextstore.Scope, eligible []contextstore.ContextItem, learning agent.MemoryLearningPolicy, ranking MemoryRuntimeRankingPolicy) (ContextQueryRankingComparison, error) {
	if plan.Primary != request.RetrievalQuery() || plan.PrimaryHash != contextQueryDigest(plan.Primary) || plan.PolicyVersion != ContextQueryPolicyVersion {
		return ContextQueryRankingComparison{}, fmt.Errorf("query plan does not match its admitted primary")
	}
	revisionReader, ok := repo.(interface {
		Revision(context.Context) (int64, error)
	})
	if !ok {
		return ContextQueryRankingComparison{}, fmt.Errorf("offline query comparison requires canonical source revision")
	}
	initialRevision, err := revisionReader.Revision(ctx)
	if err != nil {
		return ContextQueryRankingComparison{}, err
	}
	allowed := make(map[string]bool, len(eligible))
	canonical := make(map[string]contextstore.ContextItem, len(eligible))
	for _, item := range eligible {
		allowed[item.ID] = true
		canonical[item.ID] = item
	}
	input := persistentRankingInput{Query: request.RetrievalQuery(), RequestScope: scope, Base: eligible, Allowed: allowed, Learning: learning, Ranking: ranking}
	legacy, err := rankPersistentMemory(ctx, repo, experience, input)
	if err != nil {
		return ContextQueryRankingComparison{}, err
	}
	comparison := ContextQueryRankingComparison{SnapshotHash: contextQuerySnapshotDigest(eligible), Legacy: queryRankingArm(legacy), Candidate: queryRankingArm(legacy)}
	source := ContextQuerySource{Kind: QuerySharedPersistent, Request: contextstore.SearchRequest{Scope: persistentContextScope(scope), Limit: ranking.CandidateTopK}, Primary: contextstore.CloneSearchResults(legacy.Results), Eligible: canonical, Revision: strconv.FormatInt(initialRevision, 10)}
	selection, err := SelectContextQuerySources(ctx, repo, plan, []ContextQuerySource{source}, func(ctx context.Context, _ ContextQuerySource) (string, error) {
		revision, err := revisionReader.Revision(ctx)
		return strconv.FormatInt(revision, 10), err
	})
	if err != nil {
		return comparison, err
	}
	comparison.Selection = selection
	if selection.Attribution.Mode != QueryModeActive {
		return comparison, nil
	}
	candidate, err := finishPersistentRanking(ctx, experience, input, persistentRankingResult{}, selection.Sources[0].Results)
	if err != nil {
		return comparison, err
	}
	comparison.Candidate = queryRankingArm(candidate)
	return comparison, nil
}

func queryRankingArm(result persistentRankingResult) ContextQueryRankingArm {
	rows := make([]contextstore.SearchResult, 0, len(result.Selected))
	for _, item := range result.Selected {
		rows = append(rows, contextstore.SearchResult{Item: item})
	}
	rows = contextstore.CloneSearchResults(rows)
	selected := make([]contextstore.ContextItem, len(rows))
	for index, row := range rows {
		selected[index] = row.Item
	}
	aggregates := maps.Clone(result.Aggregates)
	for id, aggregate := range aggregates {
		if aggregate != nil {
			aggregates[id] = new(*aggregate)
		}
	}
	return ContextQueryRankingArm{Candidates: contextstore.CloneSearchResults(result.Results), Selected: selected, Scores: maps.Clone(result.Scores), FinalScores: maps.Clone(result.FinalScores), Aggregates: aggregates}
}

func contextQuerySnapshotDigest(items []contextstore.ContextItem) string {
	rows := make([]contextstore.SearchResult, len(items))
	for index, item := range items {
		rows[index] = contextstore.SearchResult{Item: item}
	}
	rows = contextstore.CloneSearchResults(rows)
	slices.SortFunc(rows, func(a, b contextstore.SearchResult) int { return strings.Compare(a.Item.ID, b.Item.ID) })
	data, err := json.Marshal(rows, json.Deterministic(true))
	if err != nil {
		panic(err)
	}
	return contextQueryDigest(string(data))
}
