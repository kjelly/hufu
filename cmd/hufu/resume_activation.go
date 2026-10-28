package main

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/kjelly/hufu/internal/team"
	workspacepkg "github.com/kjelly/hufu/internal/workspace"
)

type resumeActivationKey struct{}

type resumeActivationRequest struct {
	source *team.ResumeSource
	plan   *team.ResumeActivationPlan
}

func withResumeActivation(ctx context.Context, source *team.ResumeSource) context.Context {
	return context.WithValue(ctx, resumeActivationKey{}, &resumeActivationRequest{source: source})
}

// activateResumeSession runs only after the shared static startup gate and
// workspace-version binding, before the checkpoint or providers are loaded.
func activateResumeSession(ctx context.Context, session *team.TeamSession) error {
	request, ok := ctx.Value(resumeActivationKey{}).(*resumeActivationRequest)
	if !ok {
		return nil
	}
	if request.plan == nil {
		return team.ActivateVersionedResumeSource(ctx, session.Workspace, request.source, session.WorkspaceVersion)
	}
	return team.ActivateResumePlan(ctx, session.Workspace, request.source, session.WorkspaceVersion, request.plan)
}

// bindManagedResumeWorkspace recognizes explicit registered control paths.
// The general explicit-path resolver deliberately treats them as unmanaged;
// resume must restore their registered scope and retain the team lease.
func bindManagedResumeWorkspace(ctx context.Context, binding *commandWorkspaceBinding, requestedTeam string, readOnly bool) (*commandWorkspaceBinding, string, error) {
	if binding.Resolution.Managed {
		return binding, binding.Resolution.TeamName, nil
	}
	managed, registry, stateRoot, err := lookupManagedWorkspace(ctx, binding.Resolution.ControlRoot)
	if err != nil || registry == nil {
		return binding, "", err
	}
	defer func() { _ = registry.Close() }()
	if requestedTeam != "" && !strings.EqualFold(strings.TrimSpace(requestedTeam), managed.TeamName) {
		return nil, "", fmt.Errorf("resume team %q does not own workspace %s", requestedTeam, managed.ID)
	}
	project, err := registry.ResolveProject(ctx, managed.ProjectID)
	if err != nil {
		return nil, "", err
	}
	var lease *commandWorkspaceLease
	if !readOnly {
		locks, err := workspacepkg.AcquireWorkspaceLocks(stateRoot, []string{managed.ID})
		if err != nil {
			return nil, "", err
		}
		lease = &commandWorkspaceLease{locks: locks, stateRoot: stateRoot, workspaceID: managed.ID}
		current, err := registry.GetWorkspaceByID(ctx, managed.ID)
		if err != nil || current.State != "active" || current.ProjectID != managed.ProjectID || current.ControlRoot != managed.ControlRoot || current.ContextScopeID != managed.ContextScopeID || current.RequiresFreshSession {
			return nil, "", errors.Join(fmt.Errorf("resume workspace identity changed or requires a fresh session"), err, lease.Close())
		}
	}
	resolved := &commandWorkspaceBinding{Resolution: workspacepkg.Resolution{
		ProjectID: managed.ProjectID, WorkspaceID: managed.ID, ContextScopeID: managed.ContextScopeID,
		TeamName: managed.TeamName, SubjectRoot: project.SubjectRoot, ControlRoot: managed.ControlRoot, Managed: true,
	}}
	// Avoid a non-nil interface holding a nil pointer in read-only setup.
	if lease != nil {
		resolved.Lease = lease
	}
	return resolved, managed.TeamName, nil
}
