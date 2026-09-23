package main

import (
	"context"
	"fmt"
	"sort"

	contextstore "github.com/kjelly/hufu/internal/context"
)

type consolidationScopeKey struct{ project, team string }

// openConflictsByScope looks up open memory conflicts for each item in its
// own project and team scope, so an omitted --team still sees team conflicts.
func openConflictsByScope(ctx context.Context, repo contextstore.ConflictLookup, items []contextstore.ContextItem) (map[string][]string, error) {
	byScope := map[consolidationScopeKey][]string{}
	for _, item := range items {
		key := consolidationScopeKey{item.Scope.ProjectID, item.Scope.TeamID}
		byScope[key] = append(byScope[key], item.ID)
	}
	result := map[string][]string{}
	for key, ids := range byScope {
		found, err := repo.OpenConflictsForItems(ctx, key.project, key.team, ids)
		if err != nil {
			return nil, fmt.Errorf("check memory conflicts for consolidation: %w", err)
		}
		for id, conflicts := range found {
			result[id] = conflicts
		}
	}
	return result, nil
}

// validateConsolidationConflicts refuses to merge a source that has an open
// memory conflict; resolve it with supersede or dismiss first.
func validateConsolidationConflicts(ctx context.Context, repo contextstore.ConflictLookup, sources []contextstore.ContextItem) error {
	conflicts, err := openConflictsByScope(ctx, repo, sources)
	if err != nil {
		return err
	}
	for _, source := range sources {
		if found := conflicts[source.ID]; len(found) > 0 {
			return fmt.Errorf("source %q has an unresolved memory conflict (%s)", source.ID, found[0])
		}
	}
	return nil
}

// conflictedClusterIDs returns the sorted IDs of cluster members that have an
// open memory conflict.
func conflictedClusterIDs(ctx context.Context, repo contextstore.ConflictLookup, builder *consolidationClusterBuilder, clusters [][]string) ([]string, error) {
	var members []contextstore.ContextItem
	for _, cluster := range clusters {
		for _, id := range cluster {
			members = append(members, contextstore.ContextItem{ID: id, Scope: builder.scopes[id]})
		}
	}
	conflicts, err := openConflictsByScope(ctx, repo, members)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(conflicts))
	for id, found := range conflicts {
		if len(found) > 0 {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return ids, nil
}
