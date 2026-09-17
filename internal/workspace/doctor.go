package workspace

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"

	contextstore "github.com/kjelly/hufu/internal/context"
)

const (
	IssueRegistryIntegrityFailed   = "registry_integrity_failed"
	IssueMigrationChecksumMismatch = "migration_checksum_mismatch"
	IssueProjectRootMissing        = "project_root_missing"
	IssueWorkspaceMissing          = "workspace_missing"
	IssueWorkspaceMarkerMissing    = "workspace_marker_missing"
	IssueWorkspaceMarkerMismatch   = "workspace_marker_mismatch"
	IssueScopeOverlap              = "scope_overlap"
	IssueCreatingIncomplete        = "creating_incomplete"
	IssueDeletingIncomplete        = "deleting_incomplete"
	IssueRestoringIncomplete       = "restoring_incomplete"
	IssuePurgingIncomplete         = "purging_incomplete"
	IssueOrphanStaging             = "orphan_staging"
	IssueOrphanManagedWorkspace    = "orphan_managed_workspace"
	IssueTrashMissing              = "trash_missing"
	IssueLegacySourcePresent       = "legacy_source_present"
	IssueContextQuickCheckFailed   = "context_quick_check_failed"
	IssueEventChainInvalid         = "event_chain_invalid"
	IssueWorkspaceBusy             = "workspace_busy"
)

func (m *WorkspaceManager) Doctor(ctx context.Context, request DoctorRequest) (DoctorResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	result := DoctorResult{Outcome: "complete", Issues: []DoctorIssue{}}
	registry, err := OpenReadOnly(m.stateRoot)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return result, nil
		}
		result.Outcome = "failed"
		result.Issues = append(result.Issues, DoctorIssue{Code: IssueRegistryIntegrityFailed, Detail: err.Error()})
		return result, nil
	}
	projects, err := registry.ListProjects(ctx, ListOptions{})
	if err != nil {
		_ = registry.Close()
		return DoctorResult{}, err
	}
	if request.Selector != "" {
		project, resolveErr := registry.ResolveProject(ctx, request.Selector)
		if resolveErr != nil {
			_ = registry.Close()
			return DoctorResult{}, resolveErr
		}
		projects = []Project{project}
	} else if request.StartDir != "" {
		subject, discoverErr := DiscoverSubjectRoot(request.StartDir)
		if discoverErr == nil {
			if project, resolveErr := registry.ResolveProjectByRoot(ctx, subject); resolveErr == nil {
				projects = []Project{project}
			}
		}
	}
	for _, project := range projects {
		m.inspectDoctorProject(ctx, registry, project, &result)
	}
	trash, trashErr := registry.ListTrashWorkspaces(ctx)
	if trashErr == nil {
		for _, item := range trash {
			if _, statErr := os.Lstat(item.TrashPath); os.IsNotExist(statErr) {
				code := IssueTrashMissing
				if item.State == "purging" {
					code = IssuePurgingIncomplete
				}
				result.Issues = append(result.Issues, DoctorIssue{Code: code, ProjectID: item.ProjectID, WorkspaceID: item.WorkspaceID, Path: item.TrashPath})
			}
		}
	}
	_ = registry.Close()
	m.inspectOrphanStaging(&result)
	sortDoctorIssues(result.Issues)
	if request.Repair {
		if repairErr := m.repairDoctorIssues(ctx, &result); repairErr != nil {
			result.Outcome = "failed"
			return result, repairErr
		}
	}
	if result.Outcome == "complete" {
		for _, issue := range result.Issues {
			if !issue.Repaired {
				result.Outcome = "partial"
				break
			}
		}
	}
	return result, nil
}

func (m *WorkspaceManager) inspectDoctorProject(ctx context.Context, registry *SQLiteRegistry, project Project, result *DoctorResult) {
	if info, err := os.Stat(project.SubjectRoot); err != nil || !info.IsDir() {
		result.Issues = append(result.Issues, DoctorIssue{Code: IssueProjectRootMissing, ProjectID: project.ID, Path: project.SubjectRoot})
	}
	if pathsOverlap(project.SubjectRoot, m.stateRoot) {
		result.Issues = append(result.Issues, DoctorIssue{Code: IssueScopeOverlap, ProjectID: project.ID, Path: project.SubjectRoot})
	}
	workspaces, err := registry.ListWorkspaces(ctx, project.ID)
	if err != nil {
		result.Issues = append(result.Issues, DoctorIssue{Code: IssueRegistryIntegrityFailed, ProjectID: project.ID, Detail: err.Error()})
		return
	}
	knownRoots := make(map[string]struct{}, len(workspaces))
	for _, workspace := range workspaces {
		knownRoots[workspace.ControlRoot] = struct{}{}
		switch workspace.State {
		case "creating":
			result.Issues = append(result.Issues, DoctorIssue{Code: IssueCreatingIncomplete, ProjectID: project.ID, WorkspaceID: workspace.ID, Path: workspace.PendingPath})
		case "deleting":
			result.Issues = append(result.Issues, DoctorIssue{Code: IssueDeletingIncomplete, ProjectID: project.ID, WorkspaceID: workspace.ID, Path: workspace.ControlRoot})
		case "restoring":
			result.Issues = append(result.Issues, DoctorIssue{Code: IssueRestoringIncomplete, ProjectID: project.ID, WorkspaceID: workspace.ID, Path: workspace.ControlRoot})
		case "active":
			m.inspectActiveWorkspace(ctx, project, workspace, result)
		}
		legacy := filepath.Join(project.SubjectRoot, "workspace", workspace.TeamName)
		if info, statErr := os.Stat(legacy); statErr == nil && info.IsDir() {
			result.Issues = append(result.Issues, DoctorIssue{Code: IssueLegacySourcePresent, ProjectID: project.ID, WorkspaceID: workspace.ID, Path: legacy})
		}
	}
	teamsRoot := filepath.Join(project.StateDir, "teams")
	entries, readErr := os.ReadDir(teamsRoot)
	if readErr == nil {
		for _, entry := range entries {
			path := filepath.Join(teamsRoot, entry.Name())
			if entry.IsDir() {
				if _, ok := knownRoots[path]; !ok {
					result.Issues = append(result.Issues, DoctorIssue{Code: IssueOrphanManagedWorkspace, ProjectID: project.ID, Path: path})
				}
			}
		}
	}
}

func (m *WorkspaceManager) inspectActiveWorkspace(ctx context.Context, project Project, workspace Workspace, result *DoctorResult) {
	info, err := os.Lstat(workspace.ControlRoot)
	if os.IsNotExist(err) {
		result.Issues = append(result.Issues, DoctorIssue{Code: IssueWorkspaceMissing, ProjectID: project.ID, WorkspaceID: workspace.ID, Path: workspace.ControlRoot})
		return
	}
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		result.Issues = append(result.Issues, DoctorIssue{Code: IssueWorkspaceMarkerMismatch, ProjectID: project.ID, WorkspaceID: workspace.ID, Path: workspace.ControlRoot})
		return
	}
	marker, markerErr := decodeWorkspaceMarker(filepath.Join(workspace.ControlRoot, "workspace.json"))
	if os.IsNotExist(markerErr) {
		result.Issues = append(result.Issues, DoctorIssue{Code: IssueWorkspaceMarkerMissing, ProjectID: project.ID, WorkspaceID: workspace.ID, Path: workspace.ControlRoot})
	} else if markerErr != nil || !workspaceMarkerMatches(marker, workspace) {
		detail := "marker identity does not match registry"
		if markerErr != nil {
			detail = markerErr.Error()
		}
		result.Issues = append(result.Issues, DoctorIssue{Code: IssueWorkspaceMarkerMismatch, ProjectID: project.ID, WorkspaceID: workspace.ID, Path: workspace.ControlRoot, Detail: detail})
	}
	contextPath := filepath.Join(workspace.ControlRoot, legacyContextFilename)
	if _, statErr := os.Stat(contextPath); statErr == nil {
		if _, inspectErr := contextstore.InspectSQLiteSnapshot(ctx, contextPath); inspectErr != nil {
			code := IssueContextQuickCheckFailed
			if strings.Contains(inspectErr.Error(), "migration checksum") {
				code = IssueMigrationChecksumMismatch
			}
			result.Issues = append(result.Issues, DoctorIssue{Code: code, ProjectID: project.ID, WorkspaceID: workspace.ID, Path: contextPath, Detail: inspectErr.Error()})
		}
	}
	eventPath := filepath.Join(workspace.ControlRoot, "logs", "event_store.jsonl")
	if _, statErr := os.Stat(eventPath); statErr == nil {
		if verifyErr := verifyEventChain(eventPath); verifyErr != nil {
			result.Issues = append(result.Issues, DoctorIssue{Code: IssueEventChainInvalid, ProjectID: project.ID, WorkspaceID: workspace.ID, Path: eventPath, Detail: verifyErr.Error()})
		}
	}
}

func (m *WorkspaceManager) inspectOrphanStaging(result *DoctorResult) {
	root := filepath.Join(m.stateRoot, "staging")
	known := make(map[string]struct{})
	if registry, err := OpenReadOnly(m.stateRoot); err == nil {
		if projects, listErr := registry.ListProjects(context.Background(), ListOptions{}); listErr == nil {
			for _, project := range projects {
				if workspaces, workspaceErr := registry.ListWorkspaces(context.Background(), project.ID); workspaceErr == nil {
					for _, workspace := range workspaces {
						if workspace.PendingPath != "" {
							known[workspace.PendingPath] = struct{}{}
						}
					}
				}
			}
		}
		_ = registry.Close()
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if entry.IsDir() {
			path := filepath.Join(root, entry.Name())
			if _, exists := known[path]; !exists {
				result.Issues = append(result.Issues, DoctorIssue{Code: IssueOrphanStaging, Path: path})
			}
		}
	}
}

func (m *WorkspaceManager) repairDoctorIssues(ctx context.Context, result *DoctorResult) error {
	registry, err := OpenReadWrite(m.stateRoot, m.registryOptions...)
	if err != nil {
		return err
	}
	defer func() { _ = registry.Close() }()
	for index := range result.Issues {
		issue := &result.Issues[index]
		if issue.Code == IssueOrphanStaging {
			repaired, repairErr := registry.repairOrphanStaging(ctx, issue.Path)
			if repairErr == nil {
				issue.Repaired = repaired
			}
			continue
		}
		if issue.Code != IssueCreatingIncomplete || issue.WorkspaceID == "" {
			continue
		}
		workspace, lookupErr := registry.GetWorkspaceByID(ctx, issue.WorkspaceID)
		if lookupErr != nil {
			continue
		}
		locks, lockErr := AcquireWorkspaceLocks(m.stateRoot, []string{workspace.ID})
		if lockErr != nil {
			if errors.Is(lockErr, ErrBusy) {
				result.Issues = append(result.Issues, DoctorIssue{Code: IssueWorkspaceBusy, ProjectID: workspace.ProjectID, WorkspaceID: workspace.ID})
				continue
			}
			return lockErr
		}
		repaired, repairErr := registry.repairCreating(ctx, workspace)
		_ = locks.Close()
		if repairErr != nil {
			continue
		}
		issue.Repaired = repaired
	}
	return nil
}

func (r *SQLiteRegistry) repairCreating(ctx context.Context, workspace Workspace) (bool, error) {
	stagingExists := doctorPathExists(workspace.PendingPath)
	finalExists := doctorPathExists(workspace.ControlRoot)
	if stagingExists && finalExists {
		return false, errors.New("both staging and final paths exist")
	}
	repairID, err := r.idGenerator.New(OperationIDKind)
	if err != nil {
		return false, err
	}
	if err = r.startRepairOperation(ctx, repairID, workspace); err != nil {
		return false, err
	}
	complete := false
	defer func() {
		if !complete {
			_ = r.finishRepairOperation(context.Background(), repairID, "failed", "validation_failed")
		}
	}()
	switch {
	case finalExists && ownedDirectory(workspace.ControlRoot) && markerMatchesWorkspace(workspace.ControlRoot, workspace):
		err = r.activateWorkspace(ctx, workspace)
	case stagingExists && ownedDirectory(workspace.PendingPath) && markerMatchesOperation(workspace.PendingPath, workspace.OperationID, workspace.ID) && markerMatchesWorkspace(workspace.PendingPath, workspace):
		operation, operationErr := r.GetOperation(ctx, workspace.OperationID)
		if operationErr != nil {
			return false, operationErr
		}
		if operation.Kind == operationMigrate {
			if _, inspectErr := contextstore.InspectSQLiteSnapshot(ctx, filepath.Join(workspace.PendingPath, legacyContextFilename)); inspectErr != nil {
				return false, inspectErr
			}
		}
		if err = verifyLegacySession(workspace.PendingPath); err == nil {
			err = r.publishWorkspace(workspace)
		}
		if err == nil {
			err = r.activateWorkspace(ctx, workspace)
		}
	case !stagingExists && !finalExists:
		err = r.removeCreatingWorkspace(ctx, workspace)
	default:
		return false, errors.New("creating workspace markers do not match registry")
	}
	if err != nil {
		return false, err
	}
	if err = r.finishRepairOperation(ctx, repairID, "completed", ""); err != nil {
		return false, err
	}
	complete = true
	return true, nil
}

func (r *SQLiteRegistry) repairOrphanStaging(ctx context.Context, path string) (bool, error) {
	canonical, err := canonicalPath(path)
	if err != nil {
		return false, err
	}
	stagingRoot := filepath.Join(r.stateRoot, "staging")
	if filepath.Dir(canonical) != stagingRoot {
		return false, errors.New("orphan staging path is not a direct staging descendant")
	}
	marker, err := decodeOperationMarker(filepath.Join(canonical, "operation.json"))
	if err != nil {
		return false, err
	}
	operation, err := r.GetOperation(ctx, marker.OperationID)
	if err != nil || operation.WorkspaceID != marker.WorkspaceID || operation.State == "completed" {
		return false, errors.New("orphan staging marker has no failed or incomplete operation")
	}
	locks, err := AcquireWorkspaceLocks(r.stateRoot, []string{marker.WorkspaceID})
	if err != nil {
		return false, err
	}
	defer func() { _ = locks.Close() }()
	repairID, err := r.idGenerator.New(OperationIDKind)
	if err != nil {
		return false, err
	}
	workspace := Workspace{ID: marker.WorkspaceID, ProjectID: operation.ProjectID}
	if err = r.startRepairOperation(ctx, repairID, workspace); err != nil {
		return false, err
	}
	if err = os.RemoveAll(canonical); err != nil {
		_ = r.finishRepairOperation(context.Background(), repairID, "failed", "remove_failed")
		return false, err
	}
	if err = r.finishRepairOperation(ctx, repairID, "completed", ""); err != nil {
		return false, err
	}
	return true, nil
}

func (r *SQLiteRegistry) startRepairOperation(ctx context.Context, repairID string, workspace Workspace) error {
	_, err := r.db.ExecContext(ctx, `INSERT INTO registry_operations(id,kind,project_id,workspace_id,state,started_at) VALUES(?,?,?,?,?,?)`, repairID, "repair", workspace.ProjectID, workspace.ID, "started", r.now().UTC().UnixMilli())
	return err
}

func (r *SQLiteRegistry) finishRepairOperation(ctx context.Context, repairID, state, detail string) error {
	_, err := r.db.ExecContext(ctx, "UPDATE registry_operations SET state=?,detail_code=?,finished_at=? WHERE id=? AND state='started'", state, detail, r.now().UTC().UnixMilli(), repairID)
	return err
}

func (r *SQLiteRegistry) removeCreatingWorkspace(ctx context.Context, workspace Workspace) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err = tx.ExecContext(ctx, "DELETE FROM workspaces WHERE id=? AND state='creating' AND operation_id=?", workspace.ID, workspace.OperationID); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, "UPDATE registry_operations SET state='failed',detail_code='creating_incomplete',finished_at=? WHERE id=? AND state='started'", r.now().UTC().UnixMilli(), workspace.OperationID); err != nil {
		return err
	}
	return tx.Commit()
}

func doctorPathExists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

func ownedDirectory(path string) bool {
	info, err := os.Lstat(path)
	return err == nil && info.IsDir() && info.Mode()&os.ModeSymlink == 0
}

func decodeWorkspaceMarker(path string) (WorkspaceMarker, error) {
	var marker WorkspaceMarker
	data, err := os.ReadFile(path)
	if err != nil {
		return marker, err
	}
	if err = json.Unmarshal(data, &marker); err != nil {
		return marker, err
	}
	return marker, nil
}

func workspaceMarkerMatches(marker WorkspaceMarker, workspace Workspace) bool {
	return marker.SchemaVersion == markerSchemaVersion && marker.ManagedBy == "hufu" &&
		marker.WorkspaceID == workspace.ID && marker.ProjectID == workspace.ProjectID &&
		marker.ContextScopeID == workspace.ContextScopeID && marker.Team == workspace.TeamName
}

func markerMatchesWorkspace(root string, workspace Workspace) bool {
	marker, err := decodeWorkspaceMarker(filepath.Join(root, "workspace.json"))
	return err == nil && workspaceMarkerMatches(marker, workspace)
}

func sortDoctorIssues(issues []DoctorIssue) {
	sort.SliceStable(issues, func(i, j int) bool {
		first := issues[i].Code + "\x00" + issues[i].ProjectID + "\x00" + issues[i].WorkspaceID + "\x00" + issues[i].Path
		second := issues[j].Code + "\x00" + issues[j].ProjectID + "\x00" + issues[j].WorkspaceID + "\x00" + issues[j].Path
		return first < second
	})
}
