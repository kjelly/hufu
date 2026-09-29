package inspect

import (
	"context"
	"fmt"

	"github.com/kjelly/hufu/internal/cost"
	operatorpkg "github.com/kjelly/hufu/internal/operator"
	"github.com/kjelly/hufu/internal/team"
)

type CostData struct {
	cost.View
}

func InspectCost(ctx context.Context, query InspectQuery) (*Envelope, error) {
	if err := query.Validate(KindCost); err != nil {
		return nil, err
	}
	workspace, err := operatorpkg.ResolveWorkspacePath(operatorpkg.WorkspaceRequest{
		RequestedPath: query.Workspace,
		Mode:          "exact",
	})
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidQuery, err)
	}
	bound, err := BindReadTarget(ctx, operatorpkg.BindingRequest{
		Workspace: workspace,
		SessionID: query.SessionID,
		RunID:     query.RunID,
		BranchID:  query.BranchID,
	})
	if err != nil {
		return nil, err
	}
	resolved := query
	resolved.Workspace = workspace.WorkspaceExact
	resolved.RunID = bound.Scope.RunID
	resolved.SessionID = bound.Scope.SessionID
	resolved.BranchID = bound.Scope.BranchID
	selected, err := selectRun(bound.Lineage, resolved)
	if err != nil {
		return nil, err
	}
	if resolved.TaskID != "" {
		tasks, replayErr := team.ReplayTodoList(selected.runEvents)
		if replayErr != nil {
			return nil, fmt.Errorf("%w: replay cost task scope for run %q: %v", ErrIntegrity, resolved.RunID, replayErr)
		}
		found := false
		for _, item := range tasks {
			if item != nil && item.ID == resolved.TaskID {
				found = true
				break
			}
		}
		if !found {
			return nil, fmt.Errorf("%w: task %q in run %q", ErrNotFound, resolved.TaskID, resolved.RunID)
		}
	}
	view, err := team.ProjectCostView(selected.runEvents, resolved.RunID, resolved.TaskID)
	if err != nil {
		return nil, fmt.Errorf("%w: project cost for run %q: %v", ErrIntegrity, resolved.RunID, err)
	}
	return envelope(KindCost, resolved, bound.Scope.BranchID, CostData{View: view}), nil
}
