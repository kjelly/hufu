package workspace

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestWorkspaceGCDryRunIsReadOnlyAndApplyReusesPurge(t *testing.T) {
	root := t.TempDir()
	subjectRoot := filepath.Join(root, "project")
	if err := os.MkdirAll(filepath.Join(subjectRoot, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	stateRoot := filepath.Join(root, "state")
	now := time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)
	clock := now
	identifierBytes := make([]byte, 0, 16*20)
	for value := byte(1); value <= 20; value++ {
		identifierBytes = append(identifierBytes, bytes.Repeat([]byte{value}, 16)...)
	}
	manager, err := NewManager(stateRoot,
		WithIDGenerator(NewIDGenerator(bytes.NewReader(identifierBytes))),
		WithClock(func() time.Time { return clock }),
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = manager.Resolve(t.Context(), ResolveRequest{StartDir: subjectRoot, TeamName: "default", Mode: ResolveEnsure}); err != nil {
		t.Fatal(err)
	}
	deleted, err := manager.Delete(t.Context(), DeleteRequest{StartDir: subjectRoot, TeamName: "default", Retention: 720 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	registryPath := filepath.Join(stateRoot, registryFilename)
	before, err := os.ReadFile(registryPath)
	if err != nil {
		t.Fatal(err)
	}
	clock = now.Add(2 * time.Hour)
	dryRun, err := manager.GC(t.Context(), GCRequest{TrashOlderThan: time.Hour})
	if err != nil || dryRun.Apply || len(dryRun.Candidates) != 1 || len(dryRun.Purged) != 0 {
		t.Fatalf("dry-run GC = %+v, %v", dryRun, err)
	}
	after, err := os.ReadFile(registryPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) || !pathExists(deleted.Items[0].TrashPath) {
		t.Fatal("dry-run GC changed registry or trash")
	}
	applied, err := manager.GC(t.Context(), GCRequest{Apply: true, TrashOlderThan: time.Hour})
	if err != nil || len(applied.Purged) != 1 || pathExists(deleted.Items[0].TrashPath) {
		t.Fatalf("apply GC = %+v, %v", applied, err)
	}
}

func TestWorkspaceGCLeavesYoungTrashUntouched(t *testing.T) {
	fixture := newLifecycleFixture(t, nil)
	deleted, err := fixture.manager.Delete(t.Context(), DeleteRequest{StartDir: fixture.subjectRoot, TeamName: "default"})
	if err != nil {
		t.Fatal(err)
	}
	result, err := fixture.manager.GC(t.Context(), GCRequest{Apply: true, TrashOlderThan: time.Hour})
	if err != nil || len(result.Candidates) != 0 || !pathExists(deleted.Items[0].TrashPath) {
		t.Fatalf("young trash GC = %+v, %v", result, err)
	}
}
