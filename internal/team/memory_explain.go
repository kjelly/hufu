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

// MemoryExplainReason is a fixed code for why a memory was or was not
// selected by the recomputed shared-persistent ranking.
type MemoryExplainReason string

const (
	MemoryExplainSelected               MemoryExplainReason = "selected"
	MemoryExplainMustKeepForced         MemoryExplainReason = "must_keep_forced"
	MemoryExplainOutsidePersistentScope MemoryExplainReason = "outside_persistent_scope"
	MemoryExplainLifecycleIneligible    MemoryExplainReason = "lifecycle_ineligible"
	MemoryExplainExpired                MemoryExplainReason = "expired"
	MemoryExplainEnvironmentMismatch    MemoryExplainReason = "environment_mismatch"
	// MemoryExplainOutsideObserved means no retrieval path returned the item
	// within its limits; it does not mean the item is irrelevant.
	MemoryExplainOutsideObserved  MemoryExplainReason = "outside_observed_candidates"
	MemoryExplainDuplicateContent MemoryExplainReason = "duplicate_content"
	MemoryExplainRetrievalLimit   MemoryExplainReason = "retrieval_limit"
	MemoryExplainCandidateLimit   MemoryExplainReason = "candidate_limit"
	MemoryExplainBelowRelevance   MemoryExplainReason = "below_relevance"
	MemoryExplainHarmfulUse       MemoryExplainReason = "harmful_use"
	MemoryExplainNonPositiveScore MemoryExplainReason = "non_positive_score"
	MemoryExplainInjectLimit      MemoryExplainReason = "inject_limit"
)

// Checks explain-memory cannot evaluate without a live context request.
const (
	memoryExplainNotEvaluatedGates  = "activation"
	memoryExplainNotEvaluatedBudget = "token_budget"
)

// ErrMemoryExplainNotFound covers both a missing item and one outside the
// requested scope, so explain-memory never reveals another scope's items.
var ErrMemoryExplainNotFound = errors.New("context item not found")

type MemoryExplainInput struct {
	Workspace     string
	ItemID        string
	ProjectID     string
	TeamID        string
	Query         string
	PolicyVersion string
	Now           time.Time
}

type MemoryExplainRetrieval struct {
	Paths                  []contextstore.RetrievalPathObservation     `json:"paths"`
	Candidate              *contextstore.RetrievalCandidateObservation `json:"candidate,omitempty"`
	ObservedCandidateCount int                                         `json:"observed_candidate_count"`
}

type MemoryExplainRanking struct {
	BaseRank           int  `json:"base_rank,omitempty"`
	RelevanceRank      int  `json:"relevance_rank,omitempty"`
	RelevanceSelected  bool `json:"relevance_selected"`
	ReinforcedRank     int  `json:"reinforced_rank,omitempty"`
	ReinforcedSelected bool `json:"reinforced_selected"`
	// Selected is the ranking decision under the effective mode; Injected
	// also counts must-keep items the router adds back.
	Selected bool `json:"selected"`
	Injected bool `json:"injected"`
}

// MemoryExplanation recomputes, from current data, how the shared-persistent
// ranking treats one item for one query. It is never persisted and never
// contains memory content or the query text.
type MemoryExplanation struct {
	SchemaVersion int  `json:"schema_version"`
	Recomputed    bool `json:"recomputed"`
	MemoryScoreExplanation
	Scope            contextstore.Scope       `json:"scope"`
	Mode             agent.MemoryLearningMode `json:"mode"`
	ModeSource       string                   `json:"mode_source"`
	QueryHash        string                   `json:"query_hash"`
	UsedGoalFallback bool                     `json:"used_goal_fallback"`
	Retrieval        MemoryExplainRetrieval   `json:"retrieval"`
	Ranking          MemoryExplainRanking     `json:"ranking"`
	Reasons          []MemoryExplainReason    `json:"reasons"`
	NotEvaluated     []string                 `json:"not_evaluated"`
}

// ExplainPersistentMemory reruns the runtime shared-persistent ranking without
// side effects and explains one item's retrieval, fusion, and selection.
// Request-dependent activation gates and the compiler token budget are not
// evaluated and are listed in NotEvaluated.
func ExplainPersistentMemory(ctx context.Context, repo *contextstore.SQLiteRepository, in MemoryExplainInput) (MemoryExplanation, error) {
	scope := contextstore.Scope{ProjectID: strings.TrimSpace(in.ProjectID), TeamID: strings.TrimSpace(in.TeamID)}
	if scope.ProjectID == "" || scope.TeamID == "" || strings.TrimSpace(in.Query) == "" {
		return MemoryExplanation{}, errors.New("project, team, and query are required")
	}
	now := in.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}
	item, err := repo.GetScoped(ctx, in.ItemID, contextstore.ScopedReadOptions{Scope: scope})
	if errors.Is(err, contextstore.ErrReadScopeDenied) || errors.Is(err, sql.ErrNoRows) {
		return MemoryExplanation{}, fmt.Errorf("%w: context item %q was not found in project %q team %q", ErrMemoryExplainNotFound, in.ItemID, scope.ProjectID, scope.TeamID)
	}
	if err != nil {
		return MemoryExplanation{}, fmt.Errorf("load context item: %w", err)
	}
	learning, runtimePolicy, err := LoadMemoryPolicy(ctx, repo, in.PolicyVersion)
	if err != nil {
		return MemoryExplanation{}, err
	}
	modeSource, err := memoryPolicyModeSource(ctx, repo, in.PolicyVersion)
	if err != nil {
		return MemoryExplanation{}, err
	}
	ranking := effectiveRankingPolicy(runtimePolicy)
	persistent, err := repo.QuerySharedPersistentProjection(ctx, scope)
	if err != nil {
		return MemoryExplanation{}, fmt.Errorf("query shared persistent memory: %w", err)
	}
	eligible := make([]contextstore.ContextItem, 0, len(persistent))
	allowed := make(map[string]bool, len(persistent))
	for _, candidate := range persistent {
		if evaluateLifecycleEligibility(candidate, "", now) == "" {
			eligible = append(eligible, candidate)
			allowed[candidate.ID] = true
		}
	}
	observation := &contextstore.RetrievalObservation{}
	ranked, err := rankPersistentMemory(ctx, repo, repo, persistentRankingInput{
		Query: in.Query, RequestScope: scope, Base: eligible, Allowed: allowed,
		Learning: learning, Ranking: ranking, Observer: observation,
	})
	if err != nil {
		return MemoryExplanation{}, fmt.Errorf("recompute memory ranking: %w", err)
	}
	reinforced := ranked.ReinforcedEntries
	if reinforced == nil && len(ranked.Results) > 0 {
		// Off and observe modes rank by relevance only; compute the reinforced
		// score for display without letting it affect the decision.
		if reinforced, _, _, err = reinforceSearchResultsWith(ctx, repo, ranked.Results, learning, ranking, scope); err != nil {
			return MemoryExplanation{}, fmt.Errorf("compute reinforced memory score: %w", err)
		}
	}
	out := MemoryExplanation{
		SchemaVersion: 2, Recomputed: true, Scope: scope, Mode: learning.Mode, ModeSource: modeSource,
		QueryHash: QueryHash(in.Query), UsedGoalFallback: ranked.UsedGoalFallback,
		Retrieval:    MemoryExplainRetrieval{Paths: observation.Paths, ObservedCandidateCount: len(observation.Candidates)},
		NotEvaluated: []string{memoryExplainNotEvaluatedGates, memoryExplainNotEvaluatedBudget},
	}
	if candidate, ok := observation.Candidate(item.ID); ok {
		out.Retrieval.Candidate = &candidate
	}
	relevanceEntry, inRelevance := memoryRankingEntry(ranked.RelevanceEntries, item.ID)
	reinforcedEntry, inReinforced := memoryRankingEntry(reinforced, item.ID)
	aggregate, err := repo.ExperienceAggregate(ctx, item.ID, learning.PolicyVersion)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return MemoryExplanation{}, fmt.Errorf("load experience aggregate: %w", err)
	}
	out.MemoryScoreExplanation = ExplainMemoryScoreWithPolicy(item, 0, aggregate, learning, ranking)
	if inReinforced {
		out.ScoreParts, out.FinalScore = reinforcedEntry.ScoreParts, reinforcedEntry.FinalScore
	}
	out.RetrievalID = RetrievalIDForItem(in.Workspace, learning.PolicyVersion, out.QueryHash, item.ID)
	out.Ranking = MemoryExplainRanking{
		BaseRank: relevanceEntry.BaseRank, RelevanceRank: relevanceEntry.FinalRank, RelevanceSelected: inRelevance && relevanceEntry.Selected,
		ReinforcedRank: reinforcedEntry.FinalRank, ReinforcedSelected: inReinforced && reinforcedEntry.Selected,
	}
	if learning.Mode == agent.MemoryLearningActive {
		out.Ranking.Selected = out.Ranking.ReinforcedSelected
	} else {
		out.Ranking.Selected = out.Ranking.RelevanceSelected
	}
	out.Reasons = memoryExplainReasons(item, allowed[item.ID], out.Retrieval.Candidate, ranked, learning.Mode, ranking, relevanceEntry, inRelevance, reinforcedEntry, inReinforced, out.Ranking.Selected, now)
	out.Ranking.Injected = out.Ranking.Selected || slices.Contains(out.Reasons, MemoryExplainMustKeepForced)
	return out, nil
}

func memoryExplainReasons(item contextstore.ContextItem, eligible bool, candidate *contextstore.RetrievalCandidateObservation, ranked persistentRankingResult, mode agent.MemoryLearningMode, ranking MemoryRuntimeRankingPolicy, relevance MemoryRankingEntry, inRelevance bool, reinforced MemoryRankingEntry, inReinforced, selected bool, now time.Time) []MemoryExplainReason {
	if selected {
		return []MemoryExplainReason{MemoryExplainSelected}
	}
	var reasons []MemoryExplainReason
	if item.Scope.SessionID != "" || item.Scope.BranchID != "" || item.Scope.AgentID != "" || item.Scope.TaskID != "" || item.Scope.AttemptID != "" {
		reasons = append(reasons, MemoryExplainOutsidePersistentScope)
	}
	switch evaluateLifecycleEligibility(item, "", now) {
	case ContextOmittedLifecycle:
		reasons = append(reasons, MemoryExplainLifecycleIneligible)
	case ContextOmittedExpired:
		reasons = append(reasons, MemoryExplainExpired)
	case ContextOmittedEnvironment:
		reasons = append(reasons, MemoryExplainEnvironmentMismatch)
	}
	if !eligible {
		if len(reasons) == 0 {
			reasons = append(reasons, MemoryExplainLifecycleIneligible)
		}
		return reasons
	}
	if item.MustKeep {
		reasons = append(reasons, MemoryExplainMustKeepForced)
	}
	switch {
	case candidate == nil:
		return append(reasons, MemoryExplainOutsideObserved)
	case candidate.DuplicateOf != "":
		return append(reasons, MemoryExplainDuplicateContent)
	case candidate.CutByLimit:
		return append(reasons, MemoryExplainRetrievalLimit)
	case slices.Contains(ranked.CandidateCutIDs, item.ID):
		return append(reasons, MemoryExplainCandidateLimit)
	}
	entry, inEntries := relevance, inRelevance
	if mode == agent.MemoryLearningActive {
		entry, inEntries = reinforced, inReinforced
	}
	if !inEntries {
		return append(reasons, MemoryExplainOutsideObserved)
	}
	before := len(reasons)
	if entry.ScoreParts.BaseRelevance < ranking.MinimumRelevance {
		reasons = append(reasons, MemoryExplainBelowRelevance)
	}
	if mode == agent.MemoryLearningActive && entry.ScoreParts.HarmfulUsePenalty > 0 {
		reasons = append(reasons, MemoryExplainHarmfulUse)
	}
	if entry.ScoreParts.StaleEnvironmentPenalty > 0 {
		reasons = append(reasons, MemoryExplainEnvironmentMismatch)
	}
	if mode == agent.MemoryLearningActive && entry.FinalScore <= 0 {
		reasons = append(reasons, MemoryExplainNonPositiveScore)
	}
	if len(reasons) == before {
		reasons = append(reasons, MemoryExplainInjectLimit)
	}
	return reasons
}

func memoryRankingEntry(entries []MemoryRankingEntry, id string) (MemoryRankingEntry, bool) {
	for _, entry := range entries {
		if entry.ContextItemID == id {
			return entry, true
		}
	}
	return MemoryRankingEntry{}, false
}

// memoryPolicyModeSource reports whether the effective mode comes from a
// recorded policy snapshot or from the defaults used when none is adopted.
func memoryPolicyModeSource(ctx context.Context, repo *contextstore.SQLiteRepository, policyVersion string) (string, error) {
	if strings.TrimSpace(policyVersion) != "" {
		return "policy_snapshot", nil
	}
	if _, err := repo.ActiveMemoryPolicyVersion(ctx); errors.Is(err, sql.ErrNoRows) {
		return "default", nil
	} else if err != nil {
		return "", fmt.Errorf("load active memory policy: %w", err)
	}
	return "policy_snapshot", nil
}
