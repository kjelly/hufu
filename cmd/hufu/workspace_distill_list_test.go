package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDistillationListIsReadOnlyAndConfined(t *testing.T) {
	workspace := t.TempDir()
	entries, err := listDistillationAuditWorkspaces(workspace)
	if err != nil || len(entries) != 0 {
		t.Fatalf("empty listing: %v %v", entries, err)
	}
	parent := filepath.Join(workspace, "maintenance", "task-distillation")
	if _, err = os.Stat(parent); !os.IsNotExist(err) {
		t.Fatal("read created a maintenance directory")
	}
	for _, name := range []string{"draft-old", "draft-new", "unrelated"} {
		if err = os.MkdirAll(filepath.Join(parent, name), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	before := time.Now().Add(-time.Hour)
	if err = os.Chtimes(filepath.Join(parent, "draft-old"), before, before); err != nil {
		t.Fatal(err)
	}
	if err = os.Symlink(t.TempDir(), filepath.Join(parent, "draft-external")); err != nil {
		t.Fatal(err)
	}
	entries, err = listDistillationAuditWorkspaces(workspace)
	if err != nil || len(entries) != 2 || entries[0].ID != "draft-new" || entries[1].ID != "draft-old" {
		t.Fatalf("entries=%v err=%v", entries, err)
	}
}
