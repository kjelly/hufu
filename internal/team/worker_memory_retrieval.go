package team

import contextstore "github.com/kjelly/hufu/internal/context"

func workerMemorySessionSearch(req WorkerMemoryRecallRequest, branch WorkerMemoryLineageBranch, query string) contextstore.SearchRequest {
	scope := req.Scope
	scope.BranchID = branch.BranchID
	search := contextstore.SearchRequest{
		Query: query, SemanticQuery: req.SemanticQuery, Scope: scope, Limit: 100000,
	}
	if !branch.RestrictItems {
		return search
	}
	// Retrieve every authorized ancestor, rather than overfetching a fixed
	// number of newer branch records and discarding them after retrieval.
	ids := make([]string, 0, len(branch.AllowedItemIDs))
	for id, allowed := range branch.AllowedItemIDs {
		if allowed {
			ids = append(ids, id)
		}
	}
	search = search.WithAllowedItemIDs(ids)
	search.Limit = max(1, len(search.AllowedItemIDs))
	return search
}
