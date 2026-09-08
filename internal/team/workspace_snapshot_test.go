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

// TestWorkspaceSnapshotRejectsSymlinkEscape proves a symlink whose target
// resolves outside the workspace root fails the whole snapshot rather than
// being silently skipped or, worse, accepted (§11.4, §SEC-08).
func TestWorkspaceSnapshotRejectsSymlinkEscape(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(secret, []byte("outside the workspace"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(root, "escape.txt")); err != nil {
		t.Skipf("symlinks unavailable in this environment: %v", err)
	}

	if _, err := NewWorkspaceSnapshotter().Snapshot(context.Background(), root); err == nil {
		t.Fatal("expected Snapshot to reject a symlink escaping the workspace root")
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

// TestWorkspaceSnapshotGitOptimizedMatchesFallback is supplementary (not one
// of PR-05's named tests): it exercises the Git-assisted candidate-discovery
// path added alongside the required fallback, proving it produces the same
// real-byte hashes for the same content.
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
