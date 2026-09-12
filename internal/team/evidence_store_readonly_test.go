package team

import (
	"os"
	"path/filepath"
	"testing"
)

func TestOpenFileArtifactStoreReadOnlyDoesNotCreateMissingDirectories(t *testing.T) {
	workspace := filepath.Join(t.TempDir(), "missing-workspace")
	if _, err := OpenFileArtifactStoreReadOnly(workspace, workspace); err == nil {
		t.Fatal("open read-only artifact store succeeded without an existing store")
	}
	if _, err := os.Stat(workspace); !os.IsNotExist(err) {
		t.Fatalf("read-only opener created workspace: %v", err)
	}
}

func TestOpenFileArtifactStoreReadOnlyRejectsPut(t *testing.T) {
	workspace := t.TempDir()
	writable, err := NewFileArtifactStore(workspace, workspace)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := writable.Put(t.Context(), PutArtifactRequest{
		Path:    "evidence.txt",
		Content: []byte("evidence"),
	})
	if err != nil {
		t.Fatal(err)
	}

	readOnly, err := OpenFileArtifactStoreReadOnly(workspace, workspace)
	if err != nil {
		t.Fatal(err)
	}
	if err := readOnly.Verify(t.Context(), stored.ArtifactRef); err != nil {
		t.Fatalf("verify through read-only store: %v", err)
	}
	if _, err := readOnly.Put(t.Context(), PutArtifactRequest{
		Path:    "forbidden.txt",
		Content: []byte("must not be written"),
	}); err == nil {
		t.Fatal("read-only artifact store accepted Put")
	}
	if _, err := os.Stat(filepath.Join(workspace, "logs", "artifacts", "data", "forbidden.txt")); !os.IsNotExist(err) {
		t.Fatalf("read-only Put created data: %v", err)
	}
}
