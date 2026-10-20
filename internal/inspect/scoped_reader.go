package inspect

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/kjelly/hufu/internal/cost"
	operatorpkg "github.com/kjelly/hufu/internal/operator"
	"github.com/kjelly/hufu/internal/team"
)

type ReadLimits struct {
	MaxEvents        int
	MaxPayloadBytes  int
	MaxArtifactBytes int64
}

// ScopedReader retains only an immutable binding request, not a cached
// projection. Each call validates fresh persisted identity and one event head.
type ScopedReader struct {
	request operatorpkg.BindingRequest
	scope   operatorpkg.ResolvedScope
	limits  ReadLimits
	anchor  cost.Freshness
}

type ScopedObservation struct {
	SourceHead cost.Freshness `json:"source_head"`
	Result     *Envelope      `json:"result"`
}

func NewScopedReader(ctx context.Context, query InspectQuery, limits ReadLimits) (*ScopedReader, error) {
	if !filepath.IsAbs(query.Workspace) || limits.MaxEvents <= 0 ||
		limits.MaxPayloadBytes <= 0 || limits.MaxArtifactBytes <= 0 {
		return nil, fmt.Errorf("%w: explicit absolute workspace and positive limits required", ErrInvalidQuery)
	}
	if err := query.Validate(KindEconomics); err != nil {
		return nil, err
	}
	workspace, err := operatorpkg.ResolveWorkspacePath(operatorpkg.WorkspaceRequest{RequestedPath: query.Workspace, Mode: "exact"})
	if err != nil {
		return nil, fmt.Errorf("%w: workspace could not be resolved", ErrInvalidQuery)
	}
	request := operatorpkg.BindingRequest{
		Workspace: workspace, RunID: query.RunID, SessionID: query.SessionID, BranchID: query.BranchID,
	}
	bound, err := BindReadTargetWithLimits(ctx, request, limits.MaxEvents, limits.MaxPayloadBytes)
	if err != nil {
		return nil, err
	}
	events, err := observationEvents(bound.Lineage, InspectQuery{RunID: bound.Scope.RunID, SessionID: bound.Scope.SessionID})
	if err != nil {
		return nil, err
	}
	head := events[len(events)-1].Event
	return &ScopedReader{request: request, scope: bound.Scope, limits: limits,
		anchor: cost.Freshness{EventID: head.ID, EventHash: head.Hash}}, nil
}

func (r *ScopedReader) Scope() operatorpkg.ResolvedScope {
	return r.scope
}

// Inspect exposes only the metadata projections used by read-only consumers.
// There is no per-call scope override or content/artifact retrieval operation.
func (r *ScopedReader) Inspect(ctx context.Context, kind Kind, taskID string) (*ScopedObservation, error) {
	if kind != KindRun && kind != KindRoutes && kind != KindEconomics {
		return nil, fmt.Errorf("%w: unsupported scoped projection", ErrInvalidQuery)
	}
	if taskID != "" && (kind != KindRoutes || len(taskID) > 160 ||
		safeOpaqueRef(taskID) != taskID || strings.TrimSpace(taskID) != taskID) {
		return nil, fmt.Errorf("%w: invalid task identifier", ErrInvalidQuery)
	}
	bound, err := BindReadTargetWithLimits(ctx, r.request, r.limits.MaxEvents, r.limits.MaxPayloadBytes)
	if err != nil {
		return nil, err
	}
	if bound.Scope.ProjectID != r.scope.ProjectID || bound.Scope.TeamName != r.scope.TeamName ||
		bound.Scope.InvocationID != r.scope.InvocationID {
		return nil, fmt.Errorf("%w: persisted identity changed", ErrScopeConflict)
	}
	query := InspectQuery{Workspace: r.scope.WorkspaceExact, RunID: r.scope.RunID,
		SessionID: r.scope.SessionID, BranchID: r.scope.BranchID, TaskID: taskID}
	events, err := observationEvents(bound.Lineage, query)
	if err != nil {
		return nil, err
	}
	anchored := false
	for _, indexed := range events {
		if indexed.Event.ID == r.anchor.EventID {
			if indexed.Event.Hash != r.anchor.EventHash {
				return nil, fmt.Errorf("%w: bound event identity changed", ErrIntegrity)
			}
			anchored = true
		}
	}
	if !anchored {
		return nil, fmt.Errorf("%w: bound event no longer exists in selected lineage", ErrScopeConflict)
	}
	bound.Lineage.Events = events
	ctx, budget := team.WithArtifactReadBudget(ctx, r.limits.MaxArtifactBytes)
	var data any
	switch kind {
	case KindRun:
		data, _, err = projectRunData(ctx, query, bound.Lineage)
	case KindRoutes:
		data, err = projectRoutes(ctx, query, bound.Lineage)
	case KindEconomics:
		data, err = projectEconomics(ctx, query, bound.Lineage)
	}
	// Audit findings are not an escape from a transport's read budget or
	// cancellation. Never return a partial successful response in these cases.
	if limitErr := budget.Err(); limitErr != nil {
		return nil, limitErr
	}
	if contextErr := ctx.Err(); contextErr != nil {
		return nil, contextErr
	}
	if err != nil {
		return nil, err
	}
	head := events[len(events)-1].Event
	return &ScopedObservation{
		SourceHead: cost.Freshness{EventID: head.ID, EventHash: head.Hash},
		Result:     envelope(kind, query, bound.Scope.BranchID, data),
	}, nil
}
