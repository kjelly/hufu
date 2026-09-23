//go:build !windows

package team

import (
	"context"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// Phase 0 regression tests (docs/archive/implementation-plans/workspace-versioning.md
// §2.1.1): the effect snapshotter must never read a directory symlink as a
// file, never open a special file, and never observe .git content.

func snapshotWithin(t *testing.T, root string) WorkspaceSnapshot {
	t.Helper()
	type result struct {
		snap WorkspaceSnapshot
		err  error
	}
	done := make(chan result, 1)
	go func() {
		snap, err := NewWorkspaceSnapshotter().Snapshot(context.Background(), root)
		done <- result{snap, err}
	}()
	select {
	case got := <-done:
		if got.err != nil {
			t.Fatalf("Snapshot: %v", got.err)
		}
		return got.snap
	case <-time.After(10 * time.Second):
		t.Fatal("Snapshot blocked (it must never open a FIFO)")
		return WorkspaceSnapshot{}
	}
}

func initGitRepo(t *testing.T, root string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git binary unavailable")
	}
	cmd := exec.Command("git", "init", "-q")
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
}

func TestWorkspaceSnapshotWalkSkipsGitDirectory(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".git", "config"), []byte("not a repository"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "kept.txt"), []byte("kept"), 0o644); err != nil {
		t.Fatal(err)
	}
	snap := snapshotWithin(t, root)
	if _, err := snap.fileState(".git/config"); err == nil {
		t.Fatal(".git/config was observed")
	}
	if _, err := snap.fileState("kept.txt"); err != nil {
		t.Fatalf("kept.txt missing: %v", err)
	}
}

func TestWorkspaceSnapshotSymlinkToInternalDirectory(t *testing.T) {
	for _, gitMode := range []bool{false, true} {
		t.Run(map[bool]string{false: "walk", true: "git"}[gitMode], func(t *testing.T) {
			root := t.TempDir()
			if gitMode {
				initGitRepo(t, root)
			}
			if err := os.MkdirAll(filepath.Join(root, "real"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, "real", "file.txt"), []byte("x"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink("real", filepath.Join(root, "link")); err != nil {
				t.Fatal(err)
			}
			snap := snapshotWithin(t, root)
			if _, err := snap.fileState("real/file.txt"); err != nil {
				t.Fatalf("real/file.txt missing: %v", err)
			}
			if _, err := snap.fileState("link"); err == nil {
				t.Fatal("a symlink to a directory was recorded as a file")
			}
		})
	}
}

func TestWorkspaceSnapshotRecordsSpecialFilesWithoutOpening(t *testing.T) {
	tests := []struct {
		name    string
		gitMode bool
		viaLink bool
	}{
		{name: "walk fifo"},
		{name: "walk symlink to fifo", viaLink: true},
		{name: "git symlink to fifo", gitMode: true, viaLink: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			if tt.gitMode {
				initGitRepo(t, root)
			}
			if err := os.WriteFile(filepath.Join(root, "plain.txt"), []byte("p"), 0o644); err != nil {
				t.Fatal(err)
			}
			before := snapshotWithin(t, root)
			if err := syscall.Mkfifo(filepath.Join(root, "pipe"), 0o600); err != nil {
				t.Skipf("mkfifo: %v", err)
			}
			observed := "pipe"
			if tt.viaLink {
				if err := os.Symlink("pipe", filepath.Join(root, "pipe-link")); err != nil {
					t.Fatal(err)
				}
				observed = "pipe-link"
			}
			after := snapshotWithin(t, root)
			state, err := after.fileState(observed)
			if err != nil {
				t.Fatalf("%s missing: %v", observed, err)
			}
			if fs.FileMode(state.Mode)&fs.ModeNamedPipe == 0 || state.Bytes != 0 {
				t.Fatalf("%s state = %#v, want a content-free named-pipe placeholder", observed, state)
			}
			delta, err := NewWorkspaceSnapshotter().Diff(context.Background(), before, after)
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, added := range delta.Added {
				found = found || added.Path == observed
			}
			if !found {
				t.Fatalf("Diff did not report %s as added: %#v", observed, delta)
			}
		})
	}
}
