package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/kjelly/hufu/internal/team"
	workspacepkg "github.com/kjelly/hufu/internal/workspace"
)

type commandWorkspaceRequest struct {
	StartDir      string
	TeamName      string
	ExplicitExact string
	ExplicitRoot  string
	TemporaryRoot string
	Mode          workspacepkg.ResolveMode
	NewSession    bool
}

type commandWorkspaceLease struct {
	locks              *workspacepkg.WorkspaceLocks
	stateRoot          string
	workspaceID        string
	requiresFreshClear bool
	// versionLock is the workspace-versioning project lock held for the
	// coordinator lifetime in required mode; it is released before the team
	// lock.
	versionLock io.Closer
}

type commandWorkspaceBinding struct {
	Resolution workspacepkg.Resolution
	Lease      io.Closer
}

func (l *commandWorkspaceLease) Close() error {
	if l == nil {
		return nil
	}
	var err error
	if l.versionLock != nil {
		err = l.versionLock.Close()
		l.versionLock = nil
	}
	if l.locks != nil {
		err = errors.Join(err, l.locks.Close())
		l.locks = nil
	}
	return err
}

func (l *commandWorkspaceLease) completeFreshSession(ctx context.Context) error {
	if l == nil || !l.requiresFreshClear {
		return nil
	}
	registry, err := workspacepkg.OpenReadWrite(l.stateRoot)
	if err != nil {
		return err
	}
	if err = registry.SetWorkspaceRequiresFreshSession(ctx, l.workspaceID, false); err != nil {
		return errors.Join(err, registry.Close())
	}
	l.requiresFreshClear = false
	return registry.Close()
}

func resolveCommandWorkspace(ctx context.Context, request commandWorkspaceRequest) (workspacepkg.Resolution, io.Closer, error) {
	teamName := strings.TrimSpace(request.TeamName)
	if teamName != "" {
		var err error
		teamName, err = workspacepkg.NormalizeTeamName(teamName)
		if err != nil {
			return workspacepkg.Resolution{}, nil, err
		}
	} else if request.Mode != workspacepkg.ResolveExisting && request.ExplicitExact == "" && request.ExplicitRoot == "" && request.TemporaryRoot == "" {
		return workspacepkg.Resolution{}, nil, fmt.Errorf("team name is required when resolving a new workspace")
	}
	resolveRequest := workspacepkg.ResolveRequest{
		StartDir: request.StartDir, TeamName: teamName,
		ExplicitExact: request.ExplicitExact, ExplicitRoot: request.ExplicitRoot,
		TemporaryRoot: request.TemporaryRoot, Mode: request.Mode,
	}
	if resolveRequest.TemporaryRoot != "" || resolveRequest.ExplicitExact != "" || resolveRequest.ExplicitRoot != "" {
		resolution, resolveErr := workspacepkg.ResolveUnmanaged(resolveRequest)
		if resolveErr == nil && request.Mode == workspacepkg.ResolveExisting {
			resolution.ControlRoot, resolveErr = workspacepkg.CanonicalExistingDirectory(resolution.ControlRoot)
		}
		return resolution, nil, resolveErr
	}
	stateRoot, err := workspacepkg.DefaultStateRoot()
	if err != nil {
		return workspacepkg.Resolution{}, nil, err
	}
	manager, err := workspacepkg.NewManager(stateRoot)
	if err != nil {
		return workspacepkg.Resolution{}, nil, err
	}
	var resolution workspacepkg.Resolution
	if teamName == "" {
		resolution, err = manager.ResolveActive(ctx, request.StartDir)
	} else {
		resolution, err = manager.Resolve(ctx, resolveRequest)
	}
	if err != nil {
		return workspacepkg.Resolution{}, nil, err
	}
	if !resolution.Managed || request.Mode == workspacepkg.ResolvePreview || resolution.WouldCreate {
		return resolution, nil, nil
	}
	locks, err := workspacepkg.AcquireWorkspaceLocks(stateRoot, []string{resolution.WorkspaceID})
	if err != nil {
		return workspacepkg.Resolution{}, nil, err
	}
	lease := &commandWorkspaceLease{locks: locks, stateRoot: stateRoot, workspaceID: resolution.WorkspaceID}
	registry, err := workspacepkg.OpenReadOnly(stateRoot)
	if err != nil {
		return workspacepkg.Resolution{}, nil, errors.Join(err, lease.Close())
	}
	workspace, readErr := registry.GetWorkspaceByID(ctx, resolution.WorkspaceID)
	closeErr := registry.Close()
	if readErr != nil || closeErr != nil {
		return workspacepkg.Resolution{}, nil, errors.Join(readErr, closeErr, lease.Close())
	}
	if workspace.State != "active" || workspace.ProjectID != resolution.ProjectID || workspace.ControlRoot != resolution.ControlRoot || workspace.ContextScopeID != resolution.ContextScopeID {
		return workspacepkg.Resolution{}, nil, errors.Join(fmt.Errorf("%w: workspace identity changed while acquiring lock", workspacepkg.ErrConflict), lease.Close())
	}
	if workspace.RequiresFreshSession && !request.NewSession {
		return workspacepkg.Resolution{}, nil, errors.Join(fmt.Errorf("workspace %s was rebound and requires --new before execution", workspace.ID), lease.Close())
	}
	lease.requiresFreshClear = workspace.RequiresFreshSession && request.NewSession
	return resolution, lease, nil
}

func applyWorkspaceResolution(session *team.TeamSession, resolution workspacepkg.Resolution, lease io.Closer) error {
	if resolution.WouldCreate {
		stateRoot, err := workspacepkg.DefaultStateRoot()
		if err != nil {
			return err
		}
		// Preview scopes deliberately carry no fabricated project/workspace IDs.
		// StateRoot is only a non-overlapping, non-created control placeholder;
		// dry-run exits before any workspace reads or writes.
		return session.SetWorkspacePreviewScope(team.WorkspaceScope{
			ContextScopeID: resolution.SubjectRoot, ControlRoot: stateRoot,
			SubjectRoot: resolution.SubjectRoot, ProjectRoot: resolution.SubjectRoot, Managed: true,
		})
	}
	scope := team.WorkspaceScope{
		ProjectID: resolution.ProjectID, ContextScopeID: resolution.ContextScopeID,
		ControlRoot: resolution.ControlRoot, SubjectRoot: resolution.SubjectRoot,
		ProjectRoot: resolution.SubjectRoot, Managed: resolution.Managed,
	}
	if err := session.SetWorkspaceScope(scope); err != nil {
		if lease != nil {
			_ = lease.Close()
		}
		return err
	}
	session.WorkspaceLease = lease
	return nil
}

func completeManagedFreshSession(ctx context.Context, session *team.TeamSession) error {
	if session == nil || session.WorkspaceLease == nil {
		return nil
	}
	completer, ok := session.WorkspaceLease.(*commandWorkspaceLease)
	if !ok {
		return nil
	}
	return completer.completeFreshSession(ctx)
}

func closeSessionWorkspaceLease(session *team.TeamSession) error {
	if session == nil || session.WorkspaceLease == nil {
		return nil
	}
	err := session.WorkspaceLease.Close()
	session.WorkspaceLease = nil
	return err
}

func resolveExistingManagedWorkspacePath(ctx context.Context, startDir, teamName string) (string, error) {
	stateRoot, err := workspacepkg.DefaultStateRoot()
	if err != nil {
		return "", err
	}
	manager, err := workspacepkg.NewManager(stateRoot)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(teamName) == "" {
		resolution, resolveErr := manager.ResolveActive(ctx, startDir)
		if resolveErr != nil {
			return "", resolveErr
		}
		return resolution.ControlRoot, nil
	}
	resolution, err := manager.Resolve(ctx, workspacepkg.ResolveRequest{StartDir: startDir, TeamName: teamName, Mode: workspacepkg.ResolveExisting})
	if err != nil {
		return "", err
	}
	return resolution.ControlRoot, nil
}

func resolveWorkspaceForTeam(ctx context.Context, teamName string) string {
	workspace, err := resolveExistingManagedWorkspacePath(ctx, runtimeStartDir(), teamName)
	if err != nil {
		return ""
	}
	return workspace
}

func requireResolvedWorkspace(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", fmt.Errorf("managed workspace not found; run a team first or pass --workspace")
	}
	return path, nil
}

func runtimeStartDir() string {
	if strings.TrimSpace(opts.startDir) != "" {
		return opts.startDir
	}
	return currentWorkingDir()
}

func runtimeSubjectRoot() string {
	if strings.TrimSpace(opts.subjectRoot) != "" {
		return opts.subjectRoot
	}
	subjectRoot, err := workspacepkg.DiscoverSubjectRoot(currentWorkingDir())
	if err != nil {
		return currentWorkingDir()
	}
	return subjectRoot
}
