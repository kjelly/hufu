package workspace

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRebindPreservesWorkspaceIdentityAndRequiresFreshSession(t *testing.T) {
	stateRoot, oldRoot, registry, project := rebindFixture(t)
	first, err := registry.CreateWorkspace(t.Context(), project.ID, "default")
	if err != nil {
		t.Fatal(err)
	}
	second, err := registry.CreateWorkspace(t.Context(), project.ID, "dev")
	if err != nil {
		t.Fatal(err)
	}
	if err = registry.Close(); err != nil {
		t.Fatal(err)
	}
	newRoot := filepath.Join(filepath.Dir(oldRoot), "new-project")
	if err = os.Mkdir(newRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	manager, err := NewManager(stateRoot)
	if err != nil {
		t.Fatal(err)
	}
	result, err := manager.Rebind(t.Context(), project.ID, newRoot)
	if err != nil {
		t.Fatal(err)
	}
	if result.Project.SubjectRoot != newRoot || len(result.Workspaces) != 2 {
		t.Fatalf("rebind result = %+v", result)
	}
	want := map[string]Workspace{first.ID: first, second.ID: second}
	for _, workspace := range result.Workspaces {
		before := want[workspace.ID]
		if !workspace.RequiresFreshSession || workspace.ContextScopeID != before.ContextScopeID || workspace.ControlRoot != before.ControlRoot {
			t.Fatalf("rebound workspace = %+v, before %+v", workspace, before)
		}
	}
	registry = openTestRegistry(t, stateRoot)
	defer registry.Close()
	if err = registry.SetWorkspaceRequiresFreshSession(t.Context(), first.ID, false); err != nil {
		t.Fatal(err)
	}
	cleared, err := registry.GetWorkspaceByID(t.Context(), first.ID)
	if err != nil || cleared.RequiresFreshSession {
		t.Fatalf("cleared fresh flag = %+v, %v", cleared, err)
	}
}

func TestRebindFailsWhenWorkspaceBusy(t *testing.T) {
	stateRoot, oldRoot, registry, project := rebindFixture(t)
	workspace, err := registry.CreateWorkspace(t.Context(), project.ID, "default")
	if err != nil {
		t.Fatal(err)
	}
	registry.Close()
	held, err := AcquireWorkspaceLocks(stateRoot, []string{workspace.ID})
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	newRoot := filepath.Join(filepath.Dir(oldRoot), "new-project")
	if err = os.Mkdir(newRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	manager, _ := NewManager(stateRoot)
	if _, err = manager.Rebind(t.Context(), project.ID, newRoot); !errors.Is(err, ErrBusy) {
		t.Fatalf("busy rebind error = %v", err)
	}
	registry = openTestRegistry(t, stateRoot)
	defer registry.Close()
	unchanged, err := registry.ResolveProject(t.Context(), project.ID)
	if err != nil || unchanged.SubjectRoot != oldRoot {
		t.Fatalf("project changed during busy rebind: %+v, %v", unchanged, err)
	}
}

func TestRebindRejectsUnmigratedLegacyTeam(t *testing.T) {
	stateRoot, oldRoot, registry, project := rebindFixture(t)
	registry.Close()
	if err := os.MkdirAll(filepath.Join(oldRoot, "workspace", "legacy"), 0o755); err != nil {
		t.Fatal(err)
	}
	newRoot := filepath.Join(filepath.Dir(oldRoot), "new-project")
	if err := os.Mkdir(newRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	manager, _ := NewManager(stateRoot)
	if _, err := manager.Rebind(t.Context(), project.ID, newRoot); err == nil || !strings.Contains(err.Error(), "migrate") {
		t.Fatalf("legacy rebind error = %v", err)
	}
}

func TestRebindRejectsRootOwnedByAnotherProject(t *testing.T) {
	stateRoot, oldRoot, registry, project := rebindFixture(t)
	otherRoot := filepath.Join(filepath.Dir(oldRoot), "other")
	if err := os.MkdirAll(filepath.Join(otherRoot, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := registry.RegisterProject(t.Context(), otherRoot); err != nil {
		t.Fatal(err)
	}
	registry.Close()
	manager, _ := NewManager(stateRoot)
	if _, err := manager.Rebind(t.Context(), project.ID, otherRoot); err == nil {
		t.Fatal("duplicate-root rebind unexpectedly succeeded")
	}
	registry = openTestRegistry(t, stateRoot)
	defer registry.Close()
	unchanged, err := registry.ResolveProject(t.Context(), project.ID)
	if err != nil || unchanged.SubjectRoot != oldRoot {
		t.Fatalf("project changed during failed rebind: %+v, %v", unchanged, err)
	}
}

func rebindFixture(t *testing.T) (string, string, *SQLiteRegistry, Project) {
	t.Helper()
	root := t.TempDir()
	stateRoot := filepath.Join(root, "state")
	oldRoot := filepath.Join(root, "old-project")
	if err := os.MkdirAll(filepath.Join(oldRoot, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	registry := openTestRegistry(t, stateRoot)
	project, err := registry.RegisterProject(t.Context(), oldRoot)
	if err != nil {
		registry.Close()
		t.Fatal(err)
	}
	return stateRoot, oldRoot, registry, project
}
