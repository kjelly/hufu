package workspace

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
)

type RebindResult struct {
	Project    Project     `json:"project"`
	Workspaces []Workspace `json:"workspaces"`
}

func (m *WorkspaceManager) Rebind(ctx context.Context, selector, newRoot string) (RebindResult, error) {
	canonicalNewRoot, err := CanonicalExistingDirectory(newRoot)
	if err != nil {
		return RebindResult{}, fmt.Errorf("rebind project root: %w", err)
	}
	if pathsOverlap(canonicalNewRoot, m.stateRoot) {
		return RebindResult{}, fmt.Errorf("%w: subject root %q overlaps state root %q", ErrConflict, canonicalNewRoot, m.stateRoot)
	}
	registry, err := OpenReadWrite(m.stateRoot, m.registryOptions...)
	if err != nil {
		return RebindResult{}, err
	}
	defer func() { _ = registry.Close() }()
	project, err := registry.ResolveProject(ctx, selector)
	if err != nil {
		return RebindResult{}, err
	}
	workspaces, err := registry.ListWorkspaces(ctx, project.ID)
	if err != nil {
		return RebindResult{}, err
	}
	trash, err := registry.ListTrashWorkspaces(ctx)
	if err != nil {
		return RebindResult{}, err
	}
	workspaceIDs := make([]string, 0, len(workspaces)+len(trash))
	activeTeams := make(map[string]struct{}, len(workspaces))
	for _, workspace := range workspaces {
		workspaceIDs = append(workspaceIDs, workspace.ID)
		if workspace.State == "active" {
			activeTeams[workspace.TeamName] = struct{}{}
		}
	}
	for _, item := range trash {
		if item.ProjectID == project.ID {
			workspaceIDs = append(workspaceIDs, item.WorkspaceID)
		}
	}
	if err = rejectUnmigratedLegacyTeams(project.SubjectRoot, activeTeams); err != nil {
		return RebindResult{}, err
	}
	locks, err := AcquireWorkspaceLocks(m.stateRoot, workspaceIDs)
	if err != nil {
		return RebindResult{}, err
	}
	defer func() { _ = locks.Close() }()
	if err = registry.RebindProject(ctx, project.ID, canonicalNewRoot); err != nil {
		return RebindResult{}, err
	}
	project, err = registry.ResolveProjectByRoot(ctx, canonicalNewRoot)
	if err != nil {
		return RebindResult{}, err
	}
	workspaces, err = registry.ListWorkspaces(ctx, project.ID)
	if err != nil {
		return RebindResult{}, err
	}
	if workspaces == nil {
		workspaces = []Workspace{}
	}
	return RebindResult{Project: project, Workspaces: workspaces}, nil
}

func rejectUnmigratedLegacyTeams(subjectRoot string, activeTeams map[string]struct{}) error {
	legacyRoot := filepath.Join(subjectRoot, "workspace")
	info, err := os.Lstat(legacyRoot)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect legacy workspace root %q: %w", legacyRoot, err)
	}
	if !info.IsDir() {
		return nil
	}
	entries, err := os.ReadDir(legacyRoot)
	if err != nil {
		return fmt.Errorf("inspect legacy workspace root %q: %w", legacyRoot, err)
	}
	var missing []string
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		teamName, normalizeErr := NormalizeTeamName(entry.Name())
		if normalizeErr != nil {
			continue
		}
		if _, ok := activeTeams[teamName]; !ok {
			missing = append(missing, teamName)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	slices.Sort(missing)
	return fmt.Errorf("legacy workspace teams %v remain under %q; migrate them before rebind", missing, legacyRoot)
}
