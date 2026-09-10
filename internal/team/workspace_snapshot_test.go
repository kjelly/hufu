package team

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// Phase 2 tests (spec.md §36 PR-05): workspace snapshot/delta.

func mustSnapshot(t *testing.T, snapshotter WorkspaceSnapshotter, root string) WorkspaceSnapshot {
	t.Helper()
	snap, err := snapshotter.Snapshot(context.Background(), root)
	if err != nil {
		t.Fatalf("Snapshot(%q): %v", root, err)
	}
	return snap
}

// TestWorkspaceDeltaAddedModifiedDeleted proves Diff correctly classifies
// every case across two attempts of the same non-Git workspace.
func TestWorkspaceDeltaAddedModifiedDeleted(t *testing.T) {
	root := t.TempDir()
	writeFile := func(rel, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(root, rel), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeFile("kept.txt", "unchanged")
	writeFile("modified.txt", "before")

	snapshotter := NewWorkspaceSnapshotter()
	baseline := mustSnapshot(t, snapshotter, root)

	writeFile("modified.txt", "after")
	writeFile("added.txt", "new file")

	afterAdd := mustSnapshot(t, snapshotter, root)
	delta, err := snapshotter.Diff(context.Background(), baseline, afterAdd)
	if err != nil {
		t.Fatal(err)
	}
	if len(delta.Added) != 1 || delta.Added[0].Path != "added.txt" {
		t.Fatalf("Added = %#v, want exactly added.txt", delta.Added)
	}
	if len(delta.Modified) != 1 || delta.Modified[0].Path != "modified.txt" {
		t.Fatalf("Modified = %#v, want exactly modified.txt", delta.Modified)
	}
	if len(delta.Deleted) != 0 {
		t.Fatalf("Deleted = %#v, want none yet", delta.Deleted)
	}

	if err := os.Remove(filepath.Join(root, "kept.txt")); err != nil {
		t.Fatal(err)
	}
	afterDelete := mustSnapshot(t, snapshotter, root)
	delta, err = snapshotter.Diff(context.Background(), afterAdd, afterDelete)
	if err != nil {
		t.Fatal(err)
	}
	if len(delta.Deleted) != 1 || delta.Deleted[0] != "kept.txt" {
		t.Fatalf("Deleted = %#v, want exactly kept.txt", delta.Deleted)
	}
	if len(delta.Added) != 0 || len(delta.Modified) != 0 {
		t.Fatalf("delta = %#v, want only a deletion", delta)
	}
}

// TestWorkspaceSnapshotToleratesPreexistingSymlinkEscape proves a symlink
// whose target resolves outside the workspace root does not abort the whole
// snapshot: a real repository can already contain one, unrelated to any
// attempt (e.g. another local tool's own runtime artifact), and that alone
// must not make every attempt against that repository fail before it can
// even start. Its content is never read or hashed — a stable placeholder
// identity is recorded instead, never the outside-root bytes — and an
// unchanged escaping symlink present in both snapshots produces no diff
// (§11.4, §SEC-08's "MUST NOT be accepted as result artifacts" is enforced
// at the artifact-canonicalization boundary, not by refusing to observe the
// workspace at all — see TestWorkspaceDeltaDetectsNewSymlinkEscape below for
// the case §SEC-08 actually guards against).
func TestWorkspaceSnapshotToleratesPreexistingSymlinkEscape(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(secret, []byte("outside the workspace"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(root, "escape.txt")); err != nil {
		t.Skipf("symlinks unavailable in this environment: %v", err)
	}

	before := mustSnapshot(t, NewWorkspaceSnapshotter(), root)
	after := mustSnapshot(t, NewWorkspaceSnapshotter(), root)
	delta, err := NewWorkspaceSnapshotter().Diff(context.Background(), before, after)
	if err != nil {
		t.Fatal(err)
	}
	if len(delta.Added) != 0 || len(delta.Modified) != 0 || len(delta.Deleted) != 0 {
		t.Fatalf("delta = %#v, want no changes for an unchanged pre-existing escaping symlink", delta)
	}
}

// TestWorkspaceDeltaDetectsNewSymlinkEscape proves the security-relevant
// signal §SEC-08/§10.4 actually care about is preserved: a symlink escaping
// the workspace root that appears during an attempt (absent from baseline,
// present after) still shows up as Added, exactly like any other new file —
// tolerating a pre-existing one does not mean losing the ability to notice
// a newly created one.
func TestWorkspaceDeltaDetectsNewSymlinkEscape(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(secret, []byte("outside the workspace"), 0o644); err != nil {
		t.Fatal(err)
	}

	before := mustSnapshot(t, NewWorkspaceSnapshotter(), root)
	if err := os.Symlink(secret, filepath.Join(root, "escape.txt")); err != nil {
		t.Skipf("symlinks unavailable in this environment: %v", err)
	}
	after := mustSnapshot(t, NewWorkspaceSnapshotter(), root)

	delta, err := NewWorkspaceSnapshotter().Diff(context.Background(), before, after)
	if err != nil {
		t.Fatal(err)
	}
	if len(delta.Added) != 1 || delta.Added[0].Path != "escape.txt" {
		t.Fatalf("delta.Added = %#v, want exactly the newly created escaping symlink", delta.Added)
	}
}

// TestWorkspaceDeltaHashesActualBytes proves WorkspaceFileState.SHA256 is
// computed from the real file bytes, not trusted from any other source
// (§11.3: "Hufu SHALL hash actual file bytes itself").
func TestWorkspaceDeltaHashesActualBytes(t *testing.T) {
	root := t.TempDir()
	content := []byte("exact content whose hash must match independently")
	if err := os.WriteFile(filepath.Join(root, "file.txt"), content, 0o644); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(content)
	want := hex.EncodeToString(sum[:])

	snap := mustSnapshot(t, NewWorkspaceSnapshotter(), root)
	got, err := snap.fileState("file.txt")
	if err != nil {
		t.Fatal(err)
	}
	if got.SHA256 != want {
		t.Fatalf("SHA256 = %q, want %q (independently computed)", got.SHA256, want)
	}
	if got.Bytes != int64(len(content)) {
		t.Fatalf("Bytes = %d, want %d", got.Bytes, len(content))
	}
}

// TestWorkspaceSnapshotFallbackNonGit proves the non-Git directory-walk
// fallback exists and applies its own skip rules (Hufu-internal directories),
// which only matter for this path — a Git-optimized snapshot instead relies
// on Git's own ignore rules (§11.4).
func TestWorkspaceSnapshotFallbackNonGit(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "real.txt"), []byte("real"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, logsDir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, logsDir, "internal.txt"), []byte("internal"), 0o644); err != nil {
		t.Fatal(err)
	}

	snap := mustSnapshot(t, NewWorkspaceSnapshotter(), root)
	if _, err := snap.fileState("real.txt"); err != nil {
		t.Fatalf("real.txt missing from fallback snapshot: %v", err)
	}
	if _, err := snap.fileState(logsDir + "/internal.txt"); err == nil {
		t.Fatalf("Hufu-internal %s/ directory was not skipped by the fallback walker", logsDir)
	}
}

func TestWorkspaceSnapshotKeepsOrdinarySessionJSONFailClosed(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "session.json"), []byte(`{"status":"before"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	snapshotter := NewWorkspaceSnapshotter()
	baseline := mustSnapshot(t, snapshotter, root)
	if _, err := baseline.fileState("session.json"); err != nil {
		t.Fatalf("ordinary project session.json was omitted from the workspace snapshot: %v", err)
	}

	if err := os.WriteFile(filepath.Join(root, "session.json"), []byte(`{"status":"after"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	after := mustSnapshot(t, snapshotter, root)
	delta, err := snapshotter.Diff(context.Background(), baseline, after)
	if err != nil {
		t.Fatal(err)
	}
	if len(delta.Modified) != 1 || delta.Modified[0].Path != "session.json" {
		t.Fatalf("delta = %#v, want ordinary session.json modification", delta)
	}
	if err := ValidateExecutionWorldDelta(&PreparedExecutionWorld{Root: root}, delta); err == nil {
		t.Fatal("ordinary session.json change was not rejected for a read-only prepared world")
	}
}

func TestExecutionWorldSkipsIdentifiedControlWorkspaceCheckpoint(t *testing.T) {
	root := t.TempDir()
	controlWorkspace := filepath.Join(root, "workspace", "hufu-code-review")
	if err := os.MkdirAll(controlWorkspace, 0o755); err != nil {
		t.Fatal(err)
	}
	checkpoint := filepath.Join(controlWorkspace, sessionFile)
	if err := os.WriteFile(checkpoint, []byte(`{"status":"before"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	world := NewLocalExecutionWorld()
	prepared, err := world.Prepare(t.Context(), ExecutionWorldSpec{
		Root: root, ControlWorkspace: controlWorkspace, SideEffect: SideEffectNone,
	})
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	t.Cleanup(func() { _ = world.Release(context.Background(), prepared) })
	if _, err := prepared.Baseline.fileState("workspace/hufu-code-review/session.json"); err == nil {
		t.Fatal("identified Hufu control-workspace checkpoint was included in the provider snapshot")
	}

	if err := os.WriteFile(checkpoint, []byte(`{"status":"after"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "source.go"), []byte("package review\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	after, err := world.Snapshot(context.Background(), prepared)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	delta, err := NewWorkspaceSnapshotter().Diff(context.Background(), prepared.Baseline, after)
	if err != nil {
		t.Fatal(err)
	}
	if len(delta.Added) != 1 || delta.Added[0].Path != "source.go" {
		t.Fatalf("delta = %#v, want only source.go added; identified Hufu checkpoint is bookkeeping", delta)
	}
	if err := ValidateExecutionWorldDelta(prepared, delta); err == nil {
		t.Fatal("source.go change was not rejected for a read-only prepared world")
	}
}

// TestWorkspaceSnapshotGitOptimizedMatchesFallback is supplementary (not one
// of PR-05's named tests): it exercises the Git-assisted candidate-discovery
// path added alongside the required fallback, proving it produces the same
// real-byte hashes for the same content. See
// TestWorkspaceSnapshotGitOptimizedSkipsInternalDirs below for the
// Hufu-internal-directory exclusion this path also applies explicitly, not
// merely by relying on Git's own (optional, absent-by-default) ignore rules.
func TestWorkspaceSnapshotGitOptimizedMatchesFallback(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git binary unavailable")
	}
	root := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q")
	run("config", "user.email", "test@example.com")
	run("config", "user.name", "test")
	content := []byte("tracked file content")
	if err := os.WriteFile(filepath.Join(root, "tracked.txt"), content, 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "tracked.txt")
	run("commit", "-q", "-m", "initial")
	if err := os.WriteFile(filepath.Join(root, "untracked.txt"), []byte("untracked"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, ok := gitCandidateFiles(context.Background(), root); !ok {
		t.Fatal("expected root to be detected as a Git working tree")
	}
	snap := mustSnapshot(t, NewWorkspaceSnapshotter(), root)
	sum := sha256.Sum256(content)
	got, err := snap.fileState("tracked.txt")
	if err != nil {
		t.Fatal(err)
	}
	if got.SHA256 != hex.EncodeToString(sum[:]) {
		t.Fatalf("tracked.txt SHA256 = %q, want the real content hash", got.SHA256)
	}
	if _, err := snap.fileState("untracked.txt"); err != nil {
		t.Fatalf("untracked.txt missing from git-optimized snapshot: %v", err)
	}
}

// TestWorkspaceSnapshotGitOptimizedSkipsInternalDirs proves the git-assisted
// path applies the same Hufu-internal exclusions as the non-Git fallback
// (TestWorkspaceSnapshotFallbackNonGit), not just Git's own .gitignore
// rules.
//
// **Fixed 2026-09-08** (found running the real §38 smoke suite for the first
// time, against a genuine git-initialized scratch repository — every
// fake-server unit test's workspace is a plain non-Git temp dir, so none of
// them ever exercised this path at all): gitCandidateFiles previously had no
// such exclusion. A real Coordinator's own logs/event_store.jsonl, sitting
// untracked inside the same git-backed workspace an attempt operates in, was
// a legitimate git "untracked file" candidate — so any append Hufu's own
// EventStore made to it during a real attempt was indistinguishable from a
// provider write, and a read-only task (empty WritableRoots) failed closed
// on its own coordinator's bookkeeping.
func TestWorkspaceSnapshotGitOptimizedSkipsInternalDirs(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git binary unavailable")
	}
	root := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q")
	run("config", "user.email", "test@example.com")
	run("config", "user.name", "test")
	if err := os.WriteFile(filepath.Join(root, "tracked.txt"), []byte("tracked"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "tracked.txt")
	run("commit", "-q", "-m", "initial")

	// No .gitignore for logsDir at all: the exclusion must not depend on it.
	if err := os.MkdirAll(filepath.Join(root, logsDir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, logsDir, "event_store.jsonl"), []byte(""), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "session.json"), []byte(`{"status":"before"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, ok := gitCandidateFiles(context.Background(), root); !ok {
		t.Fatal("expected root to be detected as a Git working tree")
	}
	baseline := mustSnapshot(t, NewWorkspaceSnapshotter(), root)
	if _, err := baseline.fileState(logsDir + "/event_store.jsonl"); err == nil {
		t.Fatalf("Hufu-internal %s/ directory was not skipped by the git-assisted snapshot", logsDir)
	}
	if _, err := baseline.fileState("session.json"); err != nil {
		t.Fatalf("ordinary root session.json was omitted by the git-assisted snapshot: %v", err)
	}

	if err := os.WriteFile(filepath.Join(root, logsDir, "event_store.jsonl"), []byte(`{"an":"event"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "session.json"), []byte(`{"status":"after"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	after := mustSnapshot(t, NewWorkspaceSnapshotter(), root)
	delta, err := NewWorkspaceSnapshotter().Diff(context.Background(), baseline, after)
	if err != nil {
		t.Fatal(err)
	}
	if len(delta.Added) != 0 || len(delta.Modified) != 1 || delta.Modified[0].Path != "session.json" || len(delta.Deleted) != 0 {
		t.Fatalf("delta = %#v, want only ordinary session.json modification; Hufu's own %s/ write must stay excluded", delta, logsDir)
	}
}
