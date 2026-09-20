package workspace

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const workspaceSelect = `SELECT id,project_id,team_name,context_scope_id,control_root,state,COALESCE(operation_id,''),COALESCE(pending_path,''),requires_fresh_session,created_at,updated_at,last_used_at FROM workspaces`

func (r *SQLiteRegistry) GetWorkspace(ctx context.Context, projectID, teamName string) (Workspace, error) {
	team, err := NormalizeTeamName(teamName)
	if err != nil {
		return Workspace{}, err
	}
	return scanWorkspace(r.db.QueryRowContext(ctx, workspaceSelect+" WHERE project_id=? AND team_name=?", projectID, team))
}

func pathsOverlap(first, second string) bool {
	return pathContains(first, second) || pathContains(second, first)
}

func pathContains(parent, child string) bool {
	relative, err := filepath.Rel(parent, child)
	if err != nil {
		return false
	}
	return relative == "." || relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func (r *SQLiteRegistry) ListWorkspaces(ctx context.Context, projectID string) ([]Workspace, error) {
	rows, err := r.db.QueryContext(ctx, workspaceSelect+" WHERE project_id=? ORDER BY team_name,id", projectID)
	if err != nil {
		return nil, fmt.Errorf("list workspaces: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var workspaces []Workspace
	for rows.Next() {
		workspace, scanErr := scanWorkspace(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		workspaces = append(workspaces, workspace)
	}
	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("list workspaces: %w", err)
	}
	return workspaces, nil
}

func (r *SQLiteRegistry) CreateWorkspace(ctx context.Context, projectSelector, teamName string) (Workspace, error) {
	if r.readOnly {
		return Workspace{}, errors.New("create workspace: registry is read-only")
	}
	project, err := r.ResolveProject(ctx, projectSelector)
	if err != nil {
		return Workspace{}, err
	}
	team, err := NormalizeTeamName(teamName)
	if err != nil {
		return Workspace{}, err
	}
	if pathsOverlap(project.SubjectRoot, r.stateRoot) {
		return Workspace{}, fmt.Errorf("%w: subject root %q overlaps state root %q", ErrConflict, project.SubjectRoot, r.stateRoot)
	}
	existing, err := r.GetWorkspace(ctx, project.ID, team)
	if err == nil {
		if existing.State == "active" {
			return existing, nil
		}
		return Workspace{}, fmt.Errorf("%w: workspace %s/%s is %s", ErrConflict, project.ID, team, existing.State)
	}
	if !errors.Is(err, ErrNotFound) {
		return Workspace{}, err
	}

	workspaceID, err := r.idGenerator.New(WorkspaceIDKind)
	if err != nil {
		return Workspace{}, err
	}
	operationID, err := r.idGenerator.New(OperationIDKind)
	if err != nil {
		return Workspace{}, err
	}
	controlRoot := filepath.Join(project.StateDir, "teams", team)
	pendingPath := filepath.Join(r.stateRoot, "staging", operationID)
	now := r.now().UTC()
	workspace := Workspace{
		ID: workspaceID, ProjectID: project.ID, TeamName: team,
		ContextScopeID: project.SubjectRoot, ControlRoot: controlRoot,
		State: "creating", OperationID: operationID, PendingPath: pendingPath,
		CreatedAt: now, UpdatedAt: now,
	}
	if err = r.reserveWorkspace(ctx, workspace); err != nil {
		return r.resolveConcurrentWorkspaceReservation(ctx, project.ID, team, err)
	}
	if err = r.runCreateHook(CreateStageReserved); err != nil {
		return Workspace{}, err
	}
	if err = r.populateStaging(workspace); err != nil {
		return Workspace{}, err
	}
	if err = r.publishWorkspace(workspace); err != nil {
		return Workspace{}, err
	}
	if err = r.runCreateHook(CreateStageBeforeActivation); err != nil {
		return Workspace{}, err
	}
	if err = r.activateWorkspace(ctx, workspace); err != nil {
		return Workspace{}, err
	}
	workspace.State = "active"
	workspace.OperationID = ""
	workspace.PendingPath = ""
	workspace.UpdatedAt = r.now().UTC()
	return workspace, nil
}

func (r *SQLiteRegistry) resolveConcurrentWorkspaceReservation(ctx context.Context, projectID, team string, reservationErr error) (Workspace, error) {
	concurrent, lookupErr := r.GetWorkspace(ctx, projectID, team)
	if lookupErr != nil {
		return Workspace{}, reservationErr
	}
	if concurrent.State == "active" {
		return concurrent, nil
	}
	return Workspace{}, fmt.Errorf("%w: workspace %s/%s was concurrently reserved in state %s: %v", ErrConflict, projectID, team, concurrent.State, reservationErr)
}

func (r *SQLiteRegistry) reserveWorkspace(ctx context.Context, workspace Workspace) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("reserve workspace: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	_, err = tx.ExecContext(ctx, `INSERT INTO workspaces(id,project_id,team_name,context_scope_id,control_root,state,operation_id,pending_path,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?)`, workspace.ID, workspace.ProjectID, workspace.TeamName, workspace.ContextScopeID, workspace.ControlRoot, workspace.State, workspace.OperationID, workspace.PendingPath, workspace.CreatedAt.UnixMilli(), workspace.UpdatedAt.UnixMilli())
	if err != nil {
		return fmt.Errorf("reserve workspace row: %w", err)
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO registry_operations(id,kind,project_id,workspace_id,state,started_at) VALUES(?,?,?,?,?,?)`, workspace.OperationID, "create", workspace.ProjectID, workspace.ID, "started", workspace.CreatedAt.UnixMilli())
	if err != nil {
		return fmt.Errorf("reserve workspace operation: %w", err)
	}
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("commit workspace reservation: %w", err)
	}
	return nil
}

func (r *SQLiteRegistry) populateStaging(workspace Workspace) error {
	if _, err := os.Lstat(workspace.PendingPath); err == nil {
		return fmt.Errorf("%w: staging path already exists at %q", ErrConflict, workspace.PendingPath)
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("inspect staging path: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(workspace.PendingPath), 0o700); err != nil {
		return fmt.Errorf("create staging root: %w", err)
	}
	if err := r.syncManagedAncestors(filepath.Dir(workspace.PendingPath)); err != nil {
		return err
	}
	if err := os.Mkdir(workspace.PendingPath, 0o700); err != nil {
		return fmt.Errorf("create workspace staging directory: %w", err)
	}
	if err := syncDirectory(filepath.Dir(workspace.PendingPath)); err != nil {
		return err
	}
	operationMarker := OperationMarker{
		SchemaVersion: markerSchemaVersion, OperationID: workspace.OperationID,
		Kind: "create", WorkspaceID: workspace.ID, FinalControlRoot: workspace.ControlRoot,
	}
	if err := writeJSONMarker(filepath.Join(workspace.PendingPath, "operation.json"), operationMarker); err != nil {
		return err
	}
	if err := r.runCreateHook(CreateStageOperationMarked); err != nil {
		return err
	}
	workspaceMarker := WorkspaceMarker{
		SchemaVersion: markerSchemaVersion, WorkspaceID: workspace.ID,
		ProjectID: workspace.ProjectID, ContextScopeID: workspace.ContextScopeID,
		Team: workspace.TeamName, ManagedBy: "hufu", CreatedAt: workspace.CreatedAt,
	}
	if err := writeJSONMarker(filepath.Join(workspace.PendingPath, "workspace.json"), workspaceMarker); err != nil {
		return err
	}
	return r.runCreateHook(CreateStageWorkspaceMarked)
}

func (r *SQLiteRegistry) publishWorkspace(workspace Workspace) error {
	if _, err := os.Lstat(workspace.ControlRoot); err == nil {
		return fmt.Errorf("%w: final workspace path already exists at %q", ErrConflict, workspace.ControlRoot)
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("inspect final workspace path: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(workspace.ControlRoot), 0o700); err != nil {
		return fmt.Errorf("create project teams directory: %w", err)
	}
	if err := r.syncManagedAncestors(filepath.Dir(workspace.ControlRoot)); err != nil {
		return err
	}
	if err := os.Rename(workspace.PendingPath, workspace.ControlRoot); err != nil {
		return fmt.Errorf("publish workspace: %w", err)
	}
	if err := syncDirectory(filepath.Dir(workspace.ControlRoot)); err != nil {
		return err
	}
	return r.runCreateHook(CreateStageRenamed)
}

func (r *SQLiteRegistry) syncManagedAncestors(start string) error {
	for current := start; ; current = filepath.Dir(current) {
		if err := syncDirectory(current); err != nil {
			return err
		}
		if current == r.stateRoot {
			return nil
		}
		parent := filepath.Dir(current)
		if parent == current || !pathContains(r.stateRoot, parent) {
			return fmt.Errorf("managed path %q escapes state root %q", start, r.stateRoot)
		}
	}
}

func (r *SQLiteRegistry) activateWorkspace(ctx context.Context, workspace Workspace) error {
	completedAt := r.now().UTC()
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("activate workspace: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	result, err := tx.ExecContext(ctx, `UPDATE workspaces SET state='active',operation_id=NULL,pending_path=NULL,updated_at=? WHERE id=? AND state='creating' AND operation_id=?`, completedAt.UnixMilli(), workspace.ID, workspace.OperationID)
	if err != nil {
		return fmt.Errorf("activate workspace row: %w", err)
	}
	if err = requireAffected(result, "workspace", workspace.ID); err != nil {
		return err
	}
	result, err = tx.ExecContext(ctx, `UPDATE registry_operations SET state='completed',finished_at=? WHERE id=? AND state='started'`, completedAt.UnixMilli(), workspace.OperationID)
	if err != nil {
		return fmt.Errorf("complete workspace operation: %w", err)
	}
	if err = requireAffected(result, "operation", workspace.OperationID); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("commit workspace activation: %w", err)
	}
	return nil
}

func (r *SQLiteRegistry) runCreateHook(stage CreateStage) error {
	if r.createHook == nil {
		return nil
	}
	if err := r.createHook(stage); err != nil {
		return fmt.Errorf("simulated create interruption at %s: %w", stage, err)
	}
	return nil
}

func (r *SQLiteRegistry) GetOperation(ctx context.Context, operationID string) (Operation, error) {
	return scanOperation(r.db.QueryRowContext(ctx, operationSelect+" WHERE id=?", operationID))
}

func (r *SQLiteRegistry) ListIncompleteOperations(ctx context.Context) ([]Operation, error) {
	rows, err := r.db.QueryContext(ctx, operationSelect+" WHERE state<>'completed' ORDER BY started_at,id")
	if err != nil {
		return nil, fmt.Errorf("list incomplete registry operations: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var operations []Operation
	for rows.Next() {
		operation, scanErr := scanOperation(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		operations = append(operations, operation)
	}
	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("list incomplete registry operations: %w", err)
	}
	return operations, nil
}

func (r *SQLiteRegistry) ListTrashWorkspaces(ctx context.Context) ([]TrashWorkspace, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT trash_id,workspace_id,project_id,team_name,context_scope_id,original_control_root,trash_path,state,COALESCE(operation_id,''),requires_fresh_session,deleted_at,purge_after FROM trash_workspaces ORDER BY project_id,team_name,trash_id`)
	if err != nil {
		return nil, fmt.Errorf("list trash workspaces: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var trash []TrashWorkspace
	for rows.Next() {
		var item TrashWorkspace
		var requiresFresh int
		var deletedAt int64
		var purgeAfter sql.NullInt64
		if err = rows.Scan(&item.TrashID, &item.WorkspaceID, &item.ProjectID, &item.TeamName, &item.ContextScopeID, &item.OriginalControlRoot, &item.TrashPath, &item.State, &item.OperationID, &requiresFresh, &deletedAt, &purgeAfter); err != nil {
			return nil, fmt.Errorf("scan trash workspace: %w", err)
		}
		item.RequiresFreshSession = requiresFresh != 0
		item.DeletedAt = time.UnixMilli(deletedAt).UTC()
		item.PurgeAfter = nullableTime(purgeAfter)
		trash = append(trash, item)
	}
	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("list trash workspaces: %w", err)
	}
	return trash, nil
}

func (r *SQLiteRegistry) GetTrashWorkspace(ctx context.Context, trashID string) (TrashWorkspace, error) {
	return scanTrashWorkspace(r.db.QueryRowContext(ctx, `SELECT trash_id,workspace_id,project_id,team_name,context_scope_id,original_control_root,trash_path,state,COALESCE(operation_id,''),requires_fresh_session,deleted_at,purge_after FROM trash_workspaces WHERE trash_id=?`, trashID))
}

func scanTrashWorkspace(row rowScanner) (TrashWorkspace, error) {
	var item TrashWorkspace
	var requiresFresh int
	var deletedAt int64
	var purgeAfter sql.NullInt64
	if err := row.Scan(&item.TrashID, &item.WorkspaceID, &item.ProjectID, &item.TeamName, &item.ContextScopeID, &item.OriginalControlRoot, &item.TrashPath, &item.State, &item.OperationID, &requiresFresh, &deletedAt, &purgeAfter); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return TrashWorkspace{}, ErrNotFound
		}
		return TrashWorkspace{}, fmt.Errorf("scan trash workspace: %w", err)
	}
	item.RequiresFreshSession = requiresFresh != 0
	item.DeletedAt = time.UnixMilli(deletedAt).UTC()
	item.PurgeAfter = nullableTime(purgeAfter)
	return item, nil
}

func (r *SQLiteRegistry) SetWorkspaceRequiresFreshSession(ctx context.Context, workspaceID string, required bool) error {
	if r.readOnly {
		return errors.New("update workspace fresh-session requirement: registry is read-only")
	}
	value := 0
	if required {
		value = 1
	}
	result, err := r.db.ExecContext(ctx, "UPDATE workspaces SET requires_fresh_session=?,updated_at=? WHERE id=?", value, r.now().UTC().UnixMilli(), workspaceID)
	if err != nil {
		return fmt.Errorf("update workspace fresh-session requirement: %w", err)
	}
	return requireAffected(result, "workspace", workspaceID)
}

func (r *SQLiteRegistry) GetWorkspaceByID(ctx context.Context, workspaceID string) (Workspace, error) {
	return scanWorkspace(r.db.QueryRowContext(ctx, workspaceSelect+" WHERE id=?", workspaceID))
}

const operationSelect = `SELECT id,kind,COALESCE(project_id,''),COALESCE(workspace_id,''),state,detail_code,started_at,finished_at FROM registry_operations`

func scanOperation(row rowScanner) (Operation, error) {
	var operation Operation
	var startedAt int64
	var finishedAt sql.NullInt64
	err := row.Scan(&operation.ID, &operation.Kind, &operation.ProjectID, &operation.WorkspaceID, &operation.State, &operation.DetailCode, &startedAt, &finishedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Operation{}, ErrNotFound
	}
	if err != nil {
		return Operation{}, fmt.Errorf("scan registry operation: %w", err)
	}
	operation.StartedAt = time.UnixMilli(startedAt).UTC()
	operation.FinishedAt = nullableTime(finishedAt)
	return operation, nil
}

func scanWorkspace(row rowScanner) (Workspace, error) {
	var workspace Workspace
	var requiresFresh int
	var createdAt, updatedAt int64
	var lastUsedAt sql.NullInt64
	err := row.Scan(&workspace.ID, &workspace.ProjectID, &workspace.TeamName, &workspace.ContextScopeID, &workspace.ControlRoot, &workspace.State, &workspace.OperationID, &workspace.PendingPath, &requiresFresh, &createdAt, &updatedAt, &lastUsedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Workspace{}, ErrNotFound
	}
	if err != nil {
		return Workspace{}, fmt.Errorf("scan workspace: %w", err)
	}
	workspace.RequiresFreshSession = requiresFresh != 0
	workspace.CreatedAt = time.UnixMilli(createdAt).UTC()
	workspace.UpdatedAt = time.UnixMilli(updatedAt).UTC()
	workspace.LastUsedAt = nullableTime(lastUsedAt)
	return workspace, nil
}
