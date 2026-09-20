package workspace

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

type WorkspaceManager struct {
	stateRoot       string
	registryOptions []RegistryOption
}

func NewManager(stateRoot string, options ...RegistryOption) (*WorkspaceManager, error) {
	root, err := canonicalPath(stateRoot)
	if err != nil {
		return nil, fmt.Errorf("workspace manager state root: %w", err)
	}
	return &WorkspaceManager{stateRoot: root, registryOptions: options}, nil
}

func (m *WorkspaceManager) Close() error { return nil }

func (m *WorkspaceManager) Resolve(ctx context.Context, request ResolveRequest) (Resolution, error) {
	if request.TemporaryRoot != "" || request.ExplicitExact != "" || request.ExplicitRoot != "" {
		return ResolveUnmanaged(request)
	}
	teamName, err := NormalizeTeamName(request.TeamName)
	if err != nil {
		return Resolution{}, err
	}
	subjectRoot, err := DiscoverSubjectRoot(request.StartDir)
	if err != nil {
		return Resolution{}, err
	}
	switch request.Mode {
	case ResolvePreview:
		return m.resolvePreview(ctx, request.StartDir, subjectRoot, teamName)
	case ResolveExisting:
		return m.resolveExistingWithLegacyCheck(ctx, request.StartDir, subjectRoot, teamName)
	case ResolveEnsure:
		return m.resolveEnsure(ctx, request.StartDir, subjectRoot, teamName)
	default:
		return Resolution{}, fmt.Errorf("unsupported workspace resolve mode %q", request.Mode)
	}
}

// ResolveActive finds the active workspace for the project containing startDir.
// A default team is preferred for compatibility; otherwise the project must
// have exactly one active team workspace so callers do not silently inspect
// the wrong team's state.
func (m *WorkspaceManager) ResolveActive(ctx context.Context, startDir string) (Resolution, error) {
	subjectRoot, err := DiscoverSubjectRoot(startDir)
	if err != nil {
		return Resolution{}, err
	}
	registry, err := OpenReadOnly(m.stateRoot)
	if err != nil {
		return Resolution{}, err
	}
	defer func() { _ = registry.Close() }()

	project, err := registry.ResolveProjectByRoot(ctx, subjectRoot)
	if err != nil {
		return Resolution{}, err
	}
	workspaces, err := registry.ListWorkspaces(ctx, project.ID)
	if err != nil {
		return Resolution{}, err
	}

	active := make([]Workspace, 0, len(workspaces))
	for _, workspace := range workspaces {
		if workspace.State == "active" {
			active = append(active, workspace)
		}
	}
	if len(active) == 0 {
		return Resolution{}, fmt.Errorf("%w: project %s has no active workspaces", ErrNotFound, project.ID)
	}
	for _, workspace := range active {
		if workspace.TeamName == "default" {
			return managedResolution(project, workspace)
		}
	}
	if len(active) != 1 {
		return Resolution{}, fmt.Errorf("%w: project %s has multiple active team workspaces; specify --agent-team or --workspace", ErrAmbiguous, project.ID)
	}
	return managedResolution(project, active[0])
}

func ResolveUnmanaged(request ResolveRequest) (Resolution, error) {
	teamName, err := NormalizeTeamName(request.TeamName)
	if err != nil {
		return Resolution{}, err
	}
	subjectRoot, err := DiscoverSubjectRoot(request.StartDir)
	if err != nil {
		return Resolution{}, err
	}
	if request.TemporaryRoot != "" {
		return unmanagedResolution(request.TemporaryRoot, subjectRoot, teamName)
	}
	if request.ExplicitExact != "" {
		return unmanagedResolution(request.ExplicitExact, subjectRoot, teamName)
	}
	if request.ExplicitRoot != "" {
		controlRoot, resolveErr := canonicalPath(filepath.Join(request.ExplicitRoot, teamName))
		if resolveErr != nil {
			return Resolution{}, fmt.Errorf("resolve explicit workspace root: %w", resolveErr)
		}
		return Resolution{ContextScopeID: subjectRoot, TeamName: teamName, SubjectRoot: subjectRoot, ControlRoot: controlRoot}, nil
	}
	return Resolution{}, fmt.Errorf("unmanaged workspace request has no explicit or temporary root")
}

func unmanagedResolution(controlPath, subjectRoot, teamName string) (Resolution, error) {
	controlRoot, err := canonicalPath(controlPath)
	if err != nil {
		return Resolution{}, fmt.Errorf("resolve explicit workspace: %w", err)
	}
	return Resolution{ContextScopeID: subjectRoot, TeamName: teamName, SubjectRoot: subjectRoot, ControlRoot: controlRoot}, nil
}

func (m *WorkspaceManager) resolvePreview(ctx context.Context, startDir, subjectRoot, teamName string) (Resolution, error) {
	registry, err := OpenReadOnly(m.stateRoot)
	if errors.Is(err, ErrNotFound) {
		if legacyErr := rejectLegacyWorkspace(startDir, subjectRoot, teamName); legacyErr != nil {
			return Resolution{}, legacyErr
		}
		return Resolution{TeamName: teamName, SubjectRoot: subjectRoot, WouldCreate: true}, nil
	}
	if err != nil {
		return Resolution{}, err
	}
	defer func() { _ = registry.Close() }()
	project, err := registry.ResolveProjectByRoot(ctx, subjectRoot)
	if errors.Is(err, ErrNotFound) {
		if legacyErr := rejectLegacyWorkspace(startDir, subjectRoot, teamName); legacyErr != nil {
			return Resolution{}, legacyErr
		}
		return Resolution{TeamName: teamName, SubjectRoot: subjectRoot, WouldCreate: true}, nil
	}
	if err != nil {
		return Resolution{}, err
	}
	workspace, err := registry.GetWorkspace(ctx, project.ID, teamName)
	if errors.Is(err, ErrNotFound) {
		if legacyErr := rejectLegacyWorkspace(startDir, subjectRoot, teamName); legacyErr != nil {
			return Resolution{}, legacyErr
		}
		return Resolution{
			ProjectID: project.ID, ContextScopeID: subjectRoot, TeamName: teamName,
			SubjectRoot: subjectRoot, ControlRoot: filepath.Join(project.StateDir, "teams", teamName),
			Managed: true, WouldCreate: true,
		}, nil
	}
	if err != nil {
		return Resolution{}, err
	}
	return managedResolution(project, workspace)
}

func (m *WorkspaceManager) resolveExisting(ctx context.Context, subjectRoot, teamName string) (Resolution, error) {
	registry, err := OpenReadOnly(m.stateRoot)
	if err != nil {
		return Resolution{}, err
	}
	defer func() { _ = registry.Close() }()
	project, err := registry.ResolveProjectByRoot(ctx, subjectRoot)
	if err != nil {
		return Resolution{}, err
	}
	workspace, err := registry.GetWorkspace(ctx, project.ID, teamName)
	if err != nil {
		return Resolution{}, err
	}
	return managedResolution(project, workspace)
}

func (m *WorkspaceManager) resolveExistingWithLegacyCheck(ctx context.Context, startDir, subjectRoot, teamName string) (Resolution, error) {
	resolution, err := m.resolveExisting(ctx, subjectRoot, teamName)
	if err == nil || !errors.Is(err, ErrNotFound) {
		return resolution, err
	}
	if legacyErr := rejectLegacyWorkspace(startDir, subjectRoot, teamName); legacyErr != nil {
		return Resolution{}, legacyErr
	}
	return Resolution{}, err
}

func (m *WorkspaceManager) resolveEnsure(ctx context.Context, startDir, subjectRoot, teamName string) (Resolution, error) {
	resolution, err := m.resolveExisting(ctx, subjectRoot, teamName)
	if err == nil || !errors.Is(err, ErrNotFound) {
		return resolution, err
	}
	if legacyErr := rejectLegacyWorkspace(startDir, subjectRoot, teamName); legacyErr != nil {
		return Resolution{}, legacyErr
	}
	registry, err := OpenReadWrite(m.stateRoot, m.registryOptions...)
	if err != nil {
		return Resolution{}, err
	}
	defer func() { _ = registry.Close() }()
	project, err := registry.RegisterProject(ctx, subjectRoot)
	if err != nil {
		return Resolution{}, err
	}
	workspace, err := registry.CreateWorkspace(ctx, project.ID, teamName)
	if err != nil {
		return Resolution{}, err
	}
	return managedResolution(project, workspace)
}

func managedResolution(project Project, workspace Workspace) (Resolution, error) {
	if workspace.State != "active" {
		return Resolution{}, fmt.Errorf("%w: workspace %s/%s is %s", ErrConflict, project.ID, workspace.TeamName, workspace.State)
	}
	return Resolution{
		ProjectID: project.ID, WorkspaceID: workspace.ID,
		ContextScopeID: workspace.ContextScopeID, TeamName: workspace.TeamName,
		SubjectRoot: project.SubjectRoot, ControlRoot: workspace.ControlRoot, Managed: true,
	}, nil
}

func CanonicalExistingDirectory(path string) (string, error) {
	canonical, err := canonicalPath(path)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(canonical)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("%q is not a directory", canonical)
	}
	return canonical, nil
}
