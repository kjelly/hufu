package workspace

import (
	"context"
	"errors"
	"fmt"
	"sort"
)

func (m *WorkspaceManager) GC(ctx context.Context, request GCRequest) (GCResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if request.TrashOlderThan <= 0 {
		return GCResult{}, fmt.Errorf("trash retention must be positive")
	}
	registry, err := OpenReadOnly(m.stateRoot, m.registryOptions...)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return GCResult{Outcome: "complete", Apply: request.Apply, Candidates: []TrashWorkspace{}, Purged: []LifecycleItem{}}, nil
		}
		return GCResult{}, err
	}
	trash, listErr := registry.ListTrashWorkspaces(ctx)
	now := registry.now().UTC()
	closeErr := registry.Close()
	if listErr != nil || closeErr != nil {
		return GCResult{}, errors.Join(listErr, closeErr)
	}
	candidates := make([]TrashWorkspace, 0)
	for _, item := range trash {
		if item.State == "trashed" && !item.DeletedAt.Add(request.TrashOlderThan).After(now) {
			candidates = append(candidates, item)
		}
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].TrashID < candidates[j].TrashID })
	result := GCResult{Outcome: "complete", Apply: request.Apply, Candidates: candidates, Purged: []LifecycleItem{}}
	if !request.Apply {
		return result, nil
	}
	for _, candidate := range candidates {
		purged, purgeErr := m.Purge(ctx, PurgeRequest{TrashID: candidate.TrashID})
		result.Purged = append(result.Purged, purged.Items...)
		if purgeErr != nil {
			if len(result.Purged) > 0 {
				result.Outcome = "partial"
			} else {
				result.Outcome = "failed"
			}
			return result, purgeErr
		}
	}
	return result, nil
}
