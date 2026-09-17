package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/kjelly/hufu/internal/team"
)

func TestResolveTeamWorkspacePathCanonicalizesWorkingDirectorySymlink(t *testing.T) {
	physical := t.TempDir()
	if err := os.Mkdir(filepath.Join(physical, ".git"), 0o755); err != nil {
		t.Fatalf("create git marker: %v", err)
	}
	aliasParent := t.TempDir()
	alias := filepath.Join(aliasParent, "repo-alias")
	if err := os.Symlink(physical, alias); err != nil {
		t.Fatalf("create repository alias: %v", err)
	}
	original, err := os.Getwd()
	if err != nil {
		t.Fatalf("get working directory: %v", err)
	}
	if err := os.Chdir(alias); err != nil {
		t.Fatalf("chdir through repository alias: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(original) })

	originalWorkspace := opts.workspace
	opts.workspace = ""
	t.Cleanup(func() { opts.workspace = originalWorkspace })

	session := &team.TeamSession{}
	if err := resolveTeamWorkspacePath("review", session); err != nil {
		t.Fatalf("resolve team workspace: %v", err)
	}
	want := filepath.Join(physical, "workspace", "review")
	if session.Workspace != want {
		t.Fatalf("workspace = %q, want canonical path %q", session.Workspace, want)
	}
	if session.Config.WorkspaceDir != want {
		t.Fatalf("config workspace = %q, want canonical path %q", session.Config.WorkspaceDir, want)
	}
}
