package context

import (
	"context"
	"errors"
	"maps"
	"slices"
	"time"
)

const SupplementalMaxExactTerms = 8
const SupplementalMaxSourceRows = 64

var ErrSupplementalSourceLimit = errors.New("supplemental source exceeded its requested row limit")

type SupplementalRetrieval struct {
	Results        []SearchResult
	Work           QueryWork
	TruncatedTerms int
}

// RetrieveSupplemental uses the existing exact/lexical/fusion/MMR helpers,
// with no vector capability and one cumulative row allowance per scope. The
// caller owns the shared invocation deadline and cross-source transaction.
func RetrieveSupplemental(ctx context.Context, repo RetrievalRepository, request SearchRequest) (out SupplementalRetrieval, err error) {
	started := time.Now()
	defer func() {
		out.Work.Elapsed = time.Since(started)
		observeQueryWork(ctx, out.Work)
	}()
	if err := ctx.Err(); err != nil {
		return out, err
	}
	parts := DecomposeQuery(request.Query)
	var terms []string
	for _, values := range [][]string{parts.Quoted, parts.Paths, parts.Symbols, parts.Commands, parts.SHAs, parts.ErrorCodes, parts.TaskIDs, parts.AttemptIDs, parts.ToolNames, parts.IPs, parts.Ports, parts.ArtifactIDs} {
		terms = append(terms, values...)
	}
	slices.Sort(terms)
	terms = slices.Compact(terms)
	if len(terms) > SupplementalMaxExactTerms {
		out.TruncatedTerms = len(terms) - SupplementalMaxExactTerms
		terms = terms[:SupplementalMaxExactTerms]
	}
	remaining := SupplementalMaxSourceRows
	var exact []SearchResult
	eligible := func(rows []SearchResult) []SearchResult {
		var result []SearchResult
		for _, row := range filterSearchResults(rows, request) {
			if !isRetrievable(row.Item, request, EvaluationTime(ctx)) {
				continue
			}
			if request.AllowedItemIDs != nil && !slices.Contains(request.AllowedItemIDs, row.Item.ID) {
				continue
			}
			result = append(result, row)
		}
		return CloneSearchResults(result)
	}
	for _, term := range terms {
		if remaining == 0 {
			break
		}
		if err := ctx.Err(); err != nil {
			return out, err
		}
		query := request
		query.Query, query.Limit = term, remaining
		out.Work.ExactCalls++
		rows, err := repo.SearchExact(ctx, query)
		out.Work.CandidateRows += len(rows)
		if err != nil {
			return out, err
		}
		if len(rows) > remaining {
			return out, ErrSupplementalSourceLimit
		}
		remaining -= len(rows)
		exact = mergeResults(exact, eligible(rows))
	}
	var lexical []SearchResult
	if remaining > 0 {
		if err := ctx.Err(); err != nil {
			return out, err
		}
		query := request
		query.Limit = remaining
		out.Work.LexicalCalls++
		search := repo.SearchLexical
		if fixed, ok := repo.(interface {
			SearchSupplementalLexical(context.Context, SearchRequest) ([]SearchResult, error)
		}); ok {
			search = fixed.SearchSupplementalLexical
		}
		rows, err := search(ctx, query)
		out.Work.CandidateRows += len(rows)
		if err != nil {
			return out, err
		}
		if len(rows) > remaining {
			return out, ErrSupplementalSourceLimit
		}
		lexical = eligible(rows)
	}
	if err := ctx.Err(); err != nil {
		return out, err
	}
	fused := rrfObserved(nil, []string{"lexical"}, fusionConfig{mode: FusionLegacy}, lexical)
	fused, err = applyMMRObservedContext(ctx, nil, rankForScope(fused, request.Scope), .75)
	if err != nil {
		return out, err
	}
	out.Results = mergeResults(rankForScope(exact, request.Scope), fused)
	out.Results, _ = applyFilePathBoost(out.Results, request.FilePaths)
	limit := SupplementalMaxSourceRows
	if request.Limit > 0 {
		limit = min(limit, request.Limit)
	}
	if len(out.Results) > limit {
		out.Results = out.Results[:limit]
	}
	out.Work.Elapsed = time.Since(started)
	return out, nil
}

// CloneSearchResults separates both arm slices and mutable item metadata.
func CloneSearchResults(results []SearchResult) []SearchResult {
	out := slices.Clone(results)
	for index := range out {
		item := &out[index].Item
		item.Metadata = maps.Clone(item.Metadata)
		item.Evidence = slices.Clone(item.Evidence)
		item.Tags = slices.Clone(item.Tags)
		if item.ValidFrom != nil {
			item.ValidFrom = new(*item.ValidFrom)
		}
		if item.ValidUntil != nil {
			item.ValidUntil = new(*item.ValidUntil)
		}
		if item.ExpiresAt != nil {
			item.ExpiresAt = new(*item.ExpiresAt)
		}
	}
	return out
}

// isRetrievable is the in-memory equivalent of SQLite's scope, supersede,
// temporal, and lifecycle predicates, using the same ancestor-visibility
// semantics as scopeAuthorize. Supplemental sources re-check every row with
// it before fusion.
func isRetrievable(item ContextItem, req SearchRequest, now time.Time) bool {
	scope := req.Scope
	if scope.ProjectID == "" || item.Scope.ProjectID != scope.ProjectID || item.SupersededBy != "" {
		return false
	}
	if !req.IncludeCandidates && item.Lifecycle != "" && item.Lifecycle != LifecycleConfirmed {
		return false
	}
	visibility := req.Visibility
	if visibility == "" {
		visibility = VisibilityAncestors
	}
	for _, level := range [][2]string{
		{scope.TeamID, item.Scope.TeamID},
		{scope.SessionID, item.Scope.SessionID},
		{scope.BranchID, item.Scope.BranchID},
		{scope.AgentID, item.Scope.AgentID},
		{scope.TaskID, item.Scope.TaskID},
		{scope.AttemptID, item.Scope.AttemptID},
	} {
		switch visibility {
		case VisibilitySubtree:
			if level[0] != "" && level[1] != "" && level[0] != level[1] {
				return false
			}
		case VisibilityExact:
			if level[0] != level[1] {
				return false
			}
		default: // VisibilityAncestors
			if level[0] != "" {
				if level[1] != "" && level[0] != level[1] {
					return false
				}
			} else if level[1] != "" {
				return false
			}
		}
	}
	return (item.ValidFrom == nil || !item.ValidFrom.After(now)) &&
		(item.ValidUntil == nil || item.ValidUntil.After(now)) &&
		(item.ExpiresAt == nil || item.ExpiresAt.After(now))
}
