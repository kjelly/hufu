package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kjelly/hufu/internal/config"
	"github.com/kjelly/hufu/internal/team"
	workspacepkg "github.com/kjelly/hufu/internal/workspace"
	"github.com/kjelly/hufu/internal/workspace/versionstore"
)

type versionedCLIFixture struct {
	control string
	subject string
	state   string
}

// newVersionedCLIFixture registers a project and a team workspace in an
// isolated state root and enables required-mode versioning in the user
// config.
func newVersionedCLIFixture(t *testing.T) versionedCLIFixture {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	state := filepath.Join(home, "state")
	t.Setenv("HUFU_STATE_HOME", state)
	if err := os.MkdirAll(filepath.Join(home, ".config", "hufu"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".config", "hufu", "hufu.yaml"), []byte("workspace-versioning:\n  mode: required\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	subject := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(filepath.Join(subject, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(subject, ".hufuignore"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	registry, err := workspacepkg.OpenReadWrite(state)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = registry.Close() }()
	project, err := registry.RegisterProject(context.Background(), subject)
	if err != nil {
		t.Fatal(err)
	}
	created, err := registry.CreateWorkspace(context.Background(), project.ID, "dev")
	if err != nil {
		t.Fatal(err)
	}
	return versionedCLIFixture{control: created.ControlRoot, subject: project.SubjectRoot, state: project.StateDir}
}

func (f versionedCLIFixture) write(t *testing.T, rel, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(f.subject, rel), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func (f versionedCLIFixture) read(t *testing.T, rel string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(f.subject, rel))
	if err != nil {
		return "<absent>"
	}
	return string(data)
}

// runCLI runs one hufu invocation and returns stdout. Package-level flag
// variables survive between invocations, so they are reset first.
func runCLI(t *testing.T, args ...string) (string, error) {
	t.Helper()
	sessionWorkspace, sessionJSON, sessionMetadataOnly, sessionForkName = "", false, false, ""
	var runErr error
	out := captureStdout(t, func() {
		root := newRootCommand()
		root.SetArgs(args)
		runErr = root.Execute()
	})
	return out, runErr
}

func TestCLISessionWorkspaceVersioningEndToEnd(t *testing.T) {
	f := newVersionedCLIFixture(t)
	f.write(t, "A.txt", "main")

	out, err := runCLI(t, "session", "fork", "--name", "exp", "--workspace", f.control, "--json")
	if err != nil {
		t.Fatalf("fork: %v", err)
	}
	var fork sessionForkOutput
	if err = json.Unmarshal([]byte(out), &fork); err != nil {
		t.Fatalf("fork output %q: %v", out, err)
	}
	if fork.BranchID != "exp" || fork.WorkspaceSnapshotID == "" || fork.MetadataOnly || fork.WorkspaceState != "available" {
		t.Fatalf("fork = %#v", fork)
	}

	f.write(t, "A.txt", "exp")
	f.write(t, "B.txt", "only on exp")
	if _, err = runCLI(t, "session", "checkout", "main", "--workspace", f.control); err != nil {
		t.Fatalf("checkout main: %v", err)
	}
	if f.read(t, "A.txt") != "main" || f.read(t, "B.txt") != "<absent>" {
		t.Fatalf("after checkout main: A=%q B=%q", f.read(t, "A.txt"), f.read(t, "B.txt"))
	}

	out, err = runCLI(t, "session", "diff", "main", "exp", "--workspace", f.control, "--json")
	if err != nil {
		t.Fatalf("diff: %v", err)
	}
	if !strings.Contains(out, `"workspace_diff"`) || !strings.Contains(out, "B.txt") {
		t.Fatalf("diff output lacks the workspace section: %s", out)
	}

	out, err = runCLI(t, "session", "list", "--workspace", f.control, "--json")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if strings.Count(out, `"workspace_state":"available"`) != 2 {
		t.Fatalf("list output = %s", out)
	}

	if _, err = runCLI(t, "session", "checkout", "exp", "--workspace", f.control, "--metadata-only"); err == nil || !strings.Contains(err.Error(), "only for legacy branches") {
		t.Fatalf("metadata-only checkout of a versioned branch: err = %v", err)
	}

	out, err = runCLI(t, "workspace", "version", "list", "--workspace", f.control, "--json")
	if err != nil {
		t.Fatalf("version list: %v", err)
	}
	var snapshots []snapshotView
	if err = json.Unmarshal([]byte(out), &snapshots); err != nil || len(snapshots) < 3 {
		t.Fatalf("version list = %s (%v)", out, err)
	}
	baseline := snapshots[0].ID

	f.write(t, "A.txt", "edited on main")
	if _, err = runCLI(t, "workspace", "version", "restore", baseline, "--workspace", f.control); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if f.read(t, "A.txt") != "main" {
		t.Fatalf("after restore A=%q", f.read(t, "A.txt"))
	}
}

// TestCLISessionCheckoutRefusedWhileProjectLocked covers the run-conflict
// rule of §27 (IT10): another holder of the project lock blocks checkout.
func TestCLISessionCheckoutRefusedWhileProjectLocked(t *testing.T) {
	f := newVersionedCLIFixture(t)
	f.write(t, "A.txt", "main")
	if _, err := runCLI(t, "session", "fork", "--name", "exp", "--workspace", f.control); err != nil {
		t.Fatalf("fork: %v", err)
	}
	lock, err := versionstore.TryLockProject(f.state, versionstore.LockOwner{WorkspaceID: "ws_other_team", Operation: "run"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lock.Close() }()
	_, err = runCLI(t, "session", "checkout", "main", "--workspace", f.control)
	if err == nil || !strings.Contains(err.Error(), "conflicts with active execution") || !strings.Contains(err.Error(), "ws_other_team") {
		t.Fatalf("checkout under a held project lock: err = %v", err)
	}
}

// TestCLISessionForkFailsClosedWithoutEventStore covers E3: an unusable
// event store is an error, not a silently lineage-free fork.
func TestCLISessionForkFailsClosedWithoutEventStore(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "logs"), []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := runCLI(t, "session", "fork", "--name", "x", "--workspace", workspace)
	if err == nil || !strings.Contains(err.Error(), "open event store") {
		t.Fatalf("err = %v, want an event store error", err)
	}
}

// TestBindRunWorkspaceVersioning covers the run binding of §27 and G13: a
// required-mode run holds the project lock until its lease closes, and a
// later run configured below the recorded floor is refused.
func TestBindRunWorkspaceVersioning(t *testing.T) {
	f := newVersionedCLIFixture(t)
	stateRoot := os.Getenv("HUFU_STATE_HOME")
	registry, err := workspacepkg.OpenReadOnly(stateRoot)
	if err != nil {
		t.Fatal(err)
	}
	managed, err := registry.GetWorkspaceByControlRoot(context.Background(), f.control)
	_ = registry.Close()
	if err != nil {
		t.Fatal(err)
	}
	newSession := func() *team.TeamSession {
		return &team.TeamSession{Workspace: f.control, WorkspaceLease: &commandWorkspaceLease{stateRoot: stateRoot, workspaceID: managed.ID}}
	}
	cfg := &config.Config{}
	cfg.WorkspaceVersioning.Mode = "required"
	session := newSession()
	if err = bindRunWorkspaceVersioning(context.Background(), session, cfg); err != nil {
		t.Fatalf("bind required: %v", err)
	}
	if !session.WorkspaceVersion.Required() || !session.WorkspaceVersion.HoldsProjectLock {
		t.Fatalf("version = %#v", session.WorkspaceVersion)
	}
	if other := newSession(); bindRunWorkspaceVersioning(context.Background(), other, cfg) == nil {
		t.Fatal("a second required-mode coordinator on the same project was admitted")
	}
	if err = closeSessionWorkspaceLease(session); err != nil {
		t.Fatal(err)
	}
	cfg.WorkspaceVersioning.Mode = "off"
	if err = bindRunWorkspaceVersioning(context.Background(), newSession(), cfg); err == nil || !strings.Contains(err.Error(), "floor required") {
		t.Fatalf("downgrade to off: err = %v", err)
	}
	cfg.WorkspaceVersioning.Mode = "sometimes"
	if err = bindRunWorkspaceVersioning(context.Background(), newSession(), cfg); err == nil {
		t.Fatal("an unknown mode was accepted")
	}
}

// TestCLIWorkspaceVersionAdopt covers the §22.3 operator path: adopt clears
// the recovery marker and records the current files as the new head.
func TestCLIWorkspaceVersionAdopt(t *testing.T) {
	f := newVersionedCLIFixture(t)
	f.write(t, "A.txt", "main")
	if _, err := runCLI(t, "workspace", "version", "snapshot", "--workspace", f.control); err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if _, err := runCLI(t, "workspace", "version", "adopt", "--workspace", f.control); err == nil || !strings.Contains(err.Error(), "nothing to adopt") {
		t.Fatalf("adopt without a marker: err = %v", err)
	}
	store, err := versionstore.Open(context.Background(), versionstore.Options{StateDir: f.state})
	if err != nil {
		t.Fatal(err)
	}
	subject, _ := versionstore.CanonicalRoot(f.subject)
	if err = store.UpdateSubjectState(context.Background(), subject, func(state *versionstore.SubjectState) {
		state.RecoveryRequired, state.RecoveryCode = true, "unauthorized_mutation"
	}); err != nil {
		t.Fatal(err)
	}
	_ = store.Close()
	f.write(t, "A.txt", "changed by a provider")
	if _, err = runCLI(t, "session", "fork", "--name", "exp", "--workspace", f.control); err == nil || !strings.Contains(err.Error(), "requires operator recovery") {
		t.Fatalf("fork during recovery: err = %v", err)
	}
	out, err := runCLI(t, "workspace", "version", "adopt", "--workspace", f.control, "--json")
	if err != nil {
		t.Fatalf("adopt: %v", err)
	}
	if !strings.Contains(out, `"reason":"adopt"`) {
		t.Fatalf("adopt output = %s", out)
	}
	if _, err = runCLI(t, "session", "fork", "--name", "exp", "--workspace", f.control); err != nil {
		t.Fatalf("fork after adopt: %v", err)
	}
}
