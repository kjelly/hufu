package main

import (
	"os"
	"path/filepath"
	"strings"
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
	stateRoot := filepath.Join(t.TempDir(), "state")
	t.Setenv("HUFU_STATE_HOME", stateRoot)

	session := &team.TeamSession{}
	if err := resolveTeamWorkspacePath("review", session); err != nil {
		t.Fatalf("resolve team workspace: %v", err)
	}
	if !session.Scope.Managed || session.Scope.SubjectRoot != physical || !strings.HasPrefix(session.Workspace, filepath.Join(stateRoot, "projects")) {
		t.Fatalf("managed symlink scope = %+v", session.Scope)
	}
	if session.Config.WorkspaceDir != session.Workspace {
		t.Fatalf("config workspace = %q, want %q", session.Config.WorkspaceDir, session.Workspace)
	}
	_ = closeSessionWorkspaceLease(session)
}
