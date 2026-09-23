package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/kjelly/hufu/internal/config"
	"github.com/kjelly/hufu/internal/team"
	workspacepkg "github.com/kjelly/hufu/internal/workspace"
	"github.com/kjelly/hufu/internal/workspace/versionstore"
)

// configuredVersionMode parses the configured mode; a non-linux platform
// always yields off (D8), with a one-line warning when a mode was requested.
func configuredVersionMode(cfg *config.Config) (versionstore.Mode, error) {
	mode, err := versionstore.ParseMode(cfg.WorkspaceVersioning.Mode)
	if err != nil {
		return "", fmt.Errorf("workspace-versioning.mode: %w", err)
	}
	if mode != versionstore.ModeOff && !versionstore.PlatformSupported() {
		stderrLog("warning: workspace versioning is only supported on linux; treating mode %q as off\n", mode)
		return versionstore.ModeOff, nil
	}
	return mode, nil
}

// buildWorkspaceVersionContext resolves the versioning binding of one managed
// workspace. The returned context is inactive (Mode off) when the configured
// mode is off; the recorded mode floor is enforced either way.
func buildWorkspaceVersionContext(ctx context.Context, registry workspacepkg.Registry, managed workspacepkg.Workspace, cfg *config.Config) (team.WorkspaceVersionContext, error) {
	mode, err := configuredVersionMode(cfg)
	if err != nil {
		return team.WorkspaceVersionContext{}, err
	}
	project, err := registry.ResolveProject(ctx, managed.ProjectID)
	if err != nil {
		return team.WorkspaceVersionContext{}, fmt.Errorf("resolve project of workspace %s: %w", managed.ID, err)
	}
	subject, err := versionstore.CanonicalRoot(project.SubjectRoot)
	if err != nil {
		return team.WorkspaceVersionContext{}, fmt.Errorf("%w: subject root: %v", versionstore.ErrVersionedWorkspaceUnresolved, err)
	}
	control, err := filepath.EvalSymlinks(managed.ControlRoot)
	if err != nil {
		return team.WorkspaceVersionContext{}, fmt.Errorf("%w: control root: %v", versionstore.ErrVersionedWorkspaceUnresolved, err)
	}
	version := team.WorkspaceVersionContext{
		Mode: mode, WorkspaceID: managed.ID, ProjectID: managed.ProjectID, TeamName: managed.TeamName,
		SubjectRoot: subject, ControlRoot: control, StateDir: project.StateDir,
		Limits: versionstore.CaptureLimits{
			MaxFiles:        cfg.WorkspaceVersioning.Capture.MaxFiles,
			MaxFileBytes:    cfg.WorkspaceVersioning.Capture.MaxFileBytes,
			MaxLogicalBytes: cfg.WorkspaceVersioning.Capture.MaxLogicalBytes,
		}.WithDefaults(),
	}
	if err = enforceModeFloor(ctx, version); err != nil {
		return team.WorkspaceVersionContext{}, err
	}
	return version, nil
}

// enforceModeFloor refuses a configured mode below the subject root's
// recorded floor (G13). With mode off only an existing store is consulted,
// so an unversioned project keeps its old behavior exactly.
func enforceModeFloor(ctx context.Context, version team.WorkspaceVersionContext) error {
	if version.Active() {
		store, err := version.OpenStore(ctx)
		if err != nil {
			return err
		}
		defer func() { _ = store.Close() }()
		if _, err = store.EnsureSubjectState(ctx, version.SubjectRoot, version.Mode); err != nil {
			return fmt.Errorf("workspace versioning: %w (use `hufu workspace version downgrade`)", err)
		}
		return nil
	}
	if !versionstore.PlatformSupported() || version.StateDir == "" {
		return nil
	}
	store, err := versionstore.Open(ctx, versionstore.Options{StateDir: version.StateDir, ReadOnly: true})
	if errors.Is(err, versionstore.ErrStoreNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("open workspace version store: %w", err)
	}
	defer func() { _ = store.Close() }()
	state, ok, err := store.GetSubjectState(ctx, version.SubjectRoot)
	if err != nil {
		return err
	}
	if ok && versionstore.ModeOff.Below(state.ModeFloor) {
		return fmt.Errorf("workspace versioning: %w: configured off, floor %s (use `hufu workspace version downgrade`)", versionstore.ErrModeDowngradeRefused, state.ModeFloor)
	}
	return nil
}

// bindRunWorkspaceVersioning attaches the versioning context to a managed
// team session and, in required mode, takes the project lock for the whole
// coordinator lifetime (§27). The lock is released with the team lease.
func bindRunWorkspaceVersioning(ctx context.Context, session *team.TeamSession, cfg *config.Config) error {
	lease, ok := session.WorkspaceLease.(*commandWorkspaceLease)
	if !ok || lease == nil || lease.workspaceID == "" {
		return nil
	}
	registry, err := workspacepkg.OpenReadOnly(lease.stateRoot)
	if err != nil {
		return fmt.Errorf("open workspace registry: %w", err)
	}
	defer func() { _ = registry.Close() }()
	managed, err := registry.GetWorkspaceByID(ctx, lease.workspaceID)
	if err != nil {
		return fmt.Errorf("read workspace %s: %w", lease.workspaceID, err)
	}
	version, err := buildWorkspaceVersionContext(ctx, registry, managed, cfg)
	if err != nil {
		return err
	}
	if version.Required() {
		lock, lockErr := versionstore.TryLockProject(version.StateDir, versionstore.LockOwner{WorkspaceID: version.WorkspaceID, Operation: "run"})
		if lockErr != nil {
			return fmt.Errorf("workspace versioning (required mode allows one coordinator per project): %w", lockErr)
		}
		lease.versionLock = lock
		version.HoldsProjectLock = true
		if err = recoverRunWorkspace(ctx, session.Workspace, version); err != nil {
			lease.versionLock = nil
			return errors.Join(err, lock.Close())
		}
	}
	session.WorkspaceVersion = version
	return nil
}

// recoverRunWorkspace finishes interrupted workspace operations before the
// coordinator loads the session tree, so a forward-completed checkout can
// never leave the coordinator on a stale active branch (§19.2).
func recoverRunWorkspace(ctx context.Context, control string, version team.WorkspaceVersionContext) error {
	st, es, err := openSessionStores(control)
	if err != nil {
		return err
	}
	defer func() { _ = es.Close() }()
	store, err := version.OpenStore(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()
	ops := &team.WorkspaceSessionOps{Version: version, Workspace: control, Tree: st, Events: es, Store: store}
	report, err := team.RecoverWorkspaceOperations(ctx, ops)
	if err != nil {
		return fmt.Errorf("workspace recovery: %w", err)
	}
	if !report.Empty() {
		stderrLog("workspace recovery: %d snapshot(s) settled, %d operation(s) completed, %d failed\n",
			len(report.Snapshots), len(report.Completed), len(report.Failed))
	}
	return nil
}

// versionedSession is the resolved binding of a session or workspace
// version command: the control workspace, its versioning context, and the
// locks it holds.
type versionedSession struct {
	workspace string
	version   team.WorkspaceVersionContext
	closers   []io.Closer
}

func (v *versionedSession) Close() error {
	var err error
	for index := len(v.closers) - 1; index >= 0; index-- {
		err = errors.Join(err, v.closers[index].Close())
	}
	v.closers = nil
	return err
}

// openVersionedSession resolves the control workspace like every session
// command and, when it is a managed workspace with versioning enabled, its
// versioning context. mutating commands take the team lock and then the
// project lock (both non-blocking) whenever versioning is active.
func openVersionedSession(ctx context.Context, mutating bool, operation string) (*versionedSession, error) {
	workspace, err := requireSessionWorkspace()
	if err != nil {
		return nil, err
	}
	session := &versionedSession{workspace: workspace}
	cfg := config.LoadConfig()
	managed, registry, stateRoot, err := lookupManagedWorkspace(ctx, workspace)
	if err != nil || registry == nil {
		return session, err
	}
	defer func() { _ = registry.Close() }()
	version, err := buildWorkspaceVersionContext(ctx, registry, managed, cfg)
	if err != nil {
		return nil, err
	}
	session.version = version
	if !mutating || !version.Active() {
		return session, nil
	}
	locks, err := workspacepkg.AcquireWorkspaceLocks(stateRoot, []string{managed.ID})
	if err != nil {
		if errors.Is(err, workspacepkg.ErrBusy) {
			return nil, fmt.Errorf("%w: a run of team %s is active", versionstore.ErrVersionOperationInProgress, managed.TeamName)
		}
		return nil, err
	}
	session.closers = append(session.closers, locks)
	{
		// A required-mode coordinator holds this lock for its whole
		// lifetime, so taking it here also proves no run is active on the
		// subject root; observe-mode runs only take it briefly.
		lock, lockErr := versionstore.TryLockProject(version.StateDir, versionstore.LockOwner{WorkspaceID: managed.ID, Operation: operation})
		if lockErr != nil {
			return nil, errors.Join(lockErr, session.Close())
		}
		session.closers = append(session.closers, lock)
		session.version.HoldsProjectLock = true
	}
	return session, nil
}

// lookupManagedWorkspace maps a control root to its managed workspace. A
// missing registry or an unregistered path is an unmanaged workspace
// (registry == nil, no error).
func lookupManagedWorkspace(ctx context.Context, controlRoot string) (workspacepkg.Workspace, workspacepkg.Registry, string, error) {
	stateRoot, err := workspacepkg.DefaultStateRoot()
	if err != nil {
		return workspacepkg.Workspace{}, nil, "", fmt.Errorf("resolve hufu state root: %w", err)
	}
	if _, statErr := os.Stat(stateRoot); errors.Is(statErr, os.ErrNotExist) {
		return workspacepkg.Workspace{}, nil, "", nil
	}
	registry, err := workspacepkg.OpenReadOnly(stateRoot)
	if errors.Is(err, workspacepkg.ErrNotFound) {
		return workspacepkg.Workspace{}, nil, "", nil
	}
	if err != nil {
		// An unreadable registry must not silently downgrade a versioned
		// workspace to unmanaged behavior.
		return workspacepkg.Workspace{}, nil, "", fmt.Errorf("open workspace registry: %w", err)
	}
	managed, err := registry.GetWorkspaceByControlRoot(ctx, controlRoot)
	if err != nil {
		_ = registry.Close()
		if errors.Is(err, workspacepkg.ErrNotFound) {
			return workspacepkg.Workspace{}, nil, "", nil
		}
		return workspacepkg.Workspace{}, nil, "", fmt.Errorf("look up managed workspace: %w", err)
	}
	return managed, registry, stateRoot, nil
}
