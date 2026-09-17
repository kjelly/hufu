package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kjelly/hufu/internal/team"
	workspacepkg "github.com/kjelly/hufu/internal/workspace"
)

func TestResolveCommandWorkspaceManagedLockLifecycle(t *testing.T) {
	root := t.TempDir()
	stateRoot := filepath.Join(root, "state")
	t.Setenv("HUFU_STATE_HOME", stateRoot)
	subjectRoot := filepath.Join(root, "project")
	if err := os.MkdirAll(filepath.Join(subjectRoot, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	request := commandWorkspaceRequest{StartDir: subjectRoot, TeamName: "dev", Mode: workspacepkg.ResolveEnsure}
	resolution, lease, err := resolveCommandWorkspace(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	if !resolution.Managed || lease == nil {
		t.Fatalf("managed resolution = %+v, lease=%v", resolution, lease)
	}
	if _, secondLease, secondErr := resolveCommandWorkspace(t.Context(), commandWorkspaceRequest{StartDir: subjectRoot, TeamName: "dev", Mode: workspacepkg.ResolveExisting}); !errors.Is(secondErr, workspacepkg.ErrBusy) || secondLease != nil {
		t.Fatalf("second resolution = lease %v, error %v", secondLease, secondErr)
	}
	session := &team.TeamSession{}
	if err = applyWorkspaceResolution(session, resolution, lease); err != nil {
		t.Fatal(err)
	}
	if session.Workspace != resolution.ControlRoot || session.Scope.SubjectRoot != subjectRoot || !session.Scope.Managed {
		t.Fatalf("session scope = %+v", session.Scope)
	}
	if err = closeSessionWorkspaceLease(session); err != nil {
		t.Fatal(err)
	}
	_, reacquired, err := resolveCommandWorkspace(t.Context(), commandWorkspaceRequest{StartDir: subjectRoot, TeamName: "dev", Mode: workspacepkg.ResolveExisting})
	if err != nil {
		t.Fatal(err)
	}
	if err = reacquired.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestResolveCommandWorkspacePreviewAndUnmanagedDoNotCreateState(t *testing.T) {
	root := t.TempDir()
	stateRoot := filepath.Join(root, "missing-state")
	t.Setenv("HUFU_STATE_HOME", stateRoot)
	subjectRoot := filepath.Join(root, "project")
	if err := os.MkdirAll(filepath.Join(subjectRoot, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	preview, lease, err := resolveCommandWorkspace(t.Context(), commandWorkspaceRequest{StartDir: subjectRoot, TeamName: "dev", Mode: workspacepkg.ResolvePreview})
	if err != nil {
		t.Fatal(err)
	}
	if !preview.WouldCreate || lease != nil {
		t.Fatalf("preview = %+v, lease=%v", preview, lease)
	}
	if _, err = os.Stat(stateRoot); !os.IsNotExist(err) {
		t.Fatalf("preview created state root: %v", err)
	}

	t.Setenv("HUFU_STATE_HOME", "relative-invalid-but-unused")
	exact := filepath.Join(root, "exact")
	unmanaged, lease, err := resolveCommandWorkspace(t.Context(), commandWorkspaceRequest{
		StartDir: subjectRoot, TeamName: "dev", ExplicitExact: exact, Mode: workspacepkg.ResolveEnsure,
	})
	if err != nil {
		t.Fatal(err)
	}
	if unmanaged.Managed || unmanaged.ControlRoot != exact || lease != nil {
		t.Fatalf("unmanaged = %+v, lease=%v", unmanaged, lease)
	}
	legacy, lease, err := resolveCommandWorkspace(t.Context(), commandWorkspaceRequest{
		StartDir: subjectRoot, TeamName: "dev", Mode: workspacepkg.ResolveEnsure, LegacyDefault: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if legacy.ControlRoot != filepath.Join(subjectRoot, "workspace", "dev") || lease != nil {
		t.Fatalf("legacy = %+v, lease=%v", legacy, lease)
	}
}

func TestResolveCommandWorkspaceUnmanagedExistingDoesNotCreateMissingDirectory(t *testing.T) {
	root := t.TempDir()
	subjectRoot := filepath.Join(root, "project")
	if err := os.MkdirAll(filepath.Join(subjectRoot, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(root, "missing")
	_, lease, err := resolveCommandWorkspace(t.Context(), commandWorkspaceRequest{
		StartDir: subjectRoot, TeamName: "dev", ExplicitExact: missing, Mode: workspacepkg.ResolveExisting,
	})
	if !os.IsNotExist(err) || lease != nil {
		t.Fatalf("existing missing resolution = lease %v, error %v", lease, err)
	}
	if _, statErr := os.Stat(missing); !os.IsNotExist(statErr) {
		t.Fatalf("existing resolution created missing path: %v", statErr)
	}
}

func TestResolveCommandWorkspaceRebindFreshSessionGate(t *testing.T) {
	root := t.TempDir()
	stateRoot := filepath.Join(root, "state")
	t.Setenv("HUFU_STATE_HOME", stateRoot)
	oldRoot := filepath.Join(root, "old")
	newRoot := filepath.Join(root, "new")
	for _, path := range []string{oldRoot, newRoot} {
		if err := os.MkdirAll(filepath.Join(path, ".git"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	manager, err := workspacepkg.NewManager(stateRoot)
	if err != nil {
		t.Fatal(err)
	}
	created, err := manager.Resolve(t.Context(), workspacepkg.ResolveRequest{StartDir: oldRoot, TeamName: "dev", Mode: workspacepkg.ResolveEnsure})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = manager.Rebind(t.Context(), created.ProjectID, newRoot); err != nil {
		t.Fatal(err)
	}
	request := commandWorkspaceRequest{StartDir: newRoot, TeamName: "dev", Mode: workspacepkg.ResolveExisting}
	if _, lease, err := resolveCommandWorkspace(t.Context(), request); err == nil || !strings.Contains(err.Error(), "requires --new") || lease != nil {
		t.Fatalf("fresh-session gate = lease %v, error %v", lease, err)
	}
	request.NewSession = true
	resolution, lease, err := resolveCommandWorkspace(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	session := &team.TeamSession{}
	if err = applyWorkspaceResolution(session, resolution, lease); err != nil {
		t.Fatal(err)
	}
	if err = completeManagedFreshSession(t.Context(), session); err != nil {
		t.Fatal(err)
	}
	registry, err := workspacepkg.OpenReadOnly(stateRoot)
	if err != nil {
		t.Fatal(err)
	}
	workspace, err := registry.GetWorkspaceByID(t.Context(), resolution.WorkspaceID)
	if closeErr := registry.Close(); err != nil || closeErr != nil {
		t.Fatal(errors.Join(err, closeErr))
	}
	if workspace.RequiresFreshSession {
		t.Fatal("fresh-session flag was not cleared after checkpoint completion")
	}
	if err = closeSessionWorkspaceLease(session); err != nil {
		t.Fatal(err)
	}
}
