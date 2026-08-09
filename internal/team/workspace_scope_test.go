package team

import (
	"os"
	"path/filepath"
	"testing"
)

func TestValidateWorkspaceSeparation(t *testing.T) {
	root := t.TempDir()
	if err := ValidateWorkspaceSeparation(filepath.Join(root, "control"), filepath.Join(root, "subject")); err != nil {
		t.Fatal(err)
	}
	if err := ValidateWorkspaceSeparation(root, filepath.Join(root, "subject")); err == nil {
		t.Fatal("expected ancestor overlap to be rejected")
	}
	link := filepath.Join(root, "subject-link")
	if err := os.Symlink(root, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if err := ValidateWorkspaceSeparation(filepath.Join(root, "control"), link); err == nil {
		t.Fatal("expected symlink overlap to be rejected")
	}
}

func TestValidateSharedContextFilesRejectsMemoryAndRequiresRealHandoff(t *testing.T) {
	workspace := t.TempDir()
	shared := filepath.Join(workspace, sharedDir)
	if err := os.MkdirAll(shared, 0o755); err != nil {
		t.Fatalf("create shared dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(shared, "handoff.md"), []byte("typed handoff"), 0o600); err != nil {
		t.Fatalf("write handoff: %v", err)
	}
	if err := validateSharedContextFiles(workspace, []string{"handoff.md"}); err != nil {
		t.Fatalf("valid shared handoff rejected: %v", err)
	}
	for _, invalid := range []string{"../ltm-team.md", filepath.Join(workspace, "ltm-team.md"), "missing.md"} {
		if err := validateSharedContextFiles(workspace, []string{invalid}); err == nil {
			t.Errorf("context file %q unexpectedly accepted", invalid)
		}
	}
}
