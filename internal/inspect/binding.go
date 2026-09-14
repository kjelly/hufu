package inspect

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	operatorpkg "github.com/kjelly/hufu/internal/operator"
	"github.com/kjelly/hufu/internal/team"
)

var ErrScopeConflict = errors.New("inspect scope conflicts with persisted binding")

type BoundReadTarget struct {
	Scope   operatorpkg.ResolvedScope
	Lineage Lineage
	Session *team.SessionData
}

func BindReadTarget(ctx context.Context, request operatorpkg.BindingRequest) (BoundReadTarget, error) {
	query := InspectQuery{
		Workspace: request.Workspace.WorkspaceExact,
		BranchID:  strings.TrimSpace(request.BranchID),
		SessionID: strings.TrimSpace(request.SessionID),
	}
	lineage, err := LoadLineage(ctx, query)
	if err != nil {
		return BoundReadTarget{}, err
	}
	session, exists, err := team.LoadSessionReadOnly(query.Workspace)
	if err != nil {
		return BoundReadTarget{}, fmt.Errorf("%w: active session projection is unreadable", ErrIntegrity)
	}
	if !exists {
		session = nil
	}

	runID, selection, err := selectBindingRun(lineage, request, session)
	if err != nil {
		return BoundReadTarget{}, err
	}
	sessionID, err := selectBindingSession(lineage, runID, request.SessionID)
	if err != nil {
		return BoundReadTarget{}, err
	}
	projectID, teamID, err := persistedScopeIDs(lineage, runID)
	if err != nil {
		return BoundReadTarget{}, err
	}
	if err := requireCompatibleID("project", request.ProjectID, projectID); err != nil {
		return BoundReadTarget{}, err
	}
	if err := requireCompatibleID("team", request.TeamID, teamID); err != nil {
		return BoundReadTarget{}, err
	}
	persistedTeam := teamID
	if err := requireCompatibleID("team", persistedTeam, lineage.SelectedTeam); err != nil {
		return BoundReadTarget{}, err
	}
	persistedTeam = firstNonEmpty(persistedTeam, lineage.SelectedTeam)
	requestedTeam := firstNonEmpty(strings.TrimSpace(request.TeamID), request.Workspace.TeamName)
	if err := requireCompatibleID("team", requestedTeam, persistedTeam); err != nil {
		return BoundReadTarget{}, err
	}

	return BoundReadTarget{
		Scope: operatorpkg.ResolvedScope{
			RequestedPath:      request.Workspace.RequestedPath,
			RequestedSemantics: request.Workspace.RequestedSemantics,
			WorkspaceExact:     request.Workspace.WorkspaceExact,
			WorkspaceRoot:      request.Workspace.WorkspaceRoot,
			ProjectDir:         request.Workspace.ProjectDir,
			ProjectID:          projectID,
			TeamName:           persistedTeam,
			SessionID:          sessionID,
			RunID:              runID,
			BranchID:           lineage.BranchID,
			SelectionSource:    selection,
			BindingStatus:      "verified",
		},
		Lineage: lineage,
		Session: session,
	}, nil
}

func selectBindingRun(lineage Lineage, request operatorpkg.BindingRequest, session *team.SessionData) (string, string, error) {
	runs := uniqueRunIDs(lineage.Events)
	requested := strings.TrimSpace(request.RunID)
	if requested != "" {
		if !slices.Contains(runs, requested) {
			return "", "", fmt.Errorf("%w: run %q", ErrNotFound, requested)
		}
		return requested, "explicit", nil
	}
	if lineage.BranchID == lineage.ActiveBranchID && session != nil {
		active, err := activeSessionRunID(session)
		if err != nil {
			return "", "", err
		}
		if active != "" && slices.Contains(runs, active) {
			return active, "active_binding", nil
		}
	}
	switch len(runs) {
	case 0:
		return "", "", fmt.Errorf("%w: no run in branch %q", ErrNotFound, lineage.BranchID)
	case 1:
		return runs[0], "single_candidate", nil
	default:
		return "", "", fmt.Errorf("%w: branch %q contains runs %v", ErrAmbiguous, lineage.BranchID, runs)
	}
}

func activeSessionRunID(session *team.SessionData) (string, error) {
	activeSnapshotID := strings.TrimSpace(session.ActiveRunInputSnapshotID)
	if activeSnapshotID != "" {
		var runIDs []string
		for _, snapshot := range session.RunInputSnapshots {
			if snapshot.ID == activeSnapshotID {
				runIDs = appendNonEmpty(runIDs, snapshot.RunID)
			}
		}
		slices.Sort(runIDs)
		runIDs = slices.Compact(runIDs)
		switch len(runIDs) {
		case 0:
			return "", fmt.Errorf("%w: active run-input snapshot %q is unresolved", ErrIntegrity, activeSnapshotID)
		case 1:
			return runIDs[0], nil
		default:
			return "", fmt.Errorf("%w: active run-input snapshot %q names runs %v", ErrIntegrity, activeSnapshotID, runIDs)
		}
	}
	if session.RunResult != nil {
		return strings.TrimSpace(session.RunResult.RunID), nil
	}
	return "", nil
}

// ActiveSessionRunID returns the run currently materialized by session.json.
// Mutation facades use it to reject an older run that happens to share the
// active branch before loading a coordinator or provider.
func ActiveSessionRunID(session *team.SessionData) (string, error) {
	return activeSessionRunID(session)
}

func selectBindingSession(lineage Lineage, runID, requested string) (string, error) {
	var sessions []string
	for _, indexed := range lineage.Events {
		if indexed.Event.RunID != runID || strings.TrimSpace(indexed.Event.SessionID) == "" {
			continue
		}
		sessions = append(sessions, indexed.Event.SessionID)
	}
	slices.Sort(sessions)
	sessions = slices.Compact(sessions)
	requested = strings.TrimSpace(requested)
	if requested != "" {
		if !slices.Contains(sessions, requested) {
			return "", fmt.Errorf("%w: session %q does not contain run %q", ErrScopeConflict, requested, runID)
		}
		return requested, nil
	}
	switch len(sessions) {
	case 0:
		return "", nil
	case 1:
		return sessions[0], nil
	default:
		return "", fmt.Errorf("%w: run %q appears in sessions %v", ErrAmbiguous, runID, sessions)
	}
}

func uniqueRunIDs(events []IndexedEvent) []string {
	var runs []string
	for _, indexed := range events {
		if runID := strings.TrimSpace(indexed.Event.RunID); runID != "" {
			runs = append(runs, runID)
		}
	}
	slices.Sort(runs)
	return slices.Compact(runs)
}

func persistedScopeIDs(lineage Lineage, runID string) (string, string, error) {
	var projects, teams []string
	for _, indexed := range lineage.Events {
		if indexed.Event.RunID != runID || len(indexed.Event.Payload) == 0 {
			continue
		}
		var payload struct {
			ProjectID string `json:"project_id"`
			TeamID    string `json:"team_id"`
			Scope     struct {
				ProjectID string `json:"project_id"`
				TeamID    string `json:"team_id"`
			} `json:"scope"`
		}
		if err := json.Unmarshal(indexed.Event.Payload, &payload); err != nil {
			return "", "", fmt.Errorf("%w: decode persisted scope for event %q: %v", ErrIntegrity, indexed.Event.ID, err)
		}
		projects = appendNonEmpty(projects, payload.ProjectID, payload.Scope.ProjectID)
		teams = appendNonEmpty(teams, payload.TeamID, payload.Scope.TeamID)
	}
	projectID, err := uniqueScopeID("project", projects)
	if err != nil {
		return "", "", err
	}
	teamID, err := uniqueScopeID("team", teams)
	if err != nil {
		return "", "", err
	}
	return projectID, teamID, nil
}

func uniqueScopeID(kind string, values []string) (string, error) {
	slices.Sort(values)
	values = slices.Compact(values)
	switch len(values) {
	case 0:
		return "", nil
	case 1:
		return values[0], nil
	default:
		return "", fmt.Errorf("%w: run has conflicting %s IDs %v", ErrScopeConflict, kind, values)
	}
}

func appendNonEmpty(values []string, candidates ...string) []string {
	for _, candidate := range candidates {
		if candidate = strings.TrimSpace(candidate); candidate != "" {
			values = append(values, candidate)
		}
	}
	return values
}

func requireCompatibleID(kind, requested, persisted string) error {
	requested = strings.TrimSpace(requested)
	persisted = strings.TrimSpace(persisted)
	if requested != "" && persisted != "" && requested != persisted {
		return fmt.Errorf("%w: requested %s %q does not match persisted %q", ErrScopeConflict, kind, requested, persisted)
	}
	return nil
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}
