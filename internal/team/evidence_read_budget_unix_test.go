//go:build unix

package team

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func TestArtifactReadBudgetRejectsSpecialFiles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "fifo")
	if err := unix.Mkfifo(path, 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, _ := WithArtifactReadBudget(t.Context(), 1024)
	if _, err := readArtifactFile(ctx, path); err == nil {
		t.Fatal("FIFO accepted")
	}
	target := filepath.Join(dir, "target")
	if err := os.WriteFile(target, []byte("private"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := readArtifactFile(ctx, link); err == nil {
		t.Fatal("symlink accepted")
	}
}
