package workspace

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestWorkspaceLifecycleRoundTripPreservesIdentityAndDoesNotFollowChildSymlink(t *testing.T) {
	fixture := newLifecycleFixture(t, nil)
	external := filepath.Join(t.TempDir(), "external.txt")
	if err := os.WriteFile(external, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(external, filepath.Join(fixture.workspace.ControlRoot, "external-link")); err != nil {
		t.Fatal(err)
	}
	registry := openTestRegistry(t, fixture.stateRoot)
	if err := registry.SetWorkspaceRequiresFreshSession(t.Context(), fixture.workspace.ID, true); err != nil {
		t.Fatal(err)
	}
	registry.Close()

	deleted, err := fixture.manager.Delete(t.Context(), DeleteRequest{StartDir: fixture.subjectRoot, TeamName: "default", Retention: 720 * time.Hour})
	if err != nil || deleted.Outcome != "complete" || len(deleted.Items) != 1 {
		t.Fatalf("delete = %+v, %v", deleted, err)
	}
	item := deleted.Items[0]
	if pathExists(fixture.workspace.ControlRoot) || !pathExists(item.TrashPath) {
		t.Fatalf("delete paths: control=%t trash=%t", pathExists(fixture.workspace.ControlRoot), pathExists(item.TrashPath))
	}

	restored, err := fixture.manager.Restore(t.Context(), RestoreRequest{TrashID: item.TrashID})
	if err != nil || restored.Outcome != "complete" || !pathExists(fixture.workspace.ControlRoot) {
		t.Fatalf("restore = %+v, %v", restored, err)
	}
	registry = openTestRegistry(t, fixture.stateRoot)
	restoredWorkspace, err := registry.GetWorkspaceByID(t.Context(), fixture.workspace.ID)
	registry.Close()
	if err != nil || restoredWorkspace.ContextScopeID != fixture.workspace.ContextScopeID || !restoredWorkspace.RequiresFreshSession {
		t.Fatalf("restored workspace = %+v, %v", restoredWorkspace, err)
	}

	deleted, err = fixture.manager.Delete(t.Context(), DeleteRequest{StartDir: fixture.subjectRoot, TeamName: "default"})
	if err != nil {
		t.Fatal(err)
	}
	purged, err := fixture.manager.Purge(t.Context(), PurgeRequest{TrashID: deleted.Items[0].TrashID})
	if err != nil || purged.Outcome != "complete" || pathExists(deleted.Items[0].TrashPath) {
		t.Fatalf("purge = %+v, %v", purged, err)
	}
	content, err := os.ReadFile(external)
	if err != nil || string(content) != "keep" {
		t.Fatalf("external symlink target changed: %q, %v", content, err)
	}
}

func TestWorkspaceDeleteRefusesBusyLockAndMarkerMismatch(t *testing.T) {
	t.Run("busy", func(t *testing.T) {
		fixture := newLifecycleFixture(t, nil)
		locks, err := AcquireWorkspaceLocks(fixture.stateRoot, []string{fixture.workspace.ID})
		if err != nil {
			t.Fatal(err)
		}
		defer locks.Close()
		if _, err = fixture.manager.Delete(t.Context(), DeleteRequest{StartDir: fixture.subjectRoot, TeamName: "default"}); !errors.Is(err, ErrBusy) {
			t.Fatalf("delete busy error = %v", err)
		}
		if !pathExists(fixture.workspace.ControlRoot) {
			t.Fatal("busy delete moved the workspace")
		}
	})

	t.Run("marker mismatch", func(t *testing.T) {
		fixture := newLifecycleFixture(t, nil)
		markerPath := filepath.Join(fixture.workspace.ControlRoot, "workspace.json")
		marker := readWorkspaceMarker(t, markerPath)
		marker.Team = "other"
		encoded := []byte(`{"schema_version":1,"workspace_id":"` + marker.WorkspaceID + `","project_id":"` + marker.ProjectID + `","context_scope_id":"` + marker.ContextScopeID + `","team":"other","managed_by":"hufu","created_at":"2026-09-17T00:00:00Z"}`)
		if err := os.WriteFile(markerPath, encoded, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := fixture.manager.Delete(t.Context(), DeleteRequest{StartDir: fixture.subjectRoot, TeamName: "default"}); !errors.Is(err, ErrConflict) {
			t.Fatalf("delete marker error = %v", err)
		}
	})
}

func TestWorkspaceLifecycleRefusesProtectedPathsTopLevelSymlinksAndRestoreConflicts(t *testing.T) {
	fixture := newLifecycleFixture(t, nil)
	registry := openTestRegistry(t, fixture.stateRoot)
	project, err := registry.ResolveProject(t.Context(), fixture.workspace.ProjectID)
	registry.Close()
	if err != nil {
		t.Fatal(err)
	}
	for name, path := range map[string]string{
		"filesystem root": string(filepath.Separator),
		"state root":      fixture.stateRoot,
		"projects root":   filepath.Join(fixture.stateRoot, "projects"),
		"subject root":    fixture.subjectRoot,
	} {
		t.Run(name, func(t *testing.T) {
			if err := fixture.manager.validateManagedControlRoot(project, path, "default"); !errors.Is(err, ErrConflict) {
				t.Fatalf("protected path error = %v", err)
			}
		})
	}

	realRoot := fixture.workspace.ControlRoot + "-real"
	if err := os.Rename(fixture.workspace.ControlRoot, realRoot); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(realRoot, fixture.workspace.ControlRoot); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.manager.Delete(t.Context(), DeleteRequest{StartDir: fixture.subjectRoot, TeamName: "default"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("top-level symlink delete error = %v", err)
	}
	if err := os.Remove(fixture.workspace.ControlRoot); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(realRoot, fixture.workspace.ControlRoot); err != nil {
		t.Fatal(err)
	}
	deleted, err := fixture.manager.Delete(t.Context(), DeleteRequest{StartDir: fixture.subjectRoot, TeamName: "default"})
	if err != nil {
		t.Fatal(err)
	}
	if err = os.MkdirAll(fixture.workspace.ControlRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err = fixture.manager.Restore(t.Context(), RestoreRequest{TrashID: deleted.Items[0].TrashID}); !errors.Is(err, ErrConflict) {
		t.Fatalf("restore conflict error = %v", err)
	}
	if !pathExists(deleted.Items[0].TrashPath) {
		t.Fatal("restore conflict removed trash source")
	}
}

func TestWorkspaceDeleteAllTeamsLocksAndMovesEveryTarget(t *testing.T) {
	fixture := newLifecycleFixture(t, nil)
	registry := openTestRegistry(t, fixture.stateRoot)
	second, err := registry.CreateWorkspace(t.Context(), fixture.workspace.ProjectID, "dev")
	registry.Close()
	if err != nil {
		t.Fatal(err)
	}
	result, err := fixture.manager.Delete(t.Context(), DeleteRequest{StartDir: fixture.subjectRoot, AllTeams: true})
	if err != nil || result.Outcome != "complete" || len(result.Items) != 2 {
		t.Fatalf("delete all = %+v, %v", result, err)
	}
	if result.Items[0].WorkspaceID > result.Items[1].WorkspaceID {
		t.Fatalf("delete all items are not workspace-ID sorted: %+v", result.Items)
	}
	if pathExists(fixture.workspace.ControlRoot) || pathExists(second.ControlRoot) {
		t.Fatal("delete all left an active control root")
	}
}

func TestDoctorRepairsLifecycleCrashWindows(t *testing.T) {
	tests := []struct {
		name  string
		stage LifecycleStage
		run   func(*testing.T, *lifecycleFixture) string
		check func(*testing.T, *lifecycleFixture, string)
	}{
		{
			name: "delete reserved rolls back active", stage: DeleteStageReserved,
			run: func(t *testing.T, fixture *lifecycleFixture) string {
				_, err := fixture.manager.Delete(t.Context(), DeleteRequest{StartDir: fixture.subjectRoot, TeamName: "default"})
				assertLifecycleCrash(t, err)
				return ""
			},
			check: func(t *testing.T, fixture *lifecycleFixture, _ string) {
				registry := openTestRegistry(t, fixture.stateRoot)
				defer registry.Close()
				workspace, err := registry.GetWorkspaceByID(t.Context(), fixture.workspace.ID)
				if err != nil || workspace.State != "active" || !pathExists(workspace.ControlRoot) {
					t.Fatalf("repaired delete = %+v, %v", workspace, err)
				}
			},
		},
		{
			name: "delete renamed completes trash", stage: DeleteStageRenamed,
			run: func(t *testing.T, fixture *lifecycleFixture) string {
				result, err := fixture.manager.Delete(t.Context(), DeleteRequest{StartDir: fixture.subjectRoot, TeamName: "default"})
				assertLifecycleCrash(t, err)
				return result.Items[0].TrashID
			},
			check: func(t *testing.T, fixture *lifecycleFixture, trashID string) {
				registry := openTestRegistry(t, fixture.stateRoot)
				defer registry.Close()
				trash, err := registry.GetTrashWorkspace(t.Context(), trashID)
				if err != nil || trash.State != "trashed" || !pathExists(trash.TrashPath) {
					t.Fatalf("repaired trash = %+v, %v", trash, err)
				}
			},
		},
		{
			name: "restore reserved returns to trash", stage: RestoreStageReserved,
			run: crashRestore,
			check: func(t *testing.T, fixture *lifecycleFixture, trashID string) {
				registry := openTestRegistry(t, fixture.stateRoot)
				defer registry.Close()
				trash, err := registry.GetTrashWorkspace(t.Context(), trashID)
				if err != nil || trash.State != "trashed" || !pathExists(trash.TrashPath) {
					t.Fatalf("rolled back restore = %+v, %v", trash, err)
				}
			},
		},
		{
			name: "restore renamed completes active", stage: RestoreStageRenamed,
			run: crashRestore,
			check: func(t *testing.T, fixture *lifecycleFixture, _ string) {
				registry := openTestRegistry(t, fixture.stateRoot)
				defer registry.Close()
				workspace, err := registry.GetWorkspaceByID(t.Context(), fixture.workspace.ID)
				if err != nil || workspace.State != "active" || !pathExists(workspace.ControlRoot) {
					t.Fatalf("completed restore = %+v, %v", workspace, err)
				}
			},
		},
		{
			name: "purge reserved resumes removal", stage: PurgeStageReserved,
			run:   crashPurge,
			check: assertPurgeRepaired,
		},
		{
			name: "purge removed completes row", stage: PurgeStageRemoved,
			run:   crashPurge,
			check: assertPurgeRepaired,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			stop := errors.New("crash")
			fixture := newLifecycleFixture(t, func(stage LifecycleStage) error {
				if stage == test.stage {
					return stop
				}
				return nil
			})
			identity := test.run(t, fixture)
			result, err := fixture.manager.Doctor(t.Context(), DoctorRequest{StartDir: fixture.subjectRoot, Repair: true})
			if err != nil {
				t.Fatal(err)
			}
			if result.Outcome != "complete" {
				t.Fatalf("doctor result = %+v", result)
			}
			test.check(t, fixture, identity)
		})
	}
}

type lifecycleFixture struct {
	stateRoot   string
	subjectRoot string
	manager     *WorkspaceManager
	workspace   Workspace
}

func newLifecycleFixture(t *testing.T, hook func(LifecycleStage) error) *lifecycleFixture {
	t.Helper()
	root := t.TempDir()
	subjectRoot := filepath.Join(root, "project")
	if err := os.MkdirAll(filepath.Join(subjectRoot, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	identifierBytes := make([]byte, 0, 16*40)
	for value := byte(1); value <= 40; value++ {
		identifierBytes = append(identifierBytes, bytes.Repeat([]byte{value}, 16)...)
	}
	options := []RegistryOption{WithIDGenerator(NewIDGenerator(bytes.NewReader(identifierBytes)))}
	if hook != nil {
		options = append(options, WithLifecycleHook(hook))
	}
	manager, err := NewManager(filepath.Join(root, "state"), options...)
	if err != nil {
		t.Fatal(err)
	}
	resolution, err := manager.Resolve(t.Context(), ResolveRequest{StartDir: subjectRoot, TeamName: "default", Mode: ResolveEnsure})
	if err != nil {
		t.Fatal(err)
	}
	return &lifecycleFixture{
		stateRoot: filepath.Join(root, "state"), subjectRoot: subjectRoot, manager: manager,
		workspace: Workspace{ID: resolution.WorkspaceID, ProjectID: resolution.ProjectID, TeamName: resolution.TeamName, ContextScopeID: resolution.ContextScopeID, ControlRoot: resolution.ControlRoot, State: "active"},
	}
}

func crashRestore(t *testing.T, fixture *lifecycleFixture) string {
	t.Helper()
	deleted, err := fixture.manager.Delete(t.Context(), DeleteRequest{StartDir: fixture.subjectRoot, TeamName: "default"})
	if err != nil {
		t.Fatal(err)
	}
	trashID := deleted.Items[0].TrashID
	_, err = fixture.manager.Restore(t.Context(), RestoreRequest{TrashID: trashID})
	assertLifecycleCrash(t, err)
	return trashID
}

func crashPurge(t *testing.T, fixture *lifecycleFixture) string {
	t.Helper()
	deleted, err := fixture.manager.Delete(t.Context(), DeleteRequest{StartDir: fixture.subjectRoot, TeamName: "default"})
	if err != nil {
		t.Fatal(err)
	}
	trashID := deleted.Items[0].TrashID
	_, err = fixture.manager.Purge(t.Context(), PurgeRequest{TrashID: trashID})
	assertLifecycleCrash(t, err)
	return trashID
}

func assertLifecycleCrash(t *testing.T, err error) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), "crash") {
		t.Fatalf("lifecycle error = %v, want crash", err)
	}
}

func assertPurgeRepaired(t *testing.T, fixture *lifecycleFixture, trashID string) {
	t.Helper()
	registry := openTestRegistry(t, fixture.stateRoot)
	defer registry.Close()
	if _, err := registry.GetTrashWorkspace(t.Context(), trashID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("purged trash row remains: %v", err)
	}
}
