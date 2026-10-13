package team

import (
	"context"
	"database/sql"
	"slices"
	"strconv"

	"github.com/kjelly/hufu/internal/agent"
	contextstore "github.com/kjelly/hufu/internal/context"
)

type contextQueryPrivateSnapshot struct {
	sources             []ContextQuerySource
	request             WorkerMemoryRecallRequest
	legacyRows          []rankedMemory
	maxItems, maxTokens int
}

func (s *defaultWorkerMemoryService) beginPrivateQuerySnapshot(ctx context.Context, req WorkerMemoryRecallRequest, maxItems, maxTokens int) *contextQueryPrivateSnapshot {
	if !evaluatingContextQuery(ctx) {
		return nil
	}
	revision, err := s.repo.Revision(ctx)
	restricted := slices.ContainsFunc(req.SessionLineage, func(branch WorkerMemoryLineageBranch) bool { return branch.RestrictItems })
	persistent := req.Scope
	persistent.SessionID, persistent.BranchID, persistent.TaskID, persistent.AttemptID = "", "", "", ""
	snapshot := &contextQueryPrivateSnapshot{request: req, maxItems: maxItems, maxTokens: maxTokens, sources: []ContextQuerySource{
		{Kind: QueryPrivateSession, Request: contextstore.SearchRequest{Scope: req.Scope, Limit: 100000}, Revision: strconv.FormatInt(revision, 10), RestrictedLineage: restricted, Unavailable: err != nil},
		{Kind: QueryPrivatePersistent, Request: contextstore.SearchRequest{Scope: persistent, Limit: maxItems * 3}, Revision: strconv.FormatInt(revision, 10), RestrictedLineage: restricted, Unavailable: err != nil},
	}}
	if req.Policy.Mode != agent.WorkerMemoryPersistent {
		snapshot.sources[1].Disabled = QueryFallbackMemoryDisabled
	}
	if req.Scope.SessionID == "" {
		snapshot.sources[0].Disabled = QueryFallbackMemoryDisabled
	}
	return snapshot
}

func (snapshot *contextQueryPrivateSnapshot) capturePrimary(rows []rankedMemory) {
	cloned := make([]contextstore.SearchResult, len(rows))
	for index, row := range rows {
		cloned[index] = contextstore.SearchResult{Item: row.item, Score: row.score}
	}
	cloned = contextstore.CloneSearchResults(cloned)
	snapshot.legacyRows = make([]rankedMemory, len(rows))
	for index, row := range cloned {
		snapshot.legacyRows[index] = rankedMemory{item: row.Item, score: row.Score, tier: rows[index].tier}
		kind := QueryPrivateSession
		if rows[index].tier == "persistent" {
			kind = QueryPrivatePersistent
		}
		if rows[index].tier == "shared" {
			continue
		}
		for source := range snapshot.sources {
			if snapshot.sources[source].Kind == kind {
				snapshot.sources[source].Primary = append(snapshot.sources[source].Primary, row)
			}
		}
	}
}

// preparePrivateQueryEligibility reads canonical authorization under the SAME
// child deadline as supplemental search. Iterate never re-enters SQLite; typed
// activation projection is read only after the cursor has been closed.
func (c *Coordinator) preparePrivateQueryEligibility(ctx context.Context, request ContextRequest, sources []ContextQuerySource) error {
	privateRestricted := slices.ContainsFunc(sources, func(source ContextQuerySource) bool {
		return source.Kind != QuerySharedPersistent && source.RestrictedLineage
	})
	if privateRestricted {
		return nil
	}
	var enabled []int
	var readScope contextstore.Scope
	for index := range sources {
		source := &sources[index]
		if source.Kind == QuerySharedPersistent || source.Unavailable || (source.Disabled != "" && source.Disabled != QueryFallbackNone) {
			continue
		}
		enabled = append(enabled, index)
		source.Eligible = make(map[string]contextstore.ContextItem)
		if readScope.ProjectID == "" || source.Kind == QueryPrivateSession {
			readScope = source.Request.Scope
		}
	}
	if len(enabled) == 0 {
		return nil
	}
	accept := func(item contextstore.ContextItem, record contextstore.ActivationRecord) error {
		return c.acceptPrivateQueryEligibility(ctx, request, sources, enabled, itemWithContextActivation(item, record))
	}
	if iterator, ok := c.contextRepo.(interface {
		IterateAgentActivations(context.Context, contextstore.RepositoryQuery, func(contextstore.ContextItem, contextstore.ActivationRecord) error) error
	}); ok {
		return iterator.IterateAgentActivations(ctx, contextstore.RepositoryQuery{Scope: readScope}, accept)
	}
	// The existing ancestor visibility of the active worker/session scope
	// already includes that worker's persistent rows. Read this universe once,
	// then enforce each source's original scope independently below.
	var items []contextstore.ContextItem
	err := c.contextRepo.Iterate(ctx, contextstore.RepositoryQuery{Scope: readScope}, func(item contextstore.ContextItem) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if item.Scope.AgentID == readScope.AgentID && item.Scope.AgentID != "" {
			items = append(items, item)
		}
		return nil
	})
	if err != nil {
		return err
	}
	var activations map[string]contextstore.ActivationRecord
	if batch, ok := c.contextRepo.(interface {
		ActivationsForItems(context.Context, []string) (map[string]contextstore.ActivationRecord, error)
	}); ok {
		ids := make([]string, len(items))
		for index, item := range items {
			ids[index] = item.ID
		}
		activations, err = batch.ActivationsForItems(ctx, ids)
		if err != nil {
			return err
		}
	}
	for _, item := range items {
		if err := ctx.Err(); err != nil {
			return err
		}
		var projected contextstore.ContextItem
		if activations != nil {
			record, ok := activations[item.ID]
			if !ok {
				return sql.ErrNoRows
			}
			projected = itemWithContextActivation(item, record)
		} else {
			projected, err = activationItemFromRepository(ctx, c.contextRepo, item)
		}
		if err != nil {
			return err
		}
		if err := c.acceptPrivateQueryEligibility(ctx, request, sources, enabled, projected); err != nil {
			return err
		}
	}
	return nil
}

func (c *Coordinator) acceptPrivateQueryEligibility(ctx context.Context, request ContextRequest, sources []ContextQuerySource, enabled []int, item contextstore.ContextItem) error {
	eligible, _, err := EvaluateContextEligibility(item, request, c.executionRunID, contextstore.EvaluationTime(ctx))
	if err != nil || !eligible {
		return err
	}
	for _, index := range enabled {
		source := &sources[index]
		if workerMemoryScopeAllows(source.Request.Scope, item.Scope) && (source.Kind == QueryPrivateSession) == (item.Scope.SessionID != "") {
			source.Eligible[item.ID] = item
		}
	}
	return nil
}

func (snapshot *contextQueryPrivateSnapshot) mergeSelection(selection ContextQuerySelection, legacy *WorkerMemoryBundle) WorkerMemoryBundle {
	var rows []rankedMemory
	for _, row := range snapshot.legacyRows {
		if row.tier == "shared" {
			rows = append(rows, row)
		}
	}
	for _, source := range selection.Sources {
		tier := "session"
		switch source.Source {
		case QueryPrivateSession:
		case QueryPrivatePersistent:
			tier = "persistent"
		default:
			continue
		}
		for _, row := range source.Results {
			rows = append(rows, rankedMemory{item: row.Item, score: row.Score, tier: tier})
		}
	}
	bundle := selectWorkerMemoryRows(rows, snapshot.maxItems, snapshot.maxTokens, snapshot.request.WorkerID, snapshot.request.Query, legacy.Semantic)
	bundle.querySnapshot = snapshot
	return bundle
}

// queryLegacyRollback preserves each captured primary after metadata or source
// I/O failure. Disabled and restricted sources retain their original reasons.
func queryLegacyRollback(plan ContextQueryPlan, sources []ContextQuerySource, reason ContextQueryFallback) (ContextQuerySelection, error) {
	selection, err := initializeQuerySelection(plan, sources)
	if err != nil {
		return selection, err
	}
	for index := range selection.Attribution.Sources {
		attribution := &selection.Attribution.Sources[index]
		if attribution.Mode == QueryModeLegacy && attribution.Fallback == QueryFallbackNone {
			attribution.Fallback = reason
		}
	}
	return finishQuerySelection(selection), nil
}
