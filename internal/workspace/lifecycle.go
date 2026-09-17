package workspace

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

func (m *WorkspaceManager) Delete(ctx context.Context, request DeleteRequest) (DeleteResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	readRegistry, err := OpenReadOnly(m.stateRoot)
	if err != nil {
		return DeleteResult{}, err
	}
	project, err := resolveLifecycleProject(ctx, readRegistry, request.StartDir, request.Selector)
	if err != nil {
		_ = readRegistry.Close()
		return DeleteResult{}, err
	}
	var targets []Workspace
	if request.AllTeams {
		targets, err = readRegistry.ListWorkspaces(ctx, project.ID)
	} else {
		teamName := request.TeamName
		if teamName == "" {
			teamName = "default"
		}
		var workspace Workspace
		workspace, err = readRegistry.GetWorkspace(ctx, project.ID, teamName)
		targets = []Workspace{workspace}
	}
	if closeErr := readRegistry.Close(); err != nil || closeErr != nil {
		return DeleteResult{}, errors.Join(err, closeErr)
	}
	if len(targets) == 0 {
		return DeleteResult{}, fmt.Errorf("%w: project %s has no workspaces", ErrNotFound, project.ID)
	}
	sort.Slice(targets, func(i, j int) bool { return targets[i].ID < targets[j].ID })
	workspaceIDs := make([]string, 0, len(targets))
	for _, workspace := range targets {
		if workspace.State != "active" {
			return DeleteResult{}, fmt.Errorf("%w: workspace %s is %s", ErrConflict, workspace.ID, workspace.State)
		}
		if err = m.validateActiveWorkspace(project, workspace); err != nil {
			return DeleteResult{}, err
		}
		workspaceIDs = append(workspaceIDs, workspace.ID)
	}
	locks, err := AcquireWorkspaceLocks(m.stateRoot, workspaceIDs)
	if err != nil {
		return DeleteResult{}, err
	}
	defer func() { _ = locks.Close() }()
	registry, err := OpenReadWrite(m.stateRoot, m.registryOptions...)
	if err != nil {
		return DeleteResult{}, err
	}
	defer func() { _ = registry.Close() }()
	for index := range targets {
		targets[index], err = registry.GetWorkspaceByID(ctx, targets[index].ID)
		if err != nil {
			return DeleteResult{}, err
		}
		if err = m.validateActiveWorkspace(project, targets[index]); err != nil {
			return DeleteResult{}, err
		}
	}
	result := DeleteResult{Outcome: "complete", Items: []LifecycleItem{}}
	for _, workspace := range targets {
		item, deleteErr := registry.deleteWorkspaceLocked(ctx, project, workspace, request.Retention)
		if item.OperationID != "" {
			result.Items = append(result.Items, item)
		}
		if deleteErr != nil {
			if item.OperationID != "" {
				result.Outcome = "partial"
			} else {
				result.Outcome = "failed"
			}
			return result, deleteErr
		}
	}
	return result, nil
}

func (m *WorkspaceManager) Restore(ctx context.Context, request RestoreRequest) (RestoreResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	trash, project, err := m.loadTrashForLifecycle(ctx, request.TrashID)
	if err != nil {
		return RestoreResult{}, err
	}
	if err = m.validateTrashWorkspace(project, trash); err != nil {
		return RestoreResult{}, err
	}
	if _, statErr := os.Lstat(trash.OriginalControlRoot); statErr == nil {
		return RestoreResult{}, fmt.Errorf("%w: restore destination already exists at %q", ErrConflict, trash.OriginalControlRoot)
	} else if !os.IsNotExist(statErr) {
		return RestoreResult{}, fmt.Errorf("inspect restore destination: %w", statErr)
	}
	locks, err := AcquireWorkspaceLocks(m.stateRoot, []string{trash.WorkspaceID})
	if err != nil {
		return RestoreResult{}, err
	}
	defer func() { _ = locks.Close() }()
	registry, err := OpenReadWrite(m.stateRoot, m.registryOptions...)
	if err != nil {
		return RestoreResult{}, err
	}
	defer func() { _ = registry.Close() }()
	trash, err = registry.GetTrashWorkspace(ctx, request.TrashID)
	if err != nil {
		return RestoreResult{}, err
	}
	if err = m.validateTrashWorkspace(project, trash); err != nil {
		return RestoreResult{}, err
	}
	item, restoreErr := registry.restoreWorkspaceLocked(ctx, trash)
	result := RestoreResult{Outcome: "complete", Items: []LifecycleItem{}}
	if item.OperationID != "" {
		result.Items = append(result.Items, item)
	}
	if restoreErr != nil {
		if item.OperationID != "" {
			result.Outcome = "partial"
		} else {
			result.Outcome = "failed"
		}
	}
	return result, restoreErr
}

func (m *WorkspaceManager) Purge(ctx context.Context, request PurgeRequest) (PurgeResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	trash, project, err := m.loadTrashForLifecycle(ctx, request.TrashID)
	if err != nil {
		return PurgeResult{}, err
	}
	if err = m.validateTrashWorkspace(project, trash); err != nil {
		return PurgeResult{}, err
	}
	locks, err := AcquireWorkspaceLocks(m.stateRoot, []string{trash.WorkspaceID})
	if err != nil {
		return PurgeResult{}, err
	}
	defer func() { _ = locks.Close() }()
	registry, err := OpenReadWrite(m.stateRoot, m.registryOptions...)
	if err != nil {
		return PurgeResult{}, err
	}
	defer func() { _ = registry.Close() }()
	trash, err = registry.GetTrashWorkspace(ctx, request.TrashID)
	if err != nil {
		return PurgeResult{}, err
	}
	if err = m.validateTrashWorkspace(project, trash); err != nil {
		return PurgeResult{}, err
	}
	item, purgeErr := registry.purgeWorkspaceLocked(ctx, trash)
	result := PurgeResult{Outcome: "complete", Items: []LifecycleItem{}}
	if item.OperationID != "" {
		result.Items = append(result.Items, item)
	}
	if purgeErr != nil {
		if item.OperationID != "" {
			result.Outcome = "partial"
		} else {
			result.Outcome = "failed"
		}
	}
	return result, purgeErr
}

func resolveLifecycleProject(ctx context.Context, registry *SQLiteRegistry, startDir, selector string) (Project, error) {
	if selector != "" {
		return registry.ResolveProject(ctx, selector)
	}
	subjectRoot, err := DiscoverSubjectRoot(startDir)
	if err != nil {
		return Project{}, err
	}
	return registry.ResolveProjectByRoot(ctx, subjectRoot)
}

func (m *WorkspaceManager) loadTrashForLifecycle(ctx context.Context, trashID string) (TrashWorkspace, Project, error) {
	registry, err := OpenReadOnly(m.stateRoot)
	if err != nil {
		return TrashWorkspace{}, Project{}, err
	}
	trash, lookupErr := registry.GetTrashWorkspace(ctx, trashID)
	var project Project
	if lookupErr == nil {
		project, lookupErr = registry.ResolveProject(ctx, trash.ProjectID)
	}
	if closeErr := registry.Close(); lookupErr != nil || closeErr != nil {
		return TrashWorkspace{}, Project{}, errors.Join(lookupErr, closeErr)
	}
	return trash, project, nil
}

func (m *WorkspaceManager) validateActiveWorkspace(project Project, workspace Workspace) error {
	if workspace.ProjectID != project.ID || workspace.State != "active" {
		return fmt.Errorf("%w: active workspace identity changed", ErrConflict)
	}
	if err := m.validateManagedControlRoot(project, workspace.ControlRoot, workspace.TeamName); err != nil {
		return err
	}
	if !markerMatchesWorkspace(workspace.ControlRoot, workspace) {
		return fmt.Errorf("%w: workspace marker does not match registry for %s", ErrConflict, workspace.ID)
	}
	return nil
}

func (m *WorkspaceManager) validateManagedControlRoot(project Project, root, teamName string) error {
	info, err := os.Lstat(root)
	if err != nil {
		return fmt.Errorf("inspect managed workspace root: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("%w: managed workspace root is not a real directory: %q", ErrConflict, root)
	}
	canonical, err := CanonicalExistingDirectory(root)
	if err != nil {
		return err
	}
	expected, err := canonicalPath(filepath.Join(project.StateDir, "teams", teamName))
	if err != nil {
		return err
	}
	projectsRoot, err := canonicalPath(filepath.Join(m.stateRoot, "projects"))
	if err != nil {
		return err
	}
	if canonical != expected || canonical == projectsRoot || !pathContains(projectsRoot, canonical) {
		return fmt.Errorf("%w: managed workspace root %q is outside its registered location", ErrConflict, canonical)
	}
	if pathsOverlap(canonical, project.SubjectRoot) {
		return fmt.Errorf("%w: managed workspace root %q overlaps subject root %q", ErrConflict, canonical, project.SubjectRoot)
	}
	if filepath.Dir(canonical) == canonical || canonical == m.stateRoot || pathContains(canonical, m.stateRoot) {
		return fmt.Errorf("%w: refusing protected workspace path %q", ErrConflict, canonical)
	}
	if home, homeErr := os.UserHomeDir(); homeErr == nil {
		if canonicalHome, canonicalErr := canonicalPath(home); canonicalErr == nil && (canonical == canonicalHome || pathContains(canonical, canonicalHome)) {
			return fmt.Errorf("%w: refusing protected home path %q", ErrConflict, canonical)
		}
	}
	return nil
}

func (m *WorkspaceManager) validateTrashWorkspace(project Project, trash TrashWorkspace) error {
	if trash.ProjectID != project.ID || trash.State != "trashed" {
		return fmt.Errorf("%w: trash workspace %s is %s", ErrConflict, trash.TrashID, trash.State)
	}
	info, err := os.Lstat(trash.TrashPath)
	if err != nil {
		return fmt.Errorf("inspect trash workspace: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("%w: trash workspace is not a real directory", ErrConflict)
	}
	canonical, err := CanonicalExistingDirectory(trash.TrashPath)
	if err != nil {
		return err
	}
	trashRoot, err := canonicalPath(filepath.Join(m.stateRoot, "trash"))
	if err != nil {
		return err
	}
	if filepath.Dir(canonical) != trashRoot || canonical != trash.TrashPath {
		return fmt.Errorf("%w: trash path %q is not a direct managed trash entry", ErrConflict, canonical)
	}
	workspace := workspaceFromTrash(trash)
	if !markerMatchesWorkspace(trash.TrashPath, workspace) {
		return fmt.Errorf("%w: trash marker does not match registry for %s", ErrConflict, trash.TrashID)
	}
	if err = m.validateRestoreDestination(project, workspace); err != nil {
		return err
	}
	return nil
}

func (m *WorkspaceManager) validateRestoreDestination(project Project, workspace Workspace) error {
	expected, err := canonicalPath(workspace.ControlRoot)
	if err != nil {
		return err
	}
	registered, err := canonicalPath(filepath.Join(project.StateDir, "teams", workspace.TeamName))
	if err != nil {
		return err
	}
	projectsRoot, err := canonicalPath(filepath.Join(m.stateRoot, "projects"))
	if err != nil {
		return err
	}
	if expected != registered || expected == projectsRoot || !pathContains(projectsRoot, expected) || pathsOverlap(expected, project.SubjectRoot) {
		return fmt.Errorf("%w: restore destination %q is not a safe registered workspace path", ErrConflict, expected)
	}
	return nil
}

func workspaceFromTrash(trash TrashWorkspace) Workspace {
	return Workspace{
		ID: trash.WorkspaceID, ProjectID: trash.ProjectID, TeamName: trash.TeamName,
		ContextScopeID: trash.ContextScopeID, ControlRoot: trash.OriginalControlRoot,
		RequiresFreshSession: trash.RequiresFreshSession, CreatedAt: trash.DeletedAt,
	}
}

func (r *SQLiteRegistry) deleteWorkspaceLocked(ctx context.Context, project Project, workspace Workspace, retention time.Duration) (LifecycleItem, error) {
	trashID, err := r.idGenerator.New(TrashIDKind)
	if err != nil {
		return LifecycleItem{}, err
	}
	operationID, err := r.idGenerator.New(OperationIDKind)
	if err != nil {
		return LifecycleItem{}, err
	}
	trashPath := filepath.Join(r.stateRoot, "trash", trashID)
	item := lifecycleItem(operationID, trashID, workspace, trashPath)
	if _, statErr := os.Lstat(trashPath); statErr == nil {
		return LifecycleItem{}, fmt.Errorf("%w: trash destination already exists at %q", ErrConflict, trashPath)
	} else if !os.IsNotExist(statErr) {
		return LifecycleItem{}, statErr
	}
	now := r.now().UTC()
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return LifecycleItem{}, err
	}
	defer func() { _ = tx.Rollback() }()
	result, err := tx.ExecContext(ctx, "UPDATE workspaces SET state='deleting',operation_id=?,pending_path=?,updated_at=? WHERE id=? AND state='active'", operationID, trashPath, now.UnixMilli(), workspace.ID)
	if err != nil {
		return LifecycleItem{}, err
	}
	if err = requireAffected(result, "workspace", workspace.ID); err != nil {
		return LifecycleItem{}, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO registry_operations(id,kind,project_id,workspace_id,state,started_at) VALUES(?,?,?,?,?,?)`, operationID, "delete", project.ID, workspace.ID, "started", now.UnixMilli()); err != nil {
		return LifecycleItem{}, err
	}
	if err = tx.Commit(); err != nil {
		return LifecycleItem{}, err
	}
	if err = r.runLifecycleHook(DeleteStageReserved); err != nil {
		return item, err
	}
	if err = os.MkdirAll(filepath.Dir(trashPath), 0o700); err != nil {
		return item, err
	}
	if err = r.syncManagedAncestors(filepath.Dir(trashPath)); err != nil {
		return item, err
	}
	if err = os.Rename(workspace.ControlRoot, trashPath); err != nil {
		return item, fmt.Errorf("move workspace to trash: %w", err)
	}
	if err = syncDirectory(filepath.Dir(workspace.ControlRoot)); err != nil {
		return item, err
	}
	if err = syncDirectory(filepath.Dir(trashPath)); err != nil {
		return item, err
	}
	if err = r.runLifecycleHook(DeleteStageRenamed); err != nil {
		return item, err
	}
	var purgeAfter any
	if retention > 0 {
		purgeAfter = now.Add(retention).UnixMilli()
	}
	if err = r.completeDelete(ctx, workspace, item, now, purgeAfter); err != nil {
		return item, err
	}
	return item, nil
}

func (r *SQLiteRegistry) completeDelete(ctx context.Context, workspace Workspace, item LifecycleItem, deletedAt time.Time, purgeAfter any) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err = tx.ExecContext(ctx, `INSERT INTO trash_workspaces(trash_id,workspace_id,project_id,team_name,context_scope_id,original_control_root,trash_path,state,requires_fresh_session,deleted_at,purge_after) VALUES(?,?,?,?,?,?,?,?,?,?,?)`, item.TrashID, workspace.ID, workspace.ProjectID, workspace.TeamName, workspace.ContextScopeID, workspace.ControlRoot, item.TrashPath, "trashed", workspace.RequiresFreshSession, deletedAt.UnixMilli(), purgeAfter); err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, "DELETE FROM workspaces WHERE id=? AND state='deleting' AND operation_id=?", workspace.ID, item.OperationID)
	if err != nil {
		return err
	}
	if err = requireAffected(result, "workspace", workspace.ID); err != nil {
		return err
	}
	if err = completeLifecycleOperation(ctx, tx, item.OperationID, deletedAt); err != nil {
		return err
	}
	return tx.Commit()
}

func (r *SQLiteRegistry) restoreWorkspaceLocked(ctx context.Context, trash TrashWorkspace) (LifecycleItem, error) {
	if _, err := r.GetWorkspace(ctx, trash.ProjectID, trash.TeamName); err == nil {
		return LifecycleItem{}, fmt.Errorf("%w: active workspace already exists for %s/%s", ErrConflict, trash.ProjectID, trash.TeamName)
	} else if !errors.Is(err, ErrNotFound) {
		return LifecycleItem{}, err
	}
	if _, err := os.Lstat(trash.OriginalControlRoot); err == nil {
		return LifecycleItem{}, fmt.Errorf("%w: restore destination already exists at %q", ErrConflict, trash.OriginalControlRoot)
	} else if !os.IsNotExist(err) {
		return LifecycleItem{}, err
	}
	operationID, err := r.idGenerator.New(OperationIDKind)
	if err != nil {
		return LifecycleItem{}, err
	}
	workspace := workspaceFromTrash(trash)
	item := lifecycleItem(operationID, trash.TrashID, workspace, trash.TrashPath)
	now := r.now().UTC()
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return LifecycleItem{}, err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err = tx.ExecContext(ctx, `INSERT INTO workspaces(id,project_id,team_name,context_scope_id,control_root,state,operation_id,pending_path,requires_fresh_session,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?)`, workspace.ID, workspace.ProjectID, workspace.TeamName, workspace.ContextScopeID, workspace.ControlRoot, "restoring", operationID, trash.TrashPath, workspace.RequiresFreshSession, trash.DeletedAt.UnixMilli(), now.UnixMilli()); err != nil {
		return LifecycleItem{}, err
	}
	result, err := tx.ExecContext(ctx, "UPDATE trash_workspaces SET state='restoring',operation_id=? WHERE trash_id=? AND state='trashed'", operationID, trash.TrashID)
	if err != nil {
		return LifecycleItem{}, err
	}
	if err = requireAffected(result, "trash workspace", trash.TrashID); err != nil {
		return LifecycleItem{}, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO registry_operations(id,kind,project_id,workspace_id,state,started_at) VALUES(?,?,?,?,?,?)`, operationID, "restore", trash.ProjectID, trash.WorkspaceID, "started", now.UnixMilli()); err != nil {
		return LifecycleItem{}, err
	}
	if err = tx.Commit(); err != nil {
		return LifecycleItem{}, err
	}
	if err = r.runLifecycleHook(RestoreStageReserved); err != nil {
		return item, err
	}
	if err = os.MkdirAll(filepath.Dir(trash.OriginalControlRoot), 0o700); err != nil {
		return item, err
	}
	if err = r.syncManagedAncestors(filepath.Dir(trash.OriginalControlRoot)); err != nil {
		return item, err
	}
	if err = os.Rename(trash.TrashPath, trash.OriginalControlRoot); err != nil {
		return item, fmt.Errorf("restore workspace from trash: %w", err)
	}
	if err = syncDirectory(filepath.Dir(trash.TrashPath)); err != nil {
		return item, err
	}
	if err = syncDirectory(filepath.Dir(trash.OriginalControlRoot)); err != nil {
		return item, err
	}
	if err = r.runLifecycleHook(RestoreStageRenamed); err != nil {
		return item, err
	}
	if err = r.completeRestore(ctx, trash, item, now); err != nil {
		return item, err
	}
	return item, nil
}

func (r *SQLiteRegistry) completeRestore(ctx context.Context, trash TrashWorkspace, item LifecycleItem, completedAt time.Time) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	result, err := tx.ExecContext(ctx, "UPDATE workspaces SET state='active',operation_id=NULL,pending_path=NULL,updated_at=? WHERE id=? AND state='restoring' AND operation_id=?", completedAt.UnixMilli(), trash.WorkspaceID, item.OperationID)
	if err != nil {
		return err
	}
	if err = requireAffected(result, "workspace", trash.WorkspaceID); err != nil {
		return err
	}
	result, err = tx.ExecContext(ctx, "DELETE FROM trash_workspaces WHERE trash_id=? AND state='restoring' AND operation_id=?", trash.TrashID, item.OperationID)
	if err != nil {
		return err
	}
	if err = requireAffected(result, "trash workspace", trash.TrashID); err != nil {
		return err
	}
	if err = completeLifecycleOperation(ctx, tx, item.OperationID, completedAt); err != nil {
		return err
	}
	return tx.Commit()
}

func (r *SQLiteRegistry) purgeWorkspaceLocked(ctx context.Context, trash TrashWorkspace) (LifecycleItem, error) {
	operationID, err := r.idGenerator.New(OperationIDKind)
	if err != nil {
		return LifecycleItem{}, err
	}
	item := lifecycleItem(operationID, trash.TrashID, workspaceFromTrash(trash), trash.TrashPath)
	now := r.now().UTC()
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return LifecycleItem{}, err
	}
	defer func() { _ = tx.Rollback() }()
	result, err := tx.ExecContext(ctx, "UPDATE trash_workspaces SET state='purging',operation_id=? WHERE trash_id=? AND state='trashed'", operationID, trash.TrashID)
	if err != nil {
		return LifecycleItem{}, err
	}
	if err = requireAffected(result, "trash workspace", trash.TrashID); err != nil {
		return LifecycleItem{}, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO registry_operations(id,kind,project_id,workspace_id,state,started_at) VALUES(?,?,?,?,?,?)`, operationID, "purge", trash.ProjectID, trash.WorkspaceID, "started", now.UnixMilli()); err != nil {
		return LifecycleItem{}, err
	}
	if err = tx.Commit(); err != nil {
		return LifecycleItem{}, err
	}
	if err = r.runLifecycleHook(PurgeStageReserved); err != nil {
		return item, err
	}
	if err = os.RemoveAll(trash.TrashPath); err != nil {
		return item, fmt.Errorf("purge trash workspace: %w", err)
	}
	if err = syncDirectory(filepath.Dir(trash.TrashPath)); err != nil {
		return item, err
	}
	if err = r.runLifecycleHook(PurgeStageRemoved); err != nil {
		return item, err
	}
	if err = r.completePurge(ctx, trash, item, now); err != nil {
		return item, err
	}
	return item, nil
}

func (r *SQLiteRegistry) completePurge(ctx context.Context, trash TrashWorkspace, item LifecycleItem, completedAt time.Time) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	result, err := tx.ExecContext(ctx, "DELETE FROM trash_workspaces WHERE trash_id=? AND state='purging' AND operation_id=?", trash.TrashID, item.OperationID)
	if err != nil {
		return err
	}
	if err = requireAffected(result, "trash workspace", trash.TrashID); err != nil {
		return err
	}
	if err = completeLifecycleOperation(ctx, tx, item.OperationID, completedAt); err != nil {
		return err
	}
	return tx.Commit()
}

func completeLifecycleOperation(ctx context.Context, tx *sql.Tx, operationID string, completedAt time.Time) error {
	result, err := tx.ExecContext(ctx, "UPDATE registry_operations SET state='completed',finished_at=? WHERE id=? AND state='started'", completedAt.UnixMilli(), operationID)
	if err != nil {
		return err
	}
	return requireAffected(result, "operation", operationID)
}

func lifecycleItem(operationID, trashID string, workspace Workspace, trashPath string) LifecycleItem {
	return LifecycleItem{
		OperationID: operationID, TrashID: trashID, WorkspaceID: workspace.ID,
		ProjectID: workspace.ProjectID, TeamName: workspace.TeamName,
		ControlRoot: workspace.ControlRoot, TrashPath: trashPath,
	}
}

func (r *SQLiteRegistry) runLifecycleHook(stage LifecycleStage) error {
	if r.lifecycleHook == nil {
		return nil
	}
	if err := r.lifecycleHook(stage); err != nil {
		return fmt.Errorf("simulated lifecycle interruption at %s: %w", stage, err)
	}
	return nil
}
