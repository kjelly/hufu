package workspace

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	contextstore "github.com/kjelly/hufu/internal/context"
	"github.com/kjelly/hufu/internal/eventchain"
)

func TestMigrateLegacyWorkspaceIsNonDestructive(t *testing.T) {
	fixture := newMigrationFixture(t)
	before, err := buildInventory(fixture.source)
	if err != nil {
		t.Fatal(err)
	}
	result, err := fixture.manager.Migrate(t.Context(), MigrateRequest{StartDir: fixture.projectRoot, TeamName: "Default"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Outcome != "complete" || len(result.Completed) != 1 {
		t.Fatalf("result = %+v", result)
	}
	item := result.Completed[0]
	if item.Workspace.State != "active" || item.Workspace.ContextScopeID != "legacy-scope" {
		t.Fatalf("workspace = %+v", item.Workspace)
	}
	if data, readErr := os.ReadFile(filepath.Join(item.Workspace.ControlRoot, "artifact.txt")); readErr != nil || string(data) != "legacy data" {
		t.Fatalf("copied artifact = %q, %v", data, readErr)
	}
	if target, readErr := os.Readlink(filepath.Join(item.Workspace.ControlRoot, "artifact-link")); readErr != nil || target != "artifact.txt" {
		t.Fatalf("copied symlink = %q, %v", target, readErr)
	}
	after, err := buildInventory(fixture.source)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(before, after) {
		t.Fatalf("legacy source changed\nbefore=%+v\nafter=%+v", before, after)
	}
	if !markerMatchesWorkspace(item.Workspace.ControlRoot, item.Workspace) {
		t.Fatal("published workspace marker does not match registry")
	}
}

func TestMigrateRejectsAmbiguousLegacyContextScopeBeforeRegistryWrites(t *testing.T) {
	fixture := newMigrationFixture(t)
	db, err := sql.Open("sqlite", filepath.Join(fixture.source, legacyContextFilename))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`INSERT INTO context_events(event_type,item_id,scope_json,payload_json,created_at) VALUES('test',NULL,'{"project_id":"other-scope"}','{}',2)`); err != nil {
		t.Fatal(err)
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	stateRoot := fixture.manager.stateRoot
	if _, err = fixture.manager.Migrate(t.Context(), MigrateRequest{StartDir: fixture.projectRoot, TeamName: "default"}); err == nil || !bytes.Contains([]byte(err.Error()), []byte("legacy_context_scope_ambiguous")) {
		t.Fatalf("error = %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(stateRoot, registryFilename)); !os.IsNotExist(statErr) {
		t.Fatalf("ambiguous preflight created registry: %v", statErr)
	}
}

func TestDoctorRepairsInterruptedMigrationWindows(t *testing.T) {
	for _, stage := range []MigrationStage{MigrationStageReserved, MigrationStageCopied, MigrationStageVerified, MigrationStageRenamed} {
		t.Run(string(stage), func(t *testing.T) {
			fixture := newMigrationFixture(t)
			interrupted := errors.New("crash")
			manager, err := NewManager(fixture.manager.stateRoot,
				WithIDGenerator(NewIDGenerator(bytes.NewReader(bytes.Repeat([]byte{0x44}, 96)))),
				WithMigrationHook(func(current MigrationStage) error {
					if current == stage {
						return interrupted
					}
					return nil
				}),
			)
			if err != nil {
				t.Fatal(err)
			}
			result, err := manager.Migrate(t.Context(), MigrateRequest{StartDir: fixture.projectRoot, TeamName: "default"})
			if !errors.Is(err, interrupted) || len(result.Failed) != 1 {
				t.Fatalf("interruption result=%+v err=%v", result, err)
			}
			doctor, err := fixture.manager.Doctor(t.Context(), DoctorRequest{StartDir: fixture.projectRoot, Repair: true})
			if err != nil {
				t.Fatal(err)
			}
			var repaired bool
			for _, issue := range doctor.Issues {
				if issue.Code == IssueCreatingIncomplete && issue.Repaired {
					repaired = true
				}
			}
			if !repaired {
				t.Fatalf("doctor did not repair creating state: %+v", doctor)
			}
			registry, err := OpenReadOnly(fixture.manager.stateRoot)
			if err != nil {
				t.Fatal(err)
			}
			project, err := registry.ResolveProjectByRoot(t.Context(), fixture.projectRoot)
			if err != nil {
				t.Fatal(err)
			}
			workspace, lookupErr := registry.GetWorkspace(t.Context(), project.ID, "default")
			_ = registry.Close()
			if stage == MigrationStageReserved || stage == MigrationStageCopied {
				if !errors.Is(lookupErr, ErrNotFound) {
					t.Fatalf("reservation-only workspace remained: %+v, %v", workspace, lookupErr)
				}
			} else if lookupErr != nil || workspace.State != "active" {
				t.Fatalf("repaired workspace = %+v, %v", workspace, lookupErr)
			}
		})
	}
}

func TestMigrationMutationDetectionCleansOwnedStaging(t *testing.T) {
	fixture := newMigrationFixture(t)
	manager, err := NewManager(fixture.manager.stateRoot,
		WithIDGenerator(NewIDGenerator(bytes.NewReader(bytes.Repeat([]byte{0x55}, 96)))),
		WithMigrationHook(func(stage MigrationStage) error {
			if stage == MigrationStageCopied {
				return os.WriteFile(filepath.Join(fixture.source, "artifact.txt"), []byte("changed"), 0o600)
			}
			return nil
		}),
	)
	if err != nil {
		t.Fatal(err)
	}
	result, err := manager.Migrate(t.Context(), MigrateRequest{StartDir: fixture.projectRoot, TeamName: "default"})
	if err == nil || result.Outcome != "failed" {
		t.Fatalf("mutation result=%+v err=%v", result, err)
	}
	entries, readErr := os.ReadDir(filepath.Join(fixture.manager.stateRoot, "staging"))
	if readErr != nil && !os.IsNotExist(readErr) {
		t.Fatal(readErr)
	}
	if len(entries) != 0 {
		t.Fatalf("owned staging was not cleaned: %v", entries)
	}
}

func TestMigrateRejectsSubjectStateOverlapBeforeReservation(t *testing.T) {
	projectRoot := t.TempDir()
	if err := os.Mkdir(filepath.Join(projectRoot, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(projectRoot, "workspace", "default"), 0o755); err != nil {
		t.Fatal(err)
	}
	stateRoot := filepath.Join(projectRoot, ".hufu-state")
	manager, err := NewManager(stateRoot)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = manager.Migrate(t.Context(), MigrateRequest{StartDir: projectRoot, TeamName: "default"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("overlap migration error = %v, want ErrConflict", err)
	}
	if _, statErr := os.Stat(filepath.Join(stateRoot, registryFilename)); !os.IsNotExist(statErr) {
		t.Fatalf("overlap migration created registry: %v", statErr)
	}
}

func TestMigrateAllTeamsIsSortedAndStopsWithPendingList(t *testing.T) {
	root := t.TempDir()
	projectRoot := filepath.Join(root, "project")
	if err := os.MkdirAll(filepath.Join(projectRoot, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	legacyRoot := filepath.Join(projectRoot, "workspace")
	for _, teamName := range []string{"charlie", "alpha", "bravo"} {
		if err := os.MkdirAll(filepath.Join(legacyRoot, teamName), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(legacyRoot, teamName, "artifact.txt"), []byte(teamName), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(legacyRoot, "bravo", legacyContextFilename), []byte("not sqlite"), 0o600); err != nil {
		t.Fatal(err)
	}
	manager, err := NewManager(filepath.Join(root, "state"), WithIDGenerator(NewIDGenerator(bytes.NewReader(bytes.Repeat([]byte{0x66}, 192)))))
	if err != nil {
		t.Fatal(err)
	}
	result, err := manager.Migrate(t.Context(), MigrateRequest{StartDir: projectRoot, AllTeams: true})
	if err == nil || result.Outcome != "failed" {
		t.Fatalf("result=%+v error=%v", result, err)
	}
	if len(result.Completed) != 1 || result.Completed[0].TeamName != "alpha" || !slices.Equal(result.Failed, []string{"bravo"}) || !slices.Equal(result.Pending, []string{"charlie"}) {
		t.Fatalf("all-team progress = %+v", result)
	}
}

func TestVerifyLegacySessionValidatesEventHashChain(t *testing.T) {
	root := t.TempDir()
	logs := filepath.Join(root, "logs")
	if err := os.MkdirAll(logs, 0o755); err != nil {
		t.Fatal(err)
	}
	event := eventchain.Entry{ID: "evt-1", Type: "run_started", Timestamp: "2026-09-17T00:00:00Z", Payload: json.RawMessage(`{"ok":true}`)}
	event.Hash = eventchain.ComputeHash("", event.ID, event.Type, event.Timestamp, event.Payload)
	data, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(logs, "event_store.jsonl")
	if err = os.WriteFile(path, append(data, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	if err = verifyLegacySession(root); err != nil {
		t.Fatal(err)
	}
	event.Hash = "tampered"
	data, _ = json.Marshal(event)
	if err = os.WriteFile(path, append(data, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	if err = verifyLegacySession(root); err == nil || !bytes.Contains([]byte(err.Error()), []byte(IssueEventChainInvalid)) {
		t.Fatalf("tampered event error = %v", err)
	}
}

type migrationFixture struct {
	projectRoot string
	source      string
	manager     *WorkspaceManager
}

func newMigrationFixture(t *testing.T) migrationFixture {
	t.Helper()
	root := t.TempDir()
	projectRoot := filepath.Join(root, "project")
	if err := os.MkdirAll(filepath.Join(projectRoot, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(projectRoot, "workspace", "default")
	if err := os.MkdirAll(source, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "artifact.txt"), []byte("legacy data"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("artifact.txt", filepath.Join(source, "artifact-link")); err != nil {
		t.Fatal(err)
	}
	repository, err := contextstore.OpenSQLite(filepath.Join(source, legacyContextFilename))
	if err != nil {
		t.Fatal(err)
	}
	if err = repository.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(source, legacyContextFilename))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.ExecContext(t.Context(), `INSERT INTO context_events(event_type,item_id,scope_json,payload_json,created_at) VALUES('test',NULL,'{"project_id":"legacy-scope"}','{}',1)`); err != nil {
		t.Fatal(err)
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	manager, err := NewManager(filepath.Join(root, "state"), WithIDGenerator(NewIDGenerator(bytes.NewReader(bytes.Repeat([]byte{0x11}, 96)))))
	if err != nil {
		t.Fatal(err)
	}
	return migrationFixture{projectRoot: projectRoot, source: source, manager: manager}
}
