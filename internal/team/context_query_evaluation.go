package team

import (
	"context"
	"fmt"
	"slices"
	"strconv"

	"github.com/kjelly/hufu/internal/agent"
	contextstore "github.com/kjelly/hufu/internal/context"
)

type contextQueryEvaluationKey struct{}

// WithContextQueryEvaluationProfile is an offline harness seam. It has no CLI
// or CLI setting. An absent override uses the team's frozen policy.
func WithContextQueryEvaluationProfile(ctx context.Context, profile string) (context.Context, error) {
	switch profile {
	case "":
		return ctx, nil
	case "legacy":
	case "candidate":
	default:
		return nil, fmt.Errorf("query-profile-override must be legacy or candidate")
	}
	return context.WithValue(ctx, contextQueryEvaluationKey{}, profile), nil
}

func evaluatingContextQuery(ctx context.Context) bool {
	profile, _ := ctx.Value(contextQueryEvaluationKey{}).(string)
	return profile == "candidate"
}

type contextQueryPrimaryCaptureKey struct{}
type contextQuerySharedSnapshot struct {
	source ContextQuerySource
	input  persistentRankingInput
	legacy persistentRankingResult
}

func captureContextQueryPrimary(ctx context.Context, input persistentRankingInput, result persistentRankingResult) {
	if capture, ok := ctx.Value(contextQueryPrimaryCaptureKey{}).(func(persistentRankingInput, persistentRankingResult)); ok {
		capture(input, result)
	}
}

func (c *Coordinator) beginSharedQuerySnapshot(ctx context.Context) (context.Context, *contextQuerySharedSnapshot) {
	if !evaluatingContextQuery(ctx) {
		return ctx, nil
	}
	revision, err := c.contextRepo.Revision(ctx)
	snapshot := &contextQuerySharedSnapshot{source: ContextQuerySource{Kind: QuerySharedPersistent, Request: contextstore.SearchRequest{Scope: persistentContextScope(c.contextScope()), Limit: c.effectiveMemoryRankingPolicy().CandidateTopK}, Revision: strconv.FormatInt(revision, 10), Unavailable: err != nil}}
	return context.WithValue(ctx, contextQueryPrimaryCaptureKey{}, func(input persistentRankingInput, result persistentRankingResult) {
		snapshot.input, snapshot.legacy = input, result
		snapshot.source.Primary = contextstore.CloneSearchResults(result.Results)
		snapshot.source.Eligible = make(map[string]contextstore.ContextItem, len(input.Base))
		for _, row := range contextstore.CloneSearchResults(contextItemsAsSearchResults(input.Base)) {
			snapshot.source.Eligible[row.Item.ID] = row.Item
		}
	}), snapshot
}

func contextItemsAsSearchResults(items []contextstore.ContextItem) []contextstore.SearchResult {
	rows := make([]contextstore.SearchResult, len(items))
	for index, item := range items {
		rows[index].Item = item
	}
	return rows
}

// evaluateWorkerContextQueries is called once after ordinary shared/private
// recall and before compilation. It stages every participating source together;
// it never performs another primary/vector call or mutates the legacy bundles.
func (c *Coordinator) evaluateWorkerContextQueries(ctx context.Context, input WorkerContextInput) (WorkerContextInput, *ContextQueryAttribution, error) {
	if !evaluatingContextQuery(ctx) {
		return input, nil, nil
	}
	if err := ctx.Err(); err != nil {
		return input, nil, err
	}
	ctx = withContextQueryIdentity(ctx, input.Request)
	shared := (*contextQuerySharedSnapshot)(nil)
	if input.CanonicalMemory != nil {
		shared = input.CanonicalMemory.querySnapshot
	}
	private := (*contextQueryPrivateSnapshot)(nil)
	if input.WorkerMemory != nil {
		private = input.WorkerMemory.querySnapshot
	}
	sources := c.queryEvaluationSources(input, shared, private)
	hints := ContextQueryHints{}
	if shared != nil || private != nil {
		hints = c.resolveContextQueryHints(ctx, input.Request)
	}
	plan := BuildContextQueryPlan(input.Request, hints)
	var revision func(context.Context, ContextQuerySource) (string, error)
	if c.contextRepo != nil {
		revision = func(ctx context.Context, _ ContextQuerySource) (string, error) {
			current, err := c.contextRepo.Revision(ctx)
			return strconv.FormatInt(current, 10), err
		}
	}
	child, cancel := context.WithTimeout(ctx, contextQuerySupplementalTimeout)
	defer cancel()
	var preparationErr error
	if plan.Supplemental != "" && plan.Fallback == QueryFallbackNone {
		preparationErr = c.preparePrivateQueryEligibility(child, input.Request, sources)
	}
	if ctx.Err() != nil {
		return input, nil, ctx.Err()
	}
	var selection ContextQuerySelection
	var err error
	if preparationErr != nil {
		selection, err = queryLegacyRollback(plan, sources, querySupplementalFailure(child, preparationErr))
	} else {
		selection, err = selectContextQuerySources(ctx, child, c.contextRepo, plan, sources, revision)
	}
	if err != nil {
		return input, nil, err
	}
	for _, source := range selection.Sources {
		attribution := querySourceAttribution(selection.Attribution, source.Source)
		if source.Source == QuerySharedPersistent && attribution.Mode == QueryModeActive && shared != nil {
			experience, _ := c.contextRepo.(contextstore.ExperienceRepository)
			ranked, err := finishPersistentRanking(ctx, experience, shared.input, persistentRankingResult{}, source.Results)
			if err != nil {
				return input, nil, err
			}
			bundle := *input.CanonicalMemory
			bundle.SharedPersistent, bundle.SharedPersistentScores, bundle.SharedPersistentFinalScores, bundle.SharedPersistentAggregates = ranked.Selected, ranked.Scores, ranked.FinalScores, ranked.Aggregates
			// Required memory follows the original route independently of optional K.
			for _, item := range input.CanonicalMemory.SharedPersistent {
				if item.MustKeep && !slices.ContainsFunc(bundle.SharedPersistent, func(row contextstore.ContextItem) bool { return row.ID == item.ID }) {
					bundle.SharedPersistent = append(bundle.SharedPersistent, item)
					if bundle.SharedPersistentScores == nil {
						bundle.SharedPersistentScores = make(map[string]MemoryScoreParts)
					}
					if bundle.SharedPersistentFinalScores == nil {
						bundle.SharedPersistentFinalScores = make(map[string]float64)
					}
					bundle.SharedPersistentScores[item.ID] = input.CanonicalMemory.SharedPersistentScores[item.ID]
					bundle.SharedPersistentFinalScores[item.ID] = input.CanonicalMemory.SharedPersistentFinalScores[item.ID]
				}
			}
			input.CanonicalMemory = &bundle
		}
	}
	if private != nil && (querySourceAttribution(selection.Attribution, QueryPrivateSession).Mode == QueryModeActive || querySourceAttribution(selection.Attribution, QueryPrivatePersistent).Mode == QueryModeActive) {
		bundle := private.mergeSelection(selection, input.WorkerMemory)
		c.rerankWorkerMemory(ctx, &bundle, input.Request.RetrievalQuery())
		input.WorkerMemory = &bundle
	}
	observeEffectiveQuerySelection(ctx, input)
	if shared != nil || private != nil {
		var candidates []contextstore.SearchResult
		for _, source := range selection.Sources {
			candidates = append(candidates, source.Results...)
		}
		observeRetrievalCandidates(ctx, candidates, true)
	}
	return input, &selection.Attribution, nil
}

func querySourceAttribution(attribution ContextQueryAttribution, kind ContextQuerySourceKind) ContextQuerySourceAttribution {
	for _, source := range attribution.Sources {
		if source.Source == kind {
			return source
		}
	}
	return ContextQuerySourceAttribution{Source: kind, Mode: QueryModeNotApplicable, Fallback: QueryFallbackMemoryDisabled}
}

func (c *Coordinator) queryEvaluationSources(input WorkerContextInput, shared *contextQuerySharedSnapshot, private *contextQueryPrivateSnapshot) []ContextQuerySource {
	scope := c.contextScope()
	sources := []ContextQuerySource{{Kind: QuerySharedPersistent, Request: contextstore.SearchRequest{Scope: persistentContextScope(scope), Limit: c.effectiveMemoryRankingPolicy().CandidateTopK}}}
	if shared != nil {
		sources[0] = shared.source
	} else if input.DisableCanonicalMemory || c.contextRepo == nil {
		sources[0].Disabled = QueryFallbackMemoryDisabled
	} else {
		sources[0].Unavailable = true
	}
	if private != nil {
		sources = append(sources, private.sources...)
		for index := range sources {
			if sources[index].Kind != QuerySharedPersistent {
				// Keep original primary overfetch, but bound active lane merge
				// by the existing candidate policy rather than the lineage fetch.
				sources[index].Request.Limit = min(sources[index].Request.Limit, c.effectiveMemoryRankingPolicy().CandidateTopK)
			}
		}
		return sources
	}
	workerScope := scope
	if input.AgentDef != nil {
		workerScope = resolveWorkerScope(scope, input.AgentDef, c.activeBranchID())
	}
	for _, kind := range []ContextQuerySourceKind{QueryPrivateSession, QueryPrivatePersistent} {
		sourceScope := workerScope
		if kind == QueryPrivatePersistent {
			sourceScope.SessionID, sourceScope.BranchID = "", ""
		}
		source := ContextQuerySource{Kind: kind, Request: contextstore.SearchRequest{Scope: sourceScope, Limit: max(1, c.effectiveMemoryRankingPolicy().CandidateTopK)}}
		switch {
		case !workerMemoryPhaseAllowed(input.Request.Phase):
			source.Disabled = QueryFallbackHistoryNotAllowed
		case input.AgentDef == nil || input.AgentDef.Memory.Mode == agent.WorkerMemoryOff || input.AgentDef.Memory.Mode == "":
			source.Disabled = QueryFallbackMemoryDisabled
		case !shouldRecallWorkerMemory(input.AgentDef, c.ExecutionProfile()):
			source.Disabled = QueryFallbackHistoryNotAllowed
		case kind == QueryPrivatePersistent && input.AgentDef.Memory.Mode != agent.WorkerMemoryPersistent:
			source.Disabled = QueryFallbackMemoryDisabled
		default:
			source.Unavailable = true
		}
		sources = append(sources, source)
	}
	return sources
}
