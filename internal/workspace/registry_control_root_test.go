package workspace

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestGetWorkspaceByControlRoot(t *testing.T) {
	stateRoot := filepath.Join(t.TempDir(), "state")
	subjectRoot := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(filepath.Join(subjectRoot, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	registry := openTestRegistry(t, stateRoot)
	defer registry.Close()
	project, err := registry.RegisterProject(t.Context(), subjectRoot)
	if err != nil {
		t.Fatal(err)
	}
	created, err := registry.CreateWorkspace(t.Context(), project.ID, "dev")
	if err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(t.TempDir(), "alias")
	if err = os.Symlink(created.ControlRoot, alias); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name    string
		path    string
		wantErr error
	}{
		{name: "exact control root", path: created.ControlRoot},
		{name: "symlinked spelling", path: alias},
		{name: "subject root is not a control root", path: subjectRoot, wantErr: ErrNotFound},
		{name: "unrelated directory", path: t.TempDir(), wantErr: ErrNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := registry.GetWorkspaceByControlRoot(t.Context(), tt.path)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("err = %v, want %v", err, tt.wantErr)
				}
				return
			}
			if err != nil || got.ID != created.ID || got.ProjectID != project.ID {
				t.Fatalf("got %+v, %v; want workspace %s", got, err, created.ID)
			}
		})
	}
}
