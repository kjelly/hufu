package team

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/kjelly/hufu/internal/workspace/versionstore"
)

// versionFixture is a required-mode workspace: a control workspace with a
// session tree and event store, a subject root, and a version store.
type versionFixture struct {
	t       *testing.T
	control string
	subject string
	version WorkspaceVersionContext
	store   *versionstore.Store
	events  *EventStore
	tree    *SessionTree
}

func newVersionFixture(t *testing.T) *versionFixture {
	t.Helper()
	subject, err := versionstore.CanonicalRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	control, err := versionstore.CanonicalRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	f := &versionFixture{t: t, control: control, subject: subject}
	f.version = WorkspaceVersionContext{
		Mode: versionstore.ModeRequired, WorkspaceID: "ws_fixture", SubjectRoot: subject, ControlRoot: control,
		StateDir: t.TempDir(), HoldsProjectLock: true,
	}
	f.write(".hufuignore", "")
	f.reopen()
	return f
}

// reopen simulates a new process: stores and the session tree are reloaded
// from disk.
func (f *versionFixture) reopen() {
	f.t.Helper()
	f.closeStores()
	store, err := f.version.OpenStore(context.Background())
	if err != nil {
		f.t.Fatal(err)
	}
	events, err := OpenEventStore(f.control)
	if err != nil {
		f.t.Fatal(err)
	}
	tree, err := LoadSessionTree(f.control)
	if err != nil {
		f.t.Fatal(err)
	}
	f.store, f.events, f.tree = store, events, tree
	f.t.Cleanup(f.closeStores)
}

func (f *versionFixture) closeStores() {
	if f.store != nil {
		_ = f.store.Close()
		f.store = nil
	}
	if f.events != nil {
		_ = f.events.Close()
		f.events = nil
	}
}

func (f *versionFixture) ops() *WorkspaceSessionOps {
	return &WorkspaceSessionOps{Version: f.version, Workspace: f.control, Tree: f.tree, Events: f.events, Store: f.store}
}

func (f *versionFixture) write(rel, content string) {
	f.t.Helper()
	full := filepath.Join(f.subject, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		f.t.Fatal(err)
	}
}

func (f *versionFixture) read(rel string) string {
	f.t.Helper()
	data, err := os.ReadFile(filepath.Join(f.subject, filepath.FromSlash(rel)))
	if err != nil {
		f.t.Fatalf("read %s: %v", rel, err)
	}
	return string(data)
}

func (f *versionFixture) absent(rel string) {
	f.t.Helper()
	if _, err := os.Lstat(filepath.Join(f.subject, filepath.FromSlash(rel))); !errors.Is(err, os.ErrNotExist) {
		f.t.Fatalf("%s should be absent: %v", rel, err)
	}
}

func (f *versionFixture) head(branch string) versionstore.Snapshot {
	f.t.Helper()
	head, _, ok, err := f.store.GetBranchHead(context.Background(), f.version.WorkspaceID, branch)
	if err != nil || !ok {
		f.t.Fatalf("head of %s: ok=%v err=%v", branch, ok, err)
	}
	return head
}

func (f *versionFixture) event(branch, eventType string) string {
	f.t.Helper()
	event, err := f.events.AppendPersisted(RunEvent{BranchID: branch, Type: eventType, Actor: "test", Payload: []byte(`{"note":"x"}`)})
	if err != nil {
		f.t.Fatal(err)
	}
	return event.ID
}

func mustFork(t *testing.T, f *versionFixture, name, target string) WorkspaceForkResult {
	t.Helper()
	result, err := f.ops().Fork(context.Background(), name, target)
	if err != nil {
		t.Fatalf("Fork(%q, %q): %v", name, target, err)
	}
	return result
}

func mustCheckout(t *testing.T, f *versionFixture, target string) WorkspaceCheckoutResult {
	t.Helper()
	result, err := f.ops().Checkout(context.Background(), target)
	if err != nil {
		t.Fatalf("Checkout(%q): %v", target, err)
	}
	return result
}

// TestWorkspaceForkAndCheckoutSwitchFiles is the Phase 3 acceptance flow and
// IT1/IT2: forking the active head reuses its tree, and checkouts switch the
// files back and forth.
func TestWorkspaceForkAndCheckoutSwitchFiles(t *testing.T) {
	f := newVersionFixture(t)
	f.write("A.txt", "main")
	fork := mustFork(t, f, "exp", "")
	mainHead := f.head("main")
	if fork.Snapshot.RootTreeHash != mainHead.RootTreeHash || fork.Snapshot.Parent != mainHead.ID || fork.Materialized {
		t.Fatalf("fork = %#v, main head = %#v", fork, mainHead)
	}
	if f.tree.ActiveBranch != "exp" {
		t.Fatalf("active = %s, want exp", f.tree.ActiveBranch)
	}

	f.write("A.txt", "exp")
	f.write("B.txt", "only on exp")
	result := mustCheckout(t, f, "main")
	if result.Saved == nil || result.Saved.Reason != versionstore.SnapshotCheckoutSave {
		t.Fatalf("exp's live state was not saved: %#v", result.Saved)
	}
	if f.read("A.txt") != "main" {
		t.Fatal("checkout main did not restore A.txt")
	}
	f.absent("B.txt")

	mustCheckout(t, f, "exp")
	if f.read("A.txt") != "exp" || f.read("B.txt") != "only on exp" {
		t.Fatal("checkout exp did not restore exp's files")
	}
	// Repeated switching stays exact (IT2).
	mustCheckout(t, f, "main")
	mustCheckout(t, f, "exp")
	if f.read("A.txt") != "exp" || f.read("B.txt") != "only on exp" {
		t.Fatal("repeated checkouts diverged")
	}
	tree, err := LoadSessionTree(f.control)
	if err != nil || tree.ActiveBranch != "exp" {
		t.Fatalf("persisted active branch = %v, %v", tree.ActiveBranch, err)
	}
}

// TestWorkspaceHistoricalForkUsesSnapshotAtEvent covers IT3 and G1: the fork
// base is the snapshot at the fork event, and the active branch's uncaptured
// edit is saved before the files are replaced.
func TestWorkspaceHistoricalForkUsesSnapshotAtEvent(t *testing.T) {
	ctx := context.Background()
	f := newVersionFixture(t)
	f.write("v.txt", "one")
	if _, _, err := f.ops().SnapshotNow(ctx); err != nil {
		t.Fatal(err)
	}
	e2 := f.event("main", "test_marker")
	f.write("v.txt", "two")
	if _, _, err := f.ops().SnapshotNow(ctx); err != nil {
		t.Fatal(err)
	}
	f.event("main", "test_marker")
	f.write("v.txt", "live, never captured")

	fork := mustFork(t, f, "past", e2)
	if !fork.Materialized || f.read("v.txt") != "one" {
		t.Fatalf("historical fork materialized=%v, v.txt=%q; want the snapshot at %s", fork.Materialized, f.read("v.txt"), e2)
	}
	mustCheckout(t, f, "main")
	if f.read("v.txt") != "live, never captured" {
		t.Fatal("the active branch's uncaptured edit was lost by the historical fork")
	}
}

// TestWorkspaceForkLegacyEventFailsClosed covers IT4: an event without any
// workspace snapshot before it cannot be forked with workspace state.
func TestWorkspaceForkLegacyEventFailsClosed(t *testing.T) {
	f := newVersionFixture(t)
	legacy := f.event("main", "test_marker")
	f.write("x", "1")
	before := len(f.tree.Branches)
	_, err := f.ops().Fork(context.Background(), "old", legacy)
	if !errors.Is(err, versionstore.ErrWorkspaceSnapshotUnavailable) {
		t.Fatalf("err = %v, want ErrWorkspaceSnapshotUnavailable", err)
	}
	if len(f.tree.Branches) != before {
		t.Fatal("a failed fork created a branch")
	}
}

// TestWorkspaceCheckoutLegacyBranchFailsClosed: a branch without a head
// cannot be checked out with workspace semantics.
func TestWorkspaceCheckoutLegacyBranchFailsClosed(t *testing.T) {
	f := newVersionFixture(t)
	if _, err := f.tree.CreateBranch("legacy", "", f.events); err != nil {
		t.Fatal(err)
	}
	if err := SaveSessionTree(f.control, f.tree); err != nil {
		t.Fatal(err)
	}
	if _, err := f.ops().Checkout(context.Background(), "legacy"); !errors.Is(err, versionstore.ErrWorkspaceSnapshotUnavailable) {
		t.Fatalf("err = %v, want ErrWorkspaceSnapshotUnavailable", err)
	}
	if f.tree.ActiveBranch != "main" {
		t.Fatalf("active branch moved to %s", f.tree.ActiveBranch)
	}
}

// TestWorkspaceProjectDirsNamedLikeBookkeeping covers IT12 (B5).
func TestWorkspaceProjectDirsNamedLikeBookkeeping(t *testing.T) {
	f := newVersionFixture(t)
	files := map[string]string{"docs/history/a.md": "a", "internal/tasks/b.go": "b", "logs/c.txt": "c"}
	for rel, content := range files {
		f.write(rel, content)
	}
	mustFork(t, f, "exp", "")
	for rel := range files {
		f.write(rel, "changed on exp")
	}
	mustCheckout(t, f, "main")
	for rel, content := range files {
		if f.read(rel) != content {
			t.Fatalf("%s = %q, want %q", rel, f.read(rel), content)
		}
	}
}

// TestWorkspaceControlRootInsideSubject covers IT11 (defensive, §9.5).
func TestWorkspaceControlRootInsideSubject(t *testing.T) {
	f := newVersionFixture(t)
	inside := filepath.Join(f.subject, "workspace", "dev")
	if err := os.MkdirAll(inside, 0o755); err != nil {
		t.Fatal(err)
	}
	f.closeStores()
	f.control, f.version.ControlRoot = inside, inside
	f.reopen()
	f.write("app.go", "v1")
	mustFork(t, f, "exp", "")
	f.write("app.go", "v2")
	treeBefore, err := os.ReadFile(filepath.Join(inside, sessionTreeFile))
	if err != nil {
		t.Fatal(err)
	}
	mustCheckout(t, f, "main")
	if f.read("app.go") != "v1" {
		t.Fatal("managed file not restored")
	}
	treeAfter, err := os.ReadFile(filepath.Join(inside, sessionTreeFile))
	if err != nil {
		t.Fatal(err)
	}
	if string(treeBefore) == string(treeAfter) {
		t.Fatal("expected checkout to rewrite session_tree.json through the session tree, not through materialize")
	}
	if tree, _ := LoadSessionTree(inside); tree.ActiveBranch != "main" {
		t.Fatalf("session tree was clobbered by materialize: active = %s", tree.ActiveBranch)
	}

	same := f.version
	same.ControlRoot = f.subject
	if _, err = same.ExcludeSubtrees(); !errors.Is(err, versionstore.ErrVersionedWorkspaceUnresolved) {
		t.Fatalf("control root == subject root: err = %v", err)
	}
}

// TestWorkspaceRestoreAndRecoveryMarker covers §20 and §22.3.
func TestWorkspaceRestoreAndRecoveryMarker(t *testing.T) {
	ctx := context.Background()
	f := newVersionFixture(t)
	f.write("a.txt", "v1")
	first, _, err := f.ops().SnapshotNow(ctx)
	if err != nil {
		t.Fatal(err)
	}
	f.write("a.txt", "v2")
	if _, _, err = f.ops().SnapshotNow(ctx); err != nil {
		t.Fatal(err)
	}
	node, _, err := f.ops().Restore(ctx, first.ID)
	if err != nil {
		t.Fatal(err)
	}
	if node.Reason != versionstore.SnapshotRestore || node.RootTreeHash != first.RootTreeHash || f.read("a.txt") != "v1" || f.head("main").ID != node.ID {
		t.Fatalf("restore node = %#v, a.txt = %q", node, f.read("a.txt"))
	}

	// An unauthorized mutation leaves a durable marker; versioned session
	// operations refuse until the operator restores or adopts.
	if err = f.store.UpdateSubjectState(ctx, f.subject, func(state *versionstore.SubjectState) {
		state.RecoveryRequired, state.RecoveryCode = true, "unauthorized_mutation"
	}); err != nil {
		t.Fatal(err)
	}
	f.write("a.txt", "tampered")
	f.write("added-by-violation.txt", "x")
	if _, err = f.ops().Checkout(ctx, "main"); !errors.Is(err, versionstore.ErrWorkspaceRecoveryRequired) {
		t.Fatalf("checkout during recovery: err = %v", err)
	}
	if _, _, err = f.ops().Restore(ctx, f.head("main").ID); err != nil {
		t.Fatal(err)
	}
	if f.read("a.txt") != "v1" {
		t.Fatal("restore under recovery did not undo the tampered file")
	}
	f.absent("added-by-violation.txt")
	state, _, _ := f.store.GetSubjectState(ctx, f.subject)
	if state.RecoveryRequired {
		t.Fatal("restore did not clear the recovery marker")
	}

	if err = f.store.UpdateSubjectState(ctx, f.subject, func(state *versionstore.SubjectState) { state.RecoveryRequired = true }); err != nil {
		t.Fatal(err)
	}
	f.write("a.txt", "accepted by operator")
	adopted, err := f.ops().Adopt(ctx)
	if err != nil || adopted.Reason != versionstore.SnapshotAdopt || f.head("main").ID != adopted.ID {
		t.Fatalf("adopt = %#v, %v", adopted, err)
	}
	if _, err = f.ops().Adopt(ctx); err == nil {
		t.Fatal("adopt without a recovery marker succeeded")
	}
}
