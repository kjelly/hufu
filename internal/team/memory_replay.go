package team

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/kjelly/hufu/internal/agent"
	contextstore "github.com/kjelly/hufu/internal/context"
)

// RankingReplayQuery is one query to replay and where it came from
// ("flag", "file", or "session_task"). Its text is never reported.
type RankingReplayQuery struct {
	Text   string
	Source string
}

type RankingReplayInput struct {
	ProjectID       string
	TeamID          string
	PolicyVersion   string
	Queries         []RankingReplayQuery
	CandidateFusion contextstore.FusionMode
	// CandidateCarriedWeight is the score_normalized carried weight; 0 means
	// the default.
	CandidateCarriedWeight float64
	Now                    time.Time
}

// RankingReplayOutcome summarizes the experience evidence of selected items.
type RankingReplayOutcome struct {
	Selected            int     `json:"selected"`
	WithVerifiedSupport int     `json:"with_verified_support"`
	WithCausalFailure   int     `json:"with_causal_failure"`
	WithNegativeWeight  int     `json:"with_negative_weight"`
	MeanUtility         float64 `json:"mean_utility_lower_bound"`
	utilitySum          float64
}

// RankingReplayComparison compares one ranker's selection under the baseline
// and candidate fusion for one query.
type RankingReplayComparison struct {
	BaselineSelected          []string              `json:"baseline_selected"`
	CandidateSelected         []string              `json:"candidate_selected"`
	Added                     []string              `json:"added,omitempty"`
	Removed                   []string              `json:"removed,omitempty"`
	SetChanged                bool                  `json:"set_changed"`
	OrderChanged              bool                  `json:"order_changed"`
	Jaccard                   float64               `json:"jaccard"`
	BaselineMaxBaseRelevance  float64               `json:"baseline_max_base_relevance"`
	CandidateMaxBaseRelevance float64               `json:"candidate_max_base_relevance"`
	BaselineOutcome           *RankingReplayOutcome `json:"baseline_outcome,omitempty"`
	CandidateOutcome          *RankingReplayOutcome `json:"candidate_outcome,omitempty"`
}

type RankingReplayQueryResult struct {
	QueryHash  string                  `json:"query_hash"`
	Source     string                  `json:"source"`
	Relevance  RankingReplayComparison `json:"relevance"`
	Reinforced RankingReplayComparison `json:"reinforced"`
}

type RankingReplayRankerSummary struct {
	SetChangedQueries   int                   `json:"set_changed_queries"`
	OrderChangedQueries int                   `json:"order_changed_queries"`
	MeanJaccard         float64               `json:"mean_jaccard"`
	BaselineOutcome     *RankingReplayOutcome `json:"baseline_outcome,omitempty"`
	CandidateOutcome    *RankingReplayOutcome `json:"candidate_outcome,omitempty"`
}

// RankingReplayReport is the content-free comparison of the baseline and a
// candidate fusion over a set of queries. It contains IDs, query hashes,
// counts, and scores only.
type RankingReplayReport struct {
	SchemaVersion           int                        `json:"schema_version"`
	Scope                   contextstore.Scope         `json:"scope"`
	PolicyVersion           string                     `json:"policy_version"`
	ModeSource              string                     `json:"mode_source"`
	BaselineFusion          contextstore.FusionMode    `json:"baseline_fusion"`
	BaselineCarriedWeight   float64                    `json:"baseline_carried_weight,omitempty"`
	CandidateFusion         contextstore.FusionMode    `json:"candidate_fusion"`
	CandidateCarriedWeight  float64                    `json:"candidate_carried_weight,omitempty"`
	EligibleItems           int                        `json:"eligible_items"`
	ItemsWithAggregates     int                        `json:"items_with_aggregates"`
	OutcomeMetricsAvailable bool                       `json:"outcome_metrics_available"`
	QueryCount              int                        `json:"query_count"`
	Relevance               RankingReplayRankerSummary `json:"relevance"`
	Reinforced              RankingReplayRankerSummary `json:"reinforced"`
	Queries                 []RankingReplayQueryResult `json:"queries"`
}

// ReplayPersistentRanking reruns the shared-persistent ranking for each
// query under the policy's fusion and the candidate fusion, with both the
// relevance ranker (off, observe, shadow) and the reinforced ranker (active).
// It only reads: repo may be opened read-only, and no trace is written.
func ReplayPersistentRanking(ctx context.Context, repo *contextstore.SQLiteRepository, in RankingReplayInput) (RankingReplayReport, error) {
	scope := contextstore.Scope{ProjectID: strings.TrimSpace(in.ProjectID), TeamID: strings.TrimSpace(in.TeamID)}
	if scope.ProjectID == "" || scope.TeamID == "" {
		return RankingReplayReport{}, errors.New("project and team are required")
	}
	if in.CandidateFusion == "" || !contextstore.ValidFusionMode(in.CandidateFusion) {
		return RankingReplayReport{}, fmt.Errorf("unknown candidate fusion %q", in.CandidateFusion)
	}
	if !contextstore.ValidFusionCarriedWeight(in.CandidateCarriedWeight) {
		return RankingReplayReport{}, fmt.Errorf("candidate carried weight %v is outside [0,1]", in.CandidateCarriedWeight)
	}
	queries := make([]RankingReplayQuery, 0, len(in.Queries))
	for _, query := range in.Queries {
		if strings.TrimSpace(query.Text) != "" {
			queries = append(queries, query)
		}
	}
	if len(queries) == 0 {
		return RankingReplayReport{}, errors.New("at least one non-empty query is required")
	}
	now := in.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}
	learning, runtimePolicy, err := LoadMemoryPolicy(ctx, repo, in.PolicyVersion)
	if err != nil {
		return RankingReplayReport{}, err
	}
	modeSource, err := memoryPolicyModeSource(ctx, repo, in.PolicyVersion)
	if err != nil {
		return RankingReplayReport{}, err
	}
	baseline := effectiveRankingPolicy(runtimePolicy)
	candidate := baseline
	candidate.Fusion, candidate.FusionCarriedWeight = in.CandidateFusion, in.CandidateCarriedWeight
	eligible, allowed, err := eligiblePersistentMemory(ctx, repo, scope, now)
	if err != nil {
		return RankingReplayReport{}, err
	}
	aggregates := make(map[string]contextstore.ExperienceAggregate, len(eligible))
	for _, item := range eligible {
		aggregate, err := repo.ExperienceAggregate(ctx, item.ID, learning.PolicyVersion)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return RankingReplayReport{}, fmt.Errorf("load experience aggregate: %w", err)
		}
		aggregates[item.ID] = aggregate
	}
	report := RankingReplayReport{
		SchemaVersion: 1, Scope: scope, PolicyVersion: learning.PolicyVersion, ModeSource: modeSource,
		BaselineFusion: effectiveFusion(baseline.Fusion), BaselineCarriedWeight: effectiveCarriedWeight(baseline.Fusion, baseline.FusionCarriedWeight),
		CandidateFusion: in.CandidateFusion, CandidateCarriedWeight: effectiveCarriedWeight(candidate.Fusion, candidate.FusionCarriedWeight),
		EligibleItems: len(eligible), ItemsWithAggregates: len(aggregates), OutcomeMetricsAvailable: len(aggregates) > 0,
		QueryCount: len(queries), Queries: make([]RankingReplayQueryResult, 0, len(queries)),
	}
	prior := contextstore.BetaQuantile(learning.PriorAlpha, learning.PriorBeta, learning.UtilityPercentile)
	outcome := func(ids []string) *RankingReplayOutcome {
		if !report.OutcomeMetricsAvailable {
			return nil
		}
		return replayOutcome(ids, aggregates, prior)
	}
	for _, query := range queries {
		result := RankingReplayQueryResult{QueryHash: QueryHash(query.Text), Source: query.Source}
		for _, ranker := range []struct {
			mode agent.MemoryLearningMode
			out  *RankingReplayComparison
		}{{agent.MemoryLearningObserve, &result.Relevance}, {agent.MemoryLearningActive, &result.Reinforced}} {
			rankerLearning := learning
			rankerLearning.Mode = ranker.mode
			run := func(policy MemoryRuntimeRankingPolicy) (persistentRankingResult, error) {
				return rankPersistentMemory(ctx, repo, repo, persistentRankingInput{
					Query: query.Text, RequestScope: scope, Base: eligible, Allowed: allowed, Learning: rankerLearning, Ranking: policy,
				})
			}
			base, err := run(baseline)
			if err != nil {
				return RankingReplayReport{}, fmt.Errorf("replay baseline ranking: %w", err)
			}
			cand, err := run(candidate)
			if err != nil {
				return RankingReplayReport{}, fmt.Errorf("replay candidate ranking: %w", err)
			}
			*ranker.out = compareReplaySelections(base, cand)
			ranker.out.BaselineOutcome, ranker.out.CandidateOutcome = outcome(ranker.out.BaselineSelected), outcome(ranker.out.CandidateSelected)
		}
		report.Queries = append(report.Queries, result)
	}
	report.Relevance = summarizeReplay(report.Queries, func(r RankingReplayQueryResult) RankingReplayComparison { return r.Relevance }, report.OutcomeMetricsAvailable)
	report.Reinforced = summarizeReplay(report.Queries, func(r RankingReplayQueryResult) RankingReplayComparison { return r.Reinforced }, report.OutcomeMetricsAvailable)
	return report, nil
}

func compareReplaySelections(base, cand persistentRankingResult) RankingReplayComparison {
	out := RankingReplayComparison{BaselineSelected: selectedIDs(base.Selected), CandidateSelected: selectedIDs(cand.Selected)}
	inBase, inCand := map[string]bool{}, map[string]bool{}
	for _, id := range out.BaselineSelected {
		inBase[id] = true
	}
	for _, id := range out.CandidateSelected {
		inCand[id] = true
		if !inBase[id] {
			out.Added = append(out.Added, id)
		}
	}
	for _, id := range out.BaselineSelected {
		if !inCand[id] {
			out.Removed = append(out.Removed, id)
		}
	}
	union := len(inBase) + len(out.Added)
	out.Jaccard = 1
	if union > 0 {
		out.Jaccard = float64(len(inBase)-len(out.Removed)) / float64(union)
	}
	out.SetChanged = len(out.Added) > 0 || len(out.Removed) > 0
	out.OrderChanged = !slices.Equal(out.BaselineSelected, out.CandidateSelected)
	out.BaselineMaxBaseRelevance, out.CandidateMaxBaseRelevance = maxBaseRelevance(base.Scores), maxBaseRelevance(cand.Scores)
	return out
}

func selectedIDs(items []contextstore.ContextItem) []string {
	ids := make([]string, len(items))
	for i, item := range items {
		ids[i] = item.ID
	}
	return ids
}

func maxBaseRelevance(scores map[string]MemoryScoreParts) float64 {
	best := 0.0
	for _, parts := range scores {
		best = max(best, parts.BaseRelevance)
	}
	return best
}

func replayOutcome(ids []string, aggregates map[string]contextstore.ExperienceAggregate, prior float64) *RankingReplayOutcome {
	out := &RankingReplayOutcome{}
	for _, id := range ids {
		out.Selected++
		aggregate, ok := aggregates[id]
		if !ok {
			out.utilitySum += prior
			continue
		}
		out.utilitySum += aggregate.UtilityLowerBound
		if aggregate.VerifiedSupportCount > 0 {
			out.WithVerifiedSupport++
		}
		if aggregate.CausalFailureCount > 0 {
			out.WithCausalFailure++
		}
		if aggregate.NegativeWeight > 0 {
			out.WithNegativeWeight++
		}
	}
	if out.Selected > 0 {
		out.MeanUtility = out.utilitySum / float64(out.Selected)
	}
	return out
}

func summarizeReplay(results []RankingReplayQueryResult, pick func(RankingReplayQueryResult) RankingReplayComparison, withOutcomes bool) RankingReplayRankerSummary {
	var out RankingReplayRankerSummary
	if withOutcomes {
		out.BaselineOutcome, out.CandidateOutcome = &RankingReplayOutcome{}, &RankingReplayOutcome{}
	}
	for _, result := range results {
		comparison := pick(result)
		if comparison.SetChanged {
			out.SetChangedQueries++
		}
		if comparison.OrderChanged {
			out.OrderChangedQueries++
		}
		out.MeanJaccard += comparison.Jaccard
		if withOutcomes {
			addReplayOutcome(out.BaselineOutcome, comparison.BaselineOutcome)
			addReplayOutcome(out.CandidateOutcome, comparison.CandidateOutcome)
		}
	}
	if len(results) > 0 {
		out.MeanJaccard /= float64(len(results))
	}
	for _, total := range []*RankingReplayOutcome{out.BaselineOutcome, out.CandidateOutcome} {
		if total != nil && total.Selected > 0 {
			total.MeanUtility = total.utilitySum / float64(total.Selected)
		}
	}
	return out
}

func addReplayOutcome(total, add *RankingReplayOutcome) {
	if total == nil || add == nil {
		return
	}
	total.Selected += add.Selected
	total.WithVerifiedSupport += add.WithVerifiedSupport
	total.WithCausalFailure += add.WithCausalFailure
	total.WithNegativeWeight += add.WithNegativeWeight
	total.utilitySum += add.utilitySum
}

// SessionReplayQueries rebuilds approximate dispatch retrieval queries from
// the goals (or descriptions) of the tasks recorded in a workspace session.
// Injection manifests only keep query hashes, so this is a reconstruction.
func SessionReplayQueries(workspace string) []RankingReplayQuery {
	session := LoadSession(workspace)
	if session == nil {
		return nil
	}
	seen := map[string]bool{}
	var queries []RankingReplayQuery
	for _, task := range session.Tasks {
		if task == nil {
			continue
		}
		goal := strings.TrimSpace(task.Goal)
		if goal == "" {
			goal = strings.TrimSpace(task.Desc)
		}
		if goal == "" {
			continue
		}
		phase := task.Phase
		if phase == "" {
			phase = PhaseExecute
		}
		query := ContextRequest{Goal: goal, Phase: phase, Trigger: ContextTriggerTaskDispatch}.RetrievalQuery()
		if !seen[query] {
			seen[query] = true
			queries = append(queries, RankingReplayQuery{Text: query, Source: "session_task"})
		}
	}
	return queries
}
