package team

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/kjelly/hufu/internal/agent"
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

func TestSetCompatibilityWorkspaceScopePreservesLegacyPaths(t *testing.T) {
	control := filepath.Join(t.TempDir(), "workspace", "review")
	subject := t.TempDir()
	session := &TeamSession{Workspace: control}

	if err := session.SetCompatibilityWorkspaceScope(subject); err != nil {
		t.Fatal(err)
	}
	if session.Scope.ControlRoot != control || session.Workspace != control || session.Config.WorkspaceDir != control {
		t.Fatalf("control aliases diverged: scope=%q workspace=%q config=%q", session.Scope.ControlRoot, session.Workspace, session.Config.WorkspaceDir)
	}
	if session.Scope.SubjectRoot != subject || session.Scope.ProjectRoot != subject || session.Scope.ContextScopeID != subject {
		t.Fatalf("compatibility subject scope = %#v, want %q", session.Scope, subject)
	}
	if session.Scope.Managed || session.Scope.ProjectID != "" {
		t.Fatalf("compatibility scope unexpectedly managed: %#v", session.Scope)
	}
}

func TestSetWorkspaceScopeRequiresManagedIdentityAndSeparation(t *testing.T) {
	root := t.TempDir()
	session := &TeamSession{}
	if err := session.SetWorkspaceScope(WorkspaceScope{
		ContextScopeID: root,
		ControlRoot:    filepath.Join(root, "control"),
		SubjectRoot:    filepath.Join(root, "subject"),
		Managed:        true,
	}); err == nil {
		t.Fatal("managed scope without project ID was accepted")
	}
	if err := session.SetWorkspaceScope(WorkspaceScope{
		ProjectID:      "prj_test",
		ContextScopeID: root,
		ControlRoot:    root,
		SubjectRoot:    filepath.Join(root, "subject"),
		Managed:        true,
	}); err == nil {
		t.Fatal("overlapping managed scope was accepted")
	}
}

func TestSetWorkspacePreviewScopeValidatesPathsWithoutPersistedIdentity(t *testing.T) {
	root := t.TempDir()
	session := &TeamSession{}
	scope := WorkspaceScope{
		ContextScopeID: filepath.Join(root, "subject"),
		ControlRoot:    filepath.Join(root, "state"),
		SubjectRoot:    filepath.Join(root, "subject"),
		Managed:        true,
	}
	if err := session.SetWorkspacePreviewScope(scope); err != nil {
		t.Fatal(err)
	}
	if session.Scope.ProjectID != "" || session.Workspace != scope.ControlRoot || session.Config.WorkspaceDir != scope.ControlRoot {
		t.Fatalf("preview scope aliases = %#v workspace=%q config=%q", session.Scope, session.Workspace, session.Config.WorkspaceDir)
	}

	scope.ControlRoot = root
	if err := session.SetWorkspacePreviewScope(scope); err == nil {
		t.Fatal("overlapping preview scope was accepted")
	}
}

func TestContextScopeUsesCompatibilityIdentityInsteadOfSubjectRoot(t *testing.T) {
	session := &TeamSession{
		Workspace: "/control",
		Config:    agent.TeamConfig{Name: "review"},
		Scope: WorkspaceScope{
			ContextScopeID: "/legacy/context/root",
			ControlRoot:    "/control",
			SubjectRoot:    "/current/subject/root",
			ProjectRoot:    "/current/subject/root",
		},
	}
	c := &Coordinator{session: session, projectDir: session.Scope.SubjectRoot}
	if got := c.contextScope().ProjectID; got != session.Scope.ContextScopeID {
		t.Fatalf("context project ID = %q, want %q", got, session.Scope.ContextScopeID)
	}
}

func TestNewCoordinatorUsesSessionSubjectRoot(t *testing.T) {
	control := t.TempDir()
	subject := t.TempDir()
	session := &TeamSession{Workspace: control, Config: agent.TeamConfig{Name: "review"}}
	if err := session.SetCompatibilityWorkspaceScope(subject); err != nil {
		t.Fatal(err)
	}

	coordinator, err := NewCoordinator(session, "", "", nil, nil, nil, RoleModels{}, 1, false, false, false, nil, nil, nil, false, "", false, false, nil, false, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = coordinator.Close() })
	if coordinator.projectDir != subject {
		t.Fatalf("coordinator project directory = %q, want scope subject %q", coordinator.projectDir, subject)
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
