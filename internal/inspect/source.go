package inspect

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/kjelly/hufu/internal/team"
)

type IndexedEvent struct {
	Ordinal int64
	Event   team.RunEvent
}

type Lineage struct {
	BranchID         string
	ActiveBranchID   string
	GlobalEventCount int
	GlobalEvents     []IndexedEvent
	Events           []IndexedEvent
}

// LoadLineage validates the complete global event chain before selecting one
// branch. An empty BranchID selects only the active branch; sibling branches
// are never searched implicitly.
func LoadLineage(ctx context.Context, query InspectQuery) (Lineage, error) {
	workspace := strings.TrimSpace(query.Workspace)
	if workspace == "" {
		return Lineage{}, fmt.Errorf("%w: workspace is required after CLI resolution", ErrInvalidQuery)
	}
	var global []team.RunEvent
	if err := team.StreamValidatedRunEvents(ctx, workspace, func(event team.RunEvent) error {
		global = append(global, event)
		return nil
	}); err != nil {
		return Lineage{}, fmt.Errorf("%w: %v", ErrIntegrity, err)
	}

	tree, err := team.LoadSessionTree(workspace)
	if err != nil {
		return Lineage{}, fmt.Errorf("%w: load session tree: %v", ErrIntegrity, err)
	}
	branchID, err := resolveBranch(tree, query.BranchID)
	if err != nil {
		return Lineage{}, err
	}
	selected, err := team.ProjectValidatedEventsForBranch(global, tree, branchID)
	if err != nil {
		return Lineage{}, fmt.Errorf("%w: project branch %q: %v", ErrIntegrity, branchID, err)
	}

	ordinals := make(map[string]int64, len(global))
	for i, event := range global {
		if _, duplicate := ordinals[event.ID]; duplicate {
			return Lineage{}, fmt.Errorf("%w: duplicate event id %q", ErrIntegrity, event.ID)
		}
		ordinals[event.ID] = int64(i + 1)
	}
	indexed := make([]IndexedEvent, 0, len(selected))
	for _, event := range selected {
		ordinal, ok := ordinals[event.ID]
		if !ok {
			return Lineage{}, fmt.Errorf("%w: branch event %q is absent from global chain", ErrIntegrity, event.ID)
		}
		indexed = append(indexed, IndexedEvent{Ordinal: ordinal, Event: event})
	}
	globalIndexed := make([]IndexedEvent, len(global))
	for index, event := range global {
		globalIndexed[index] = IndexedEvent{Ordinal: int64(index + 1), Event: event}
	}
	return Lineage{BranchID: branchID, ActiveBranchID: tree.ActiveBranch, GlobalEventCount: len(global), GlobalEvents: globalIndexed, Events: indexed}, nil
}

func resolveBranch(tree *team.SessionTree, requested string) (string, error) {
	if tree == nil {
		return "", fmt.Errorf("%w: session tree is unavailable", ErrIntegrity)
	}
	requested = strings.TrimSpace(requested)
	if requested == "" {
		active := strings.TrimSpace(tree.ActiveBranch)
		if active == "" || tree.Branches[active] == nil {
			return "", fmt.Errorf("%w: active branch is unavailable", ErrNotFound)
		}
		return active, nil
	}
	if tree.Branches[requested] != nil {
		return requested, nil
	}

	target := requested
	if labelTarget, ok := tree.Labels[requested]; ok {
		target = labelTarget
		if tree.Branches[target] != nil {
			return target, nil
		}
	}
	var matches []string
	for id, branch := range tree.Branches {
		if branch != nil && branch.Name == target {
			matches = append(matches, id)
		}
	}
	slices.Sort(matches)
	switch len(matches) {
	case 0:
		return "", fmt.Errorf("%w: branch %q", ErrNotFound, requested)
	case 1:
		return matches[0], nil
	default:
		return "", fmt.Errorf("%w: branch name %q matches %v", ErrAmbiguous, requested, matches)
	}
}
