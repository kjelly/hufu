package workspace

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func TestRegistryOpenModesAndMigrationChecksum(t *testing.T) {
	stateRoot := filepath.Join(t.TempDir(), "state")
	if _, err := OpenReadOnly(stateRoot); !errors.Is(err, ErrNotFound) {
		t.Fatalf("OpenReadOnly missing error = %v, want ErrNotFound", err)
	}
	if _, err := os.Stat(stateRoot); !os.IsNotExist(err) {
		t.Fatalf("read-only open created state root: %v", err)
	}

	registry := openTestRegistry(t, stateRoot)
	if err := registry.Close(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(stateRoot, registryFilename))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("registry mode = %o, want 600", info.Mode().Perm())
	}

	readOnly, err := OpenReadOnly(stateRoot)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = readOnly.db.Exec("CREATE TABLE forbidden(value TEXT)"); err == nil {
		t.Fatal("read-only registry accepted mutation")
	}
	if err = readOnly.Close(); err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{"-journal", "-wal", "-shm"} {
		if _, statErr := os.Stat(filepath.Join(stateRoot, registryFilename) + suffix); !os.IsNotExist(statErr) {
			t.Fatalf("read-only open created %s sidecar: %v", suffix, statErr)
		}
	}

	db, err := sql.Open("sqlite", filepath.Join(stateRoot, registryFilename))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec("UPDATE schema_migrations SET checksum='tampered' WHERE version=1"); err != nil {
		t.Fatal(err)
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err = OpenReadWrite(stateRoot); err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("tampered migration error = %v", err)
	}
}

func TestRegistryReadWriteDSNPreservesSpecialPathCharacters(t *testing.T) {
	root := t.TempDir()
	stateRoot := filepath.Join(root, "state?quoted#root")
	projectRoot := filepath.Join(root, "project")
	if err := os.MkdirAll(filepath.Join(projectRoot, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	registry := openTestRegistry(t, stateRoot)
	project, err := registry.RegisterProject(t.Context(), projectRoot)
	if err != nil {
		t.Fatal(err)
	}
	if err = registry.Close(); err != nil {
		t.Fatal(err)
	}
	registry, err = OpenReadOnly(stateRoot)
	if err != nil {
		t.Fatal(err)
	}
	defer registry.Close()
	resolved, err := registry.ResolveProject(t.Context(), project.ID)
	if err != nil || resolved.ID != project.ID {
		t.Fatalf("reopened special-path registry project = %+v, %v", resolved, err)
	}
	if _, statErr := os.Stat(filepath.Join(root, "state")); !os.IsNotExist(statErr) {
		t.Fatalf("SQLite created a truncated DSN target: %v", statErr)
	}
}

func TestRegistryRejectsDatabaseSymlink(t *testing.T) {
	root := t.TempDir()
	stateRoot := filepath.Join(root, "state")
	if err := os.Mkdir(stateRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "target.sqlite")
	if err := os.WriteFile(target, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(stateRoot, registryFilename)); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenReadWrite(stateRoot); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("read-write symlink error = %v", err)
	}
	if _, err := OpenReadOnly(stateRoot); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("read-only symlink error = %v", err)
	}
}

func TestRegistryConcurrentInitialOpen(t *testing.T) {
	stateRoot := filepath.Join(t.TempDir(), "state")
	const count = 8
	start := make(chan struct{})
	results := make(chan error, count)
	var waitGroup sync.WaitGroup
	for range count {
		waitGroup.Go(func() {
			<-start
			registry, err := OpenReadWrite(stateRoot)
			if err == nil {
				err = registry.Close()
			}
			results <- err
		})
	}
	close(start)
	waitGroup.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Error(err)
		}
	}
	registry, err := OpenReadOnly(stateRoot)
	if err != nil {
		t.Fatal(err)
	}
	defer registry.Close()
}

func TestProjectRegistrationSelectorsAndAliases(t *testing.T) {
	stateRoot := filepath.Join(t.TempDir(), "state")
	root := t.TempDir()
	firstRoot := filepath.Join(root, "one", "same")
	secondRoot := filepath.Join(root, "two", "same")
	for _, path := range []string{firstRoot, secondRoot} {
		if err := os.MkdirAll(filepath.Join(path, ".git"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	firstID := append(bytes.Repeat([]byte{0}, 4), bytes.Repeat([]byte{1}, 12)...)
	secondID := append(bytes.Repeat([]byte{0}, 4), bytes.Repeat([]byte{2}, 12)...)
	registry := openTestRegistry(t, stateRoot, WithIDGenerator(NewIDGenerator(bytes.NewReader(append(firstID, secondID...)))))
	defer registry.Close()

	first, err := registry.RegisterProject(t.Context(), firstRoot)
	if err != nil {
		t.Fatal(err)
	}
	again, err := registry.RegisterProject(t.Context(), firstRoot)
	if err != nil {
		t.Fatal(err)
	}
	if again.ID != first.ID {
		t.Fatalf("idempotent registration ID = %q, want %q", again.ID, first.ID)
	}
	second, err := registry.RegisterProject(t.Context(), secondRoot)
	if err != nil {
		t.Fatal(err)
	}
	if first.Slug != "same" || second.Slug != "same" {
		t.Fatalf("slugs = %q, %q", first.Slug, second.Slug)
	}
	if _, err = registry.ResolveProject(t.Context(), "00000000"); !errors.Is(err, ErrAmbiguous) {
		t.Fatalf("ambiguous prefix error = %v", err)
	}
	if _, err = registry.ResolveProject(t.Context(), "same"); !errors.Is(err, ErrAmbiguous) {
		t.Fatalf("ambiguous slug error = %v", err)
	}
	if err = registry.SetAlias(t.Context(), first.ID, " My.Project "); err != nil {
		t.Fatal(err)
	}
	resolved, err := registry.ResolveProject(t.Context(), "my.project")
	if err != nil {
		t.Fatal(err)
	}
	if resolved.ID != first.ID {
		t.Fatalf("alias resolved %q, want %q", resolved.ID, first.ID)
	}
	if err = registry.SetAlias(t.Context(), second.ID, "my.project"); err == nil {
		t.Fatal("duplicate alias unexpectedly succeeded")
	}
	if err = registry.ClearAlias(t.Context(), first.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = registry.ResolveProject(t.Context(), "my.project"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cleared alias error = %v, want ErrNotFound", err)
	}
}

func TestResolveProjectHexLikeNamesAndIDPrefixes(t *testing.T) {
	t.Run("hex slug without collision", func(t *testing.T) {
		root := t.TempDir()
		projectRoot := filepath.Join(root, "deadbeef")
		if err := os.MkdirAll(filepath.Join(projectRoot, ".git"), 0o755); err != nil {
			t.Fatal(err)
		}
		registry := openTestRegistry(t, filepath.Join(root, "state"), WithIDGenerator(NewIDGenerator(bytes.NewReader(bytes.Repeat([]byte{0x11}, 16)))))
		defer registry.Close()
		project, err := registry.RegisterProject(t.Context(), projectRoot)
		if err != nil {
			t.Fatal(err)
		}
		resolved, err := registry.ResolveProject(t.Context(), "deadbeef")
		if err != nil || resolved.ID != project.ID {
			t.Fatalf("hex slug resolved to %+v, %v; want %s", resolved, err, project.ID)
		}
	})

	t.Run("hex slug colliding with ID prefix", func(t *testing.T) {
		root := t.TempDir()
		idRoot := filepath.Join(root, "ordinary")
		hexRoot := filepath.Join(root, "deadbeef")
		for _, path := range []string{idRoot, hexRoot} {
			if err := os.MkdirAll(filepath.Join(path, ".git"), 0o755); err != nil {
				t.Fatal(err)
			}
		}
		firstID := append([]byte{0xde, 0xad, 0xbe, 0xef}, bytes.Repeat([]byte{0x01}, 12)...)
		secondID := bytes.Repeat([]byte{0x22}, 16)
		registry := openTestRegistry(t, filepath.Join(root, "state"), WithIDGenerator(NewIDGenerator(bytes.NewReader(append(firstID, secondID...)))))
		defer registry.Close()
		if _, err := registry.RegisterProject(t.Context(), idRoot); err != nil {
			t.Fatal(err)
		}
		if _, err := registry.RegisterProject(t.Context(), hexRoot); err != nil {
			t.Fatal(err)
		}
		if _, err := registry.ResolveProject(t.Context(), "deadbeef"); !errors.Is(err, ErrAmbiguous) {
			t.Fatalf("hex slug/ID collision error = %v, want ErrAmbiguous", err)
		}
	})
}

func TestRegisterProjectRetriesStateDirectoryCollision(t *testing.T) {
	stateRoot := filepath.Join(t.TempDir(), "state")
	subjectRoot := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(filepath.Join(subjectRoot, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	firstBytes := bytes.Repeat([]byte{0x11}, 16)
	secondBytes := bytes.Repeat([]byte{0x22}, 16)
	collisionPath := filepath.Join(stateRoot, "projects", "project--"+strings.Repeat("11", 8))
	if err := os.MkdirAll(collisionPath, 0o700); err != nil {
		t.Fatal(err)
	}
	registry := openTestRegistry(t, stateRoot, WithIDGenerator(NewIDGenerator(bytes.NewReader(append(firstBytes, secondBytes...)))))
	defer registry.Close()
	project, err := registry.RegisterProject(t.Context(), subjectRoot)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(project.ID, "prj_"+strings.Repeat("22", 16)) {
		t.Fatalf("project ID = %q, want retry ID", project.ID)
	}
}

func TestRegistryConcurrentReadersAndWriters(t *testing.T) {
	stateRoot := filepath.Join(t.TempDir(), "state")
	initial := openTestRegistry(t, stateRoot)
	initial.Close()

	const count = 12
	registries := make([]*SQLiteRegistry, count)
	for index := range count {
		registry, err := OpenReadWrite(stateRoot)
		if err != nil {
			t.Fatal(err)
		}
		registries[index] = registry
		defer registry.Close()
	}
	start := make(chan struct{})
	errorsChannel := make(chan error, count*2)
	var waitGroup sync.WaitGroup
	for index, registry := range registries {
		waitGroup.Go(func() {
			<-start
			root := filepath.Join(t.TempDir(), fmt.Sprintf("project-%02d", index))
			if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
				errorsChannel <- err
				return
			}
			if _, err := registry.RegisterProject(t.Context(), root); err != nil {
				errorsChannel <- err
				return
			}
			if _, err := registry.ListProjects(t.Context(), ListOptions{}); err != nil {
				errorsChannel <- err
			}
		})
	}
	close(start)
	waitGroup.Wait()
	close(errorsChannel)
	for err := range errorsChannel {
		t.Error(err)
	}
	projects, err := registries[0].ListProjects(t.Context(), ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(projects) != count {
		t.Fatalf("project count = %d, want %d", len(projects), count)
	}
}

func TestCreateWorkspacePublishesMarkersAndOperation(t *testing.T) {
	stateRoot := filepath.Join(t.TempDir(), "state")
	subjectRoot := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(filepath.Join(subjectRoot, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	identifierBytes := append(bytes.Repeat([]byte{1}, 16), bytes.Repeat([]byte{2}, 16)...)
	identifierBytes = append(identifierBytes, bytes.Repeat([]byte{3}, 16)...)
	now := time.Date(2026, 9, 17, 13, 0, 0, 0, time.UTC)
	registry := openTestRegistry(t, stateRoot, WithIDGenerator(NewIDGenerator(bytes.NewReader(identifierBytes))), WithClock(func() time.Time { return now }))
	defer registry.Close()
	project, err := registry.RegisterProject(t.Context(), subjectRoot)
	if err != nil {
		t.Fatal(err)
	}
	created, err := registry.CreateWorkspace(t.Context(), project.ID, " Dev ")
	if err != nil {
		t.Fatal(err)
	}
	if created.State != "active" || created.TeamName != "dev" || created.ContextScopeID != project.SubjectRoot {
		t.Fatalf("created workspace = %+v", created)
	}
	if _, err = os.Stat(filepath.Join(created.ControlRoot, "workspace.json")); err != nil {
		t.Fatal(err)
	}
	marker := readWorkspaceMarker(t, filepath.Join(created.ControlRoot, "workspace.json"))
	if marker.WorkspaceID != created.ID || marker.ProjectID != project.ID || marker.ManagedBy != "hufu" || marker.CreatedAt != now {
		t.Fatalf("workspace marker = %+v", marker)
	}
	operationMarker := readOperationMarker(t, filepath.Join(created.ControlRoot, "operation.json"))
	if operationMarker.WorkspaceID != created.ID || operationMarker.Kind != "create" || operationMarker.FinalControlRoot != created.ControlRoot {
		t.Fatalf("operation marker = %+v", operationMarker)
	}
	operation, err := registry.GetOperation(t.Context(), operationMarker.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	if operation.State != "completed" || operation.FinishedAt == nil {
		t.Fatalf("operation = %+v", operation)
	}
	again, err := registry.CreateWorkspace(t.Context(), project.ID, "dev")
	if err != nil || again.ID != created.ID {
		t.Fatalf("idempotent create = %+v, %v", again, err)
	}
}

func TestCreateWorkspaceCrashFixtures(t *testing.T) {
	stages := []CreateStage{
		CreateStageReserved,
		CreateStageOperationMarked,
		CreateStageWorkspaceMarked,
		CreateStageRenamed,
		CreateStageBeforeActivation,
	}
	for _, stage := range stages {
		t.Run(string(stage), func(t *testing.T) {
			stateRoot := filepath.Join(t.TempDir(), "state")
			subjectRoot := filepath.Join(t.TempDir(), "project")
			if err := os.MkdirAll(filepath.Join(subjectRoot, ".git"), 0o755); err != nil {
				t.Fatal(err)
			}
			stop := errors.New("crash")
			registry := openTestRegistry(t, stateRoot, WithCreateHook(func(got CreateStage) error {
				if got == stage {
					return stop
				}
				return nil
			}))
			defer registry.Close()
			project, err := registry.RegisterProject(t.Context(), subjectRoot)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = registry.CreateWorkspace(t.Context(), project.ID, "dev"); !errors.Is(err, stop) {
				t.Fatalf("create error = %v, want interruption", err)
			}
			workspace, err := registry.GetWorkspace(t.Context(), project.ID, "dev")
			if err != nil {
				t.Fatal(err)
			}
			if workspace.State != "creating" || workspace.OperationID == "" {
				t.Fatalf("crash workspace = %+v", workspace)
			}
			operation, err := registry.GetOperation(t.Context(), workspace.OperationID)
			if err != nil || operation.State != "started" {
				t.Fatalf("crash operation = %+v, %v", operation, err)
			}
			assertCrashPaths(t, stage, workspace)
		})
	}
}

func TestConcurrentCreatingWorkspaceNormalizesReservationConflict(t *testing.T) {
	stateRoot := filepath.Join(t.TempDir(), "state")
	subjectRoot := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(filepath.Join(subjectRoot, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	stop := errors.New("stop after reservation")
	registry := openTestRegistry(t, stateRoot, WithCreateHook(func(stage CreateStage) error {
		if stage == CreateStageReserved {
			return stop
		}
		return nil
	}))
	defer registry.Close()
	project, err := registry.RegisterProject(t.Context(), subjectRoot)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = registry.CreateWorkspace(t.Context(), project.ID, "dev"); !errors.Is(err, stop) {
		t.Fatalf("create interruption = %v", err)
	}
	_, err = registry.resolveConcurrentWorkspaceReservation(t.Context(), project.ID, "dev", errors.New("unique constraint failed"))
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("concurrent reservation error = %v, want ErrConflict", err)
	}
}

func TestCreateWorkspaceRejectsSubjectStateOverlapBeforeReservation(t *testing.T) {
	stateRoot := filepath.Join(t.TempDir(), "state")
	subjectRoot := filepath.Join(stateRoot, "source")
	if err := os.MkdirAll(filepath.Join(subjectRoot, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	registry := openTestRegistry(t, stateRoot)
	defer registry.Close()
	project, err := registry.RegisterProject(t.Context(), subjectRoot)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = registry.CreateWorkspace(t.Context(), project.ID, "dev"); !errors.Is(err, ErrConflict) {
		t.Fatalf("overlap error = %v, want ErrConflict", err)
	}
	if _, err = registry.GetWorkspace(t.Context(), project.ID, "dev"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("overlap reserved workspace: %v", err)
	}
}

func assertCrashPaths(t *testing.T, stage CreateStage, workspace Workspace) {
	t.Helper()
	pendingExists := pathExists(workspace.PendingPath)
	finalExists := pathExists(workspace.ControlRoot)
	switch stage {
	case CreateStageReserved:
		if pendingExists || finalExists {
			t.Fatalf("reserved crash paths: pending=%t final=%t", pendingExists, finalExists)
		}
	case CreateStageOperationMarked, CreateStageWorkspaceMarked:
		if !pendingExists || finalExists {
			t.Fatalf("staging crash paths: pending=%t final=%t", pendingExists, finalExists)
		}
		if !pathExists(filepath.Join(workspace.PendingPath, "operation.json")) {
			t.Fatal("operation marker is missing")
		}
		if stage == CreateStageWorkspaceMarked && !pathExists(filepath.Join(workspace.PendingPath, "workspace.json")) {
			t.Fatal("workspace marker is missing")
		}
	case CreateStageRenamed, CreateStageBeforeActivation:
		if pendingExists || !finalExists {
			t.Fatalf("published crash paths: pending=%t final=%t", pendingExists, finalExists)
		}
		if !pathExists(filepath.Join(workspace.ControlRoot, "workspace.json")) {
			t.Fatal("final workspace marker is missing")
		}
	}
}

func openTestRegistry(t *testing.T, stateRoot string, options ...RegistryOption) *SQLiteRegistry {
	t.Helper()
	registry, err := OpenReadWrite(stateRoot, options...)
	if err != nil {
		t.Fatal(err)
	}
	return registry
}

func readWorkspaceMarker(t *testing.T, path string) WorkspaceMarker {
	t.Helper()
	var marker WorkspaceMarker
	readJSONFile(t, path, &marker)
	return marker
}

func readOperationMarker(t *testing.T, path string) OperationMarker {
	t.Helper()
	var marker OperationMarker
	readJSONFile(t, path, &marker)
	return marker
}

func readJSONFile(t *testing.T, path string, target any) {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(content, target); err != nil {
		t.Fatal(err)
	}
}

func pathExists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

func TestRegistryContextCancellation(t *testing.T) {
	registry := openTestRegistry(t, filepath.Join(t.TempDir(), "state"))
	defer registry.Close()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := registry.ListProjects(ctx, ListOptions{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled list error = %v", err)
	}
}
