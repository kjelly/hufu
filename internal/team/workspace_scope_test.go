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
