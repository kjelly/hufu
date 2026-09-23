package versionstore

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

func writeFiles(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for rel, content := range files {
		full := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func baseRequest(root string) CaptureRequest {
	return CaptureRequest{WorkspaceID: "ws_test", BranchID: "main", Root: root, Reason: SnapshotBaseline}
}

func mustCapture(t *testing.T, store *Store, req CaptureRequest) Snapshot {
	t.Helper()
	snapshot, _, err := store.Capture(context.Background(), req)
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	return snapshot
}

func leafPaths(t *testing.T, store *Store, snapshot Snapshot) []string {
	t.Helper()
	leaves, err := store.flattenTree(context.Background(), snapshot.RootTreeHash)
	if err != nil {
		t.Fatal(err)
	}
	paths := make([]string, 0, len(leaves))
	for rel := range leaves {
		paths = append(paths, rel)
	}
	sort.Strings(paths)
	return paths
}

func gitInit(t *testing.T, root string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q")
	run("-c", "user.email=t@example.com", "-c", "user.name=t", "commit", "-q", "--allow-empty", "-m", "init")
}

func TestCaptureRecordsRegularFilesModesAndSymlinks(t *testing.T) {
	store := openTestStore(t)
	root := t.TempDir()
	writeFiles(t, root, map[string]string{"a.txt": "alpha", "dir/nested/b.txt": "beta", "run.sh": "#!/bin/sh"})
	if err := os.Chmod(filepath.Join(root, "run.sh"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("a.txt", filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "empty"), 0o755); err != nil {
		t.Fatal(err)
	}
	snapshot := mustCapture(t, store, baseRequest(root))
	if snapshot.State != StatePending || !snapshot.Materializable || snapshot.FileCount != 4 || snapshot.LogicalBytes != int64(len("alpha")+len("beta")+len("#!/bin/sh")) {
		t.Fatalf("snapshot = %#v", snapshot)
	}
	leaves, err := store.flattenTree(context.Background(), snapshot.RootTreeHash)
	if err != nil {
		t.Fatal(err)
	}
	if got := leaves["run.sh"]; got.Kind != EntryBlob || got.Mode != 0o755 {
		t.Fatalf("run.sh leaf = %#v", got)
	}
	if got := leaves["link"]; got.Kind != EntrySymlink || got.Hash != sha("a.txt") || got.Size != 5 {
		t.Fatalf("link leaf = %#v", got)
	}
	if got := leaves["a.txt"]; got.Hash != sha("alpha") {
		t.Fatalf("a.txt hash = %s", got.Hash)
	}
	if paths := leafPaths(t, store, snapshot); len(paths) != 4 {
		t.Fatalf("paths = %v (empty directory must not be captured)", paths)
	}
	stored, err := store.GetSnapshot(context.Background(), snapshot.ID)
	if err != nil || stored.RootTreeHash != snapshot.RootTreeHash || stored.State != StatePending {
		t.Fatalf("GetSnapshot = %#v, %v", stored, err)
	}
	if err = store.Verify(context.Background(), snapshot.ID); err != nil {
		t.Fatalf("Verify: %v", err)
	}
}

func TestCaptureEmptyWorkspaceAndIdenticalContent(t *testing.T) {
	store := openTestStore(t)
	root := t.TempDir()
	empty := mustCapture(t, store, baseRequest(root))
	if empty.FileCount != 0 {
		t.Fatalf("empty workspace captured %d files", empty.FileCount)
	}
	writeFiles(t, root, map[string]string{"x": "same"})
	first := mustCapture(t, store, baseRequest(root))
	second := mustCapture(t, store, baseRequest(root))
	if first.ID == second.ID || first.RootTreeHash != second.RootTreeHash {
		t.Fatalf("identical content: ids %s/%s trees %s/%s", first.ID, second.ID, first.RootTreeHash, second.RootTreeHash)
	}
}

func TestCaptureClassifiesSymlinkSafety(t *testing.T) {
	outside := t.TempDir()
	tests := []struct {
		name         string
		target       string
		materialized bool
	}{
		{name: "internal relative", target: "file.txt", materialized: true},
		{name: "internal directory", target: "sub", materialized: true},
		{name: "dangling internal", target: "missing.txt", materialized: true},
		{name: "escaping relative", target: "../../outside", materialized: false},
		{name: "escaping absolute", target: outside, materialized: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := openTestStore(t)
			root := t.TempDir()
			writeFiles(t, root, map[string]string{"file.txt": "x", "sub/inner.txt": "y"})
			if err := os.Symlink(tt.target, filepath.Join(root, "link")); err != nil {
				t.Fatal(err)
			}
			snapshot := mustCapture(t, store, baseRequest(root))
			if snapshot.Materializable != tt.materialized {
				t.Fatalf("Materializable = %v, want %v", snapshot.Materializable, tt.materialized)
			}
		})
	}
}

func TestCaptureRejectsSpecialFilesWithoutBlocking(t *testing.T) {
	store := openTestStore(t)
	root := t.TempDir()
	if err := syscall.Mkfifo(filepath.Join(root, "pipe"), 0o600); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	_, _, err := store.Capture(context.Background(), baseRequest(root))
	if !errors.Is(err, ErrUnsupportedFileType) {
		t.Fatalf("err = %v, want ErrUnsupportedFileType", err)
	}
}

func TestCaptureEnforcesLimits(t *testing.T) {
	tests := []struct {
		name   string
		limits CaptureLimits
	}{
		{name: "file count", limits: CaptureLimits{MaxFiles: 1}},
		{name: "file bytes", limits: CaptureLimits{MaxFileBytes: 3}},
		{name: "logical bytes", limits: CaptureLimits{MaxLogicalBytes: 5}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := openTestStore(t)
			root := t.TempDir()
			writeFiles(t, root, map[string]string{"a": "1234", "b": "5678"})
			req := baseRequest(root)
			req.Limits = tt.limits
			if _, _, err := store.Capture(context.Background(), req); !errors.Is(err, ErrCaptureLimitExceeded) {
				t.Fatalf("err = %v, want ErrCaptureLimitExceeded", err)
			}
		})
	}
}

func TestCaptureInclusionPolicy(t *testing.T) {
	store := openTestStore(t)
	root := t.TempDir()
	writeFiles(t, root, map[string]string{
		"docs/history/a.md":     "a",
		"internal/tasks/b.go":   "b",
		"logs/c.txt":            "c",
		"shared/d":              "d",
		"status/e":              "e",
		".git/config":           "git",
		"control/session.json":  "control",
		"node_modules/pkg/x.js": "dep",
		"build.log":             "log",
		".hufuignore":           "node_modules/\n*.log\n",
		"keep.txt":              "keep",
		".hufu-materialize-ab":  "stale temp",
	})
	req := baseRequest(root)
	req.ExcludeSubtrees = []string{"control"}
	snapshot := mustCapture(t, store, req)
	got := leafPaths(t, store, snapshot)
	want := []string{".hufuignore", "docs/history/a.md", "internal/tasks/b.go", "keep.txt", "logs/c.txt", "shared/d", "status/e"}
	if len(got) != len(want) {
		t.Fatalf("paths = %v, want %v", got, want)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("paths = %v, want %v", got, want)
		}
	}
}

func TestCaptureRequireHufuignore(t *testing.T) {
	store := openTestStore(t)
	root := t.TempDir()
	writeFiles(t, root, map[string]string{"a": "1"})
	req := baseRequest(root)
	req.RequireHufuignore = true
	if _, _, err := store.Capture(context.Background(), req); !errors.Is(err, ErrHufuignoreRequired) {
		t.Fatalf("err = %v, want ErrHufuignoreRequired", err)
	}
	writeFiles(t, root, map[string]string{".hufuignore": ""})
	if _, _, err := store.Capture(context.Background(), req); err != nil {
		t.Fatalf("empty .hufuignore should satisfy the requirement: %v", err)
	}
}

func TestCaptureGitCandidates(t *testing.T) {
	store := openTestStore(t)
	root := t.TempDir()
	gitInit(t, root)
	writeFiles(t, root, map[string]string{
		".gitignore":          "ignored/\n*.tmp\n",
		"tracked.txt":         "t",
		"untracked.txt":       "u",
		"ignored/secret":      "s",
		"scratch.tmp":         "x",
		"logs/kept.txt":       "k",
		"nested/repo/file.go": "n",
	})
	gitInit(t, filepath.Join(root, "nested", "repo"))
	writeFiles(t, root, map[string]string{"half/repo/.git/HEAD": "ref: refs/heads/main\n", "half/repo/x.go": "h"})
	cmd := exec.Command("git", "add", "tracked.txt", ".gitignore")
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git add: %v\n%s", err, out)
	}
	if !IsGitWorkTree(context.Background(), root) {
		t.Fatal("expected a Git work tree")
	}
	snapshot := mustCapture(t, store, baseRequest(root))
	got := leafPaths(t, store, snapshot)
	want := []string{".gitignore", "logs/kept.txt", "tracked.txt", "untracked.txt"}
	if len(got) != len(want) {
		t.Fatalf("paths = %v, want %v", got, want)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("paths = %v, want %v", got, want)
		}
	}
}

func TestCaptureRejectsUnpublishedParent(t *testing.T) {
	store := openTestStore(t)
	root := t.TempDir()
	pending := mustCapture(t, store, baseRequest(root))
	req := baseRequest(root)
	req.Parent = pending.ID
	if _, _, err := store.Capture(context.Background(), req); !errors.Is(err, ErrWorkspaceSnapshotUnavailable) {
		t.Fatalf("err = %v, want ErrWorkspaceSnapshotUnavailable", err)
	}
	req.Parent = "wsv_" + SnapshotID(hashA[:32])
	if _, _, err := store.Capture(context.Background(), req); !errors.Is(err, ErrSnapshotNotFound) {
		t.Fatalf("err = %v, want ErrSnapshotNotFound", err)
	}
}

func TestCaptureStatCacheHitsAndMisses(t *testing.T) {
	ctx := context.Background()
	future := time.Now().Add(time.Hour)
	store, err := Open(ctx, Options{StateDir: t.TempDir(), Now: func() time.Time { return future }})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	hits := &atomic.Int64{}
	store.statCacheHits = hits
	root := t.TempDir()
	writeFiles(t, root, map[string]string{"a.txt": "one", "b.txt": "two"})
	first := mustCapture(t, store, baseRequest(root))
	if hits.Load() != 0 {
		t.Fatalf("first capture had %d cache hits", hits.Load())
	}
	second := mustCapture(t, store, baseRequest(root))
	if hits.Load() != 2 || second.RootTreeHash != first.RootTreeHash {
		t.Fatalf("second capture hits = %d, tree %s vs %s", hits.Load(), second.RootTreeHash, first.RootTreeHash)
	}
	// Same size, restored mtime: ctime still changes, so the cache must miss
	// and the new bytes must be captured.
	path := filepath.Join(root, "a.txt")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, []byte("ONE"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err = os.Chtimes(path, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	third := mustCapture(t, store, baseRequest(root))
	leaves, err := store.flattenTree(ctx, third.RootTreeHash)
	if err != nil {
		t.Fatal(err)
	}
	if leaves["a.txt"].Hash != sha("ONE") {
		t.Fatal("stat cache hid a same-size, same-mtime edit")
	}
}

func mustCanonical(t *testing.T, root string) string {
	t.Helper()
	canonical, err := CanonicalRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	return canonical
}

func TestCaptureIfChangedSkipsUnchangedTrees(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	root := t.TempDir()
	writeFiles(t, root, map[string]string{"a": "1"})
	head := mustCapture(t, store, baseRequest(root))
	before, _ := store.ListSnapshots(ctx, SnapshotFilter{})
	_, changed, _, err := store.CaptureIfChanged(ctx, baseRequest(root), head.RootTreeHash)
	if err != nil || changed {
		t.Fatalf("unchanged capture: changed=%v err=%v", changed, err)
	}
	if after, _ := store.ListSnapshots(ctx, SnapshotFilter{}); len(after) != len(before) {
		t.Fatalf("unchanged capture wrote a row: %d -> %d", len(before), len(after))
	}
	writeFiles(t, root, map[string]string{"a": "2"})
	drift, changed, _, err := store.CaptureIfChanged(ctx, baseRequest(root), head.RootTreeHash)
	if err != nil || !changed || drift.RootTreeHash == head.RootTreeHash || drift.State != StatePending {
		t.Fatalf("drift capture = %#v changed=%v err=%v", drift, changed, err)
	}
}
