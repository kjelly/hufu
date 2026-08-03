package team

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestCleanRunDirsWithEvidencePersistsBeforeAfterState(t *testing.T) {
	workspace := t.TempDir()
	if err := EnsureWorkspaceDirs(workspace); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, tasksDir, "task.txt"), []byte("secret-looking task"), 0o600); err != nil {
		t.Fatal(err)
	}
	evidence, err := CleanRunDirsWithEvidence(workspace)
	if err != nil {
		t.Fatal(err)
	}
	if len(evidence.Before) != 2 || evidence.Before[0].EntryCount != 1 {
		t.Fatalf("unexpected before state: %#v", evidence.Before)
	}
	if len(evidence.After) != 2 || evidence.After[0].Exists {
		t.Fatalf("unexpected after state: %#v", evidence.After)
	}
	data, err := os.ReadFile(filepath.Join(workspace, logsDir, "cleanup_evidence.json"))
	if err != nil {
		t.Fatal(err)
	}
	var persisted CleanupEvidence
	if err := json.Unmarshal(data, &persisted); err != nil {
		t.Fatal(err)
	}
	if persisted.Workspace == "" || len(persisted.Before) != 2 || len(persisted.After) != 2 {
		t.Fatalf("incomplete persisted evidence: %#v", persisted)
	}
}

func TestCleanRunDirsRefusesProtectedRoots(t *testing.T) {
	if _, err := CleanRunDirsWithEvidence("/"); err == nil {
		t.Fatal("cleanup of root must be rejected")
	}
	if home, err := os.UserHomeDir(); err == nil {
		if _, err := CleanRunDirsWithEvidence(home); err == nil {
			t.Fatal("cleanup of home must be rejected")
		}
	}
}

func TestCleanRunDirsWithEvidenceFailsWhenEvidenceCannotBeWritten(t *testing.T) {
	workspace := t.TempDir()
	if err := EnsureWorkspaceDirs(workspace); err != nil {
		t.Fatal(err)
	}
	logsPath := filepath.Join(workspace, logsDir)
	if err := os.RemoveAll(logsPath); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(logsPath, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := CleanRunDirsWithEvidence(workspace); err == nil {
		t.Fatal("evidence persistence failure must be returned")
	}
}
