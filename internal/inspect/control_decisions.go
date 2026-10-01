package inspect

import (
	"context"
	"fmt"

	operatorpkg "github.com/kjelly/hufu/internal/operator"
	"github.com/kjelly/hufu/internal/team"
)

// ControlDecisionsData summarizes runtime control decision observations
// (docs/architecture/decision-primitive.md §59) per point and mode. Scope is
// "branch" for the selected branch lineage or "all_branches".
type ControlDecisionsData struct {
	Scope     string                        `json:"scope"`
	Summaries []team.ControlDecisionSummary `json:"summaries"`
}

// InspectControlDecisions aggregates control decision observations across
// every run in the selected branch lineage, across every branch when
// AllBranches is set (each --new run starts a new branch), or for one run
// when RunID is set. The whole event chain is validated first; nothing is
// executed.
func InspectControlDecisions(ctx context.Context, query InspectQuery) (*Envelope, error) {
	if err := query.Validate(KindControlDecisions); err != nil {
		return nil, err
	}
	workspace, err := operatorpkg.ResolveWorkspacePath(operatorpkg.WorkspaceRequest{RequestedPath: query.Workspace, Mode: "exact"})
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidQuery, err)
	}
	resolved := query
	resolved.Workspace = workspace.WorkspaceExact
	lineage, err := LoadLineage(ctx, resolved)
	if err != nil {
		return nil, err
	}
	source, scope := lineage.Events, "branch"
	if resolved.AllBranches {
		source, scope = lineage.GlobalEvents, "all_branches"
	}
	events := make([]team.RunEvent, 0, len(source))
	runFound := resolved.RunID == ""
	for _, indexed := range source {
		if resolved.SessionID != "" && indexed.Event.SessionID != resolved.SessionID {
			continue
		}
		if resolved.RunID != "" && indexed.Event.RunID != resolved.RunID {
			continue
		}
		runFound = true
		events = append(events, indexed.Event)
	}
	if !runFound {
		return nil, fmt.Errorf("%w: run %q", ErrNotFound, resolved.RunID)
	}
	summaries := team.SummarizeControlDecisions(events)
	if summaries == nil {
		summaries = []team.ControlDecisionSummary{}
	}
	branchID := lineage.BranchID
	if resolved.AllBranches {
		branchID = ""
	}
	return envelope(KindControlDecisions, resolved, branchID, ControlDecisionsData{Scope: scope, Summaries: summaries}), nil
}
