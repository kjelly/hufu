//go:build unix

package runtimestore

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// TestOpenKeepsEverySidecarPrivate pins that the WAL, shared-memory, and
// rollback files are as private as the database under a permissive umask:
// they hold the same runtime data.
func TestOpenKeepsEverySidecarPrivate(t *testing.T) {
	previous := syscall.Umask(0o022)
	t.Cleanup(func() { syscall.Umask(previous) })
	tests := []struct {
		name  string
		setup func(t *testing.T, workspace string)
	}{
		{name: "new database"},
		{name: "existing database and WAL with a wide mode", setup: func(t *testing.T, workspace string) {
			for _, name := range []string{File, File + "-wal"} {
				if err := os.WriteFile(filepath.Join(workspace, name), nil, 0o644); err != nil {
					t.Fatal(err)
				}
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			workspace := t.TempDir()
			if tt.setup != nil {
				tt.setup(t, workspace)
			}
			store, err := Open(context.Background(), workspace)
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			if err := store.PutDocument(context.Background(), DocumentSession, []byte(`{"secret":"in the wal"}`)); err != nil {
				t.Fatal(err)
			}
			seen := 0
			for _, name := range Files {
				info, err := os.Stat(filepath.Join(workspace, name))
				if os.IsNotExist(err) {
					continue
				}
				if err != nil {
					t.Fatal(err)
				}
				seen++
				if mode := info.Mode().Perm(); mode != 0o600 {
					t.Errorf("%s mode = %o, want 600", name, mode)
				}
			}
			if seen < 3 {
				t.Fatalf("only %d of the database files exist; the WAL and shared memory were not exercised", seen)
			}
		})
	}
}
