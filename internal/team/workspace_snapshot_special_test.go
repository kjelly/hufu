//go:build !windows

package team

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestExternalResultGroundingRequiresObservedRegularContent(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside.txt")
	if err := os.WriteFile(outside, []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "deleted-link")); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(root, "deleted-pipe"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "deleted-file"), []byte("observed"), 0o600); err != nil {
		t.Fatal(err)
	}
	before := snapshotWithin(t, root)
	for _, path := range []string{"deleted-link", "deleted-pipe", "deleted-file"} {
		if err := os.Remove(filepath.Join(root, path)); err != nil {
			t.Fatal(err)
		}
	}
	if err := syscall.Mkfifo(filepath.Join(root, "added-pipe"), 0o600); err != nil {
		t.Fatal(err)
	}
	after := snapshotWithin(t, root)
	delta, err := NewWorkspaceSnapshotter().Diff(t.Context(), before, after)
	if err != nil {
		t.Fatal(err)
	}
	req := scopedAttemptRequest("codex", "review")
	req.Task.Execution.RequiresGroundedResult = true
	req.resultWorkspaceBaseline, req.resultWorkspaceSnapshot = &before, &after
	for _, tc := range []struct {
		name, field string
		wantsSource bool
	}{
		{"added FIFO artifact", `"proposed_files":[{"path":"added-pipe"}]`, true},
		{"added FIFO read", `"files_read":["added-pipe"]`, true},
		{"deleted FIFO read", `"files_read":["deleted-pipe"]`, true},
		{"deleted escaping symlink read", `"files_read":["deleted-link"]`, true},
		{"deleted regular file read", `"files_read":["deleted-file"]`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			proposal, err := DecodeWorkerResultProposal([]byte(validProposalJSON(tc.field)))
			if err != nil {
				t.Fatal(err)
			}
			canonical, err := NewExternalResultCanonicalizer().Canonicalize(t.Context(), req, AttemptResult{ResultProposal: proposal}, delta, root)
			if tc.wantsSource {
				if localResultRejectionReason(err) != RepairFailureSource || canonical != nil {
					t.Fatalf("accepted unsupported file content: result=%+v error=%v", canonical, err)
				}
			} else if err != nil || canonical == nil || len(canonical.FilesRead) != 1 || canonical.FilesRead[0].Path != "deleted-file" {
				t.Fatalf("lost the frozen regular file evidence: result=%+v error=%v", canonical, err)
			}
			raw := fmt.Sprintf(`{"status":"success","summary":"",%s}`, tc.field)
			rejected := classifyRejectedCodexResult(t.Context(), req, CodexTurnResult{RawFinalOutput: raw}, delta, root, &CodexProtocolIncompleteError{})
			want := RepairFailureInvalidSchema
			if tc.wantsSource {
				want = RepairFailureSource
			}
			if localResultRejectionReason(rejected) != want {
				t.Fatalf("correction admission reason=%s, want %s: %v", localResultRejectionReason(rejected), want, rejected)
			}
		})
	}
}

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
			if link, err := snap.fileState("link"); err != nil || fs.FileMode(link.Mode) != fs.ModeSymlink || link.Bytes != 0 {
				t.Fatalf("link = %#v, err = %v, want a content-free symlink identity", link, err)
			}
			if _, err := snap.fileState("link/file.txt"); err == nil {
				t.Fatal("snapshot traversed the symlink")
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
			observed, observedType := "pipe", fs.ModeNamedPipe
			if tt.viaLink {
				if err := os.Symlink("pipe", filepath.Join(root, "pipe-link")); err != nil {
					t.Fatal(err)
				}
				observed = "pipe-link"
				observedType = fs.ModeSymlink
			}
			after := snapshotWithin(t, root)
			state, err := after.fileState(observed)
			if err != nil {
				t.Fatalf("%s missing: %v", observed, err)
			}
			if fs.FileMode(state.Mode).Type() != observedType || state.Bytes != 0 {
				t.Fatalf("%s state = %#v, want a content-free %v placeholder", observed, state, observedType)
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
