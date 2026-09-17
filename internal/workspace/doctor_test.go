package workspace

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestDoctorReadOnlyReportsMarkerAndContextFailuresWithoutChangingRegistry(t *testing.T) {
	root := t.TempDir()
	projectRoot := filepath.Join(root, "project")
	if err := os.MkdirAll(filepath.Join(projectRoot, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	stateRoot := filepath.Join(root, "state")
	manager, err := NewManager(stateRoot, WithIDGenerator(NewIDGenerator(bytes.NewReader(bytes.Repeat([]byte{0x71}, 96)))))
	if err != nil {
		t.Fatal(err)
	}
	resolution, err := manager.Resolve(t.Context(), ResolveRequest{StartDir: projectRoot, TeamName: "default", Mode: ResolveEnsure})
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(filepath.Join(resolution.ControlRoot, "workspace.json")); err != nil {
		t.Fatal(err)
	}
	registryPath := filepath.Join(stateRoot, registryFilename)
	before, err := os.ReadFile(registryPath)
	if err != nil {
		t.Fatal(err)
	}
	result, err := manager.Doctor(t.Context(), DoctorRequest{StartDir: projectRoot})
	if err != nil {
		t.Fatal(err)
	}
	if result.Outcome != "partial" || !hasDoctorIssue(result, IssueWorkspaceMarkerMissing) {
		t.Fatalf("doctor result = %+v", result)
	}
	after, err := os.ReadFile(registryPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("read-only doctor changed registry bytes")
	}
}

func TestDoctorRepairRemovesOnlyMarkerBackedOrphanStaging(t *testing.T) {
	root := t.TempDir()
	projectRoot := filepath.Join(root, "project")
	if err := os.MkdirAll(filepath.Join(projectRoot, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	stateRoot := filepath.Join(root, "state")
	crash := errors.New("crash")
	registry, err := OpenReadWrite(stateRoot,
		WithIDGenerator(NewIDGenerator(bytes.NewReader(bytes.Repeat([]byte{0x72}, 128)))),
		WithCreateHook(func(stage CreateStage) error {
			if stage == CreateStageOperationMarked {
				return crash
			}
			return nil
		}),
	)
	if err != nil {
		t.Fatal(err)
	}
	project, err := registry.RegisterProject(t.Context(), projectRoot)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = registry.CreateWorkspace(t.Context(), project.ID, "default"); !errors.Is(err, crash) {
		t.Fatalf("create interruption = %v", err)
	}
	workspace, err := registry.GetWorkspace(t.Context(), project.ID, "default")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = registry.db.Exec("UPDATE registry_operations SET state='failed',finished_at=1 WHERE id=?", workspace.OperationID); err != nil {
		t.Fatal(err)
	}
	if _, err = registry.db.Exec("DELETE FROM workspaces WHERE id=?", workspace.ID); err != nil {
		t.Fatal(err)
	}
	if err = registry.Close(); err != nil {
		t.Fatal(err)
	}
	manager, err := NewManager(stateRoot, WithIDGenerator(NewIDGenerator(bytes.NewReader(bytes.Repeat([]byte{0x73}, 32)))))
	if err != nil {
		t.Fatal(err)
	}
	result, err := manager.Doctor(t.Context(), DoctorRequest{StartDir: projectRoot, Repair: true})
	if err != nil {
		t.Fatal(err)
	}
	if !hasRepairedDoctorIssue(result, IssueOrphanStaging) {
		t.Fatalf("doctor result = %+v", result)
	}
	if _, statErr := os.Stat(workspace.PendingPath); !os.IsNotExist(statErr) {
		t.Fatalf("orphan staging remains: %v", statErr)
	}
}

func hasDoctorIssue(result DoctorResult, code string) bool {
	for _, issue := range result.Issues {
		if issue.Code == code {
			return true
		}
	}
	return false
}

func hasRepairedDoctorIssue(result DoctorResult, code string) bool {
	for _, issue := range result.Issues {
		if issue.Code == code && issue.Repaired {
			return true
		}
	}
	return false
}
