package team

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
)

type isolatedWorldFixture struct {
	subject, control string
	world            *IsolatedCopyExecutionWorld
}

func newIsolatedWorldFixture(t *testing.T, files map[string]string) isolatedWorldFixture {
	t.Helper()
	f := isolatedWorldFixture{subject: t.TempDir(), control: t.TempDir(), world: NewIsolatedCopyExecutionWorld()}
	for rel, content := range files {
		writeFixtureFile(t, f.subject, rel, content)
	}
	return f
}

func writeFixtureFile(t *testing.T, root, rel, content string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readFixtureFile(t *testing.T, root, rel string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func (f isolatedWorldFixture) prepare(t *testing.T) *PreparedExecutionWorld {
	t.Helper()
	prepared, err := f.world.Prepare(context.Background(), ExecutionWorldSpec{
		RunID: "run-1", TaskID: "7", Attempt: 1, OccurrenceAttempt: 1, Root: f.subject, ControlWorkspace: f.control,
	})
	if err != nil {
		t.Fatal(err)
	}
	return prepared
}

func (f isolatedWorldFixture) delta(t *testing.T, prepared *PreparedExecutionWorld) *AttemptWorkspaceDelta {
	t.Helper()
	delta, err := f.world.Delta(context.Background(), prepared)
	if err != nil {
		t.Fatal(err)
	}
	return delta
}

func changeOps(delta *AttemptWorkspaceDelta) map[string]string {
	ops := make(map[string]string, len(delta.Changes))
	for _, change := range delta.Changes {
		ops[change.Path] = change.Op
	}
	return ops
}

func TestIsolatedWorldPrepareCopiesProjectAndOwnsItsDirectory(t *testing.T) {
	f := newIsolatedWorldFixture(t, map[string]string{"main.go": "package main\n", "docs/readme.md": "hello\n"})
	if err := os.Symlink("docs/readme.md", filepath.Join(f.subject, "readme-link")); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	prepared := f.prepare(t)
	if !strings.HasPrefix(prepared.Root, f.control) || strings.HasPrefix(prepared.Root, f.subject) {
		t.Fatalf("world root %s is not under the control root", prepared.Root)
	}
	if got := readFixtureFile(t, prepared.Root, "docs/readme.md"); got != "hello\n" {
		t.Fatalf("copied file = %q", got)
	}
	if target, err := os.Readlink(filepath.Join(prepared.Root, "readme-link")); err != nil || target != "docs/readme.md" {
		t.Fatalf("copied symlink = %q, %v", target, err)
	}
	owner, err := readAttemptWorldOwner(filepath.Dir(prepared.Root))
	if err != nil || owner.WorldID != prepared.ID || owner.TaskID != "7" || owner.SourceRoot == "" {
		t.Fatalf("owner = %#v, %v", owner, err)
	}
	if !strings.HasPrefix(prepared.ID, "aw-") || len(prepared.ID) != len("aw-")+16 {
		t.Fatalf("world id %q is not a nonce", prepared.ID)
	}
	other := f.prepare(t)
	if other.ID == prepared.ID {
		t.Fatal("two worlds shared an id")
	}
	if err := f.world.Release(context.Background(), prepared); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Dir(prepared.Root)); !os.IsNotExist(err) {
		t.Fatalf("released world still exists: %v", err)
	}
}

func TestIsolatedWorldPrepareRejectsUnsupportedLayouts(t *testing.T) {
	tests := []struct {
		name  string
		setup func(t *testing.T, f *isolatedWorldFixture)
		want  string
	}{
		{name: "escaping symlink", setup: func(t *testing.T, f *isolatedWorldFixture) {
			if err := os.Symlink("/etc/passwd", filepath.Join(f.subject, "escape")); err != nil {
				t.Skipf("symlink unsupported: %v", err)
			}
		}, want: "points outside the project"},
		{name: "parent traversal symlink", setup: func(t *testing.T, f *isolatedWorldFixture) {
			if err := os.Symlink("../outside", filepath.Join(f.subject, "up")); err != nil {
				t.Skipf("symlink unsupported: %v", err)
			}
		}, want: "points outside the project"},
		{name: "fifo", setup: func(t *testing.T, f *isolatedWorldFixture) {
			if err := syscall.Mkfifo(filepath.Join(f.subject, "pipe"), 0o644); err != nil {
				t.Skipf("mkfifo unsupported: %v", err)
			}
		}, want: "special file"},
		{name: "control root inside project", setup: func(t *testing.T, f *isolatedWorldFixture) {
			f.control = filepath.Join(f.subject, ".hufu")
			if err := os.MkdirAll(f.control, 0o755); err != nil {
				t.Fatal(err)
			}
		}, want: workspaceIsolationUnsupportedCode},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newIsolatedWorldFixture(t, map[string]string{"a.txt": "a\n"})
			tt.setup(t, &f)
			_, err := f.world.Prepare(context.Background(), ExecutionWorldSpec{RunID: "r", TaskID: "1", Attempt: 1, Root: f.subject, ControlWorkspace: f.control})
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Prepare error = %v, want %q", err, tt.want)
			}
			entries, _ := os.ReadDir(filepath.Join(f.control, attemptWorldsDirName))
			if len(entries) != 0 {
				t.Fatalf("a failed Prepare left %d world(s) behind", len(entries))
			}
		})
	}
}

func TestIsolatedWorldDeltaIsKindAwareAndExcludesGitAndIgnored(t *testing.T) {
	f := newIsolatedWorldFixture(t, map[string]string{
		"keep.txt": "keep\n", "edit.txt": "old\n", "gone/remove.txt": "bye\n", "kind.txt": "file\n",
		".hufuignore": "build/\n*.log\n",
	})
	prepared := f.prepare(t)
	root := prepared.Root
	writeFixtureFile(t, root, "edit.txt", "new\n")
	writeFixtureFile(t, root, "added/new.txt", "fresh\n")
	writeFixtureFile(t, root, "build/out.bin", "artifact\n")
	writeFixtureFile(t, root, "debug.log", "noise\n")
	writeFixtureFile(t, root, "sub/.git/config", "[core]\n")
	if err := os.Remove(filepath.Join(root, "gone", "remove.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, "kind.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("keep.txt", filepath.Join(root, "kind.txt")); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	delta := f.delta(t, prepared)
	want := map[string]string{"edit.txt": AttemptPathModify, "added/new.txt": AttemptPathAdd, "gone/remove.txt": AttemptPathDelete, "kind.txt": AttemptPathModify}
	got := changeOps(delta)
	if len(got) != len(want) {
		t.Fatalf("changes = %v, want %v", got, want)
	}
	for rel, op := range want {
		if got[rel] != op {
			t.Fatalf("change %s = %q, want %q (all: %v)", rel, got[rel], op, got)
		}
	}
	again := f.delta(t, prepared)
	if again.Digest != delta.Digest {
		t.Fatalf("delta digest is not deterministic: %s vs %s", again.Digest, delta.Digest)
	}
}

func TestIsolatedWorldDeltaRejectsEscapesAndSpecialFiles(t *testing.T) {
	tests := []struct {
		name  string
		setup func(t *testing.T, root string)
	}{
		{name: "escaping symlink", setup: func(t *testing.T, root string) {
			if err := os.Symlink("../../../../etc/passwd", filepath.Join(root, "escape")); err != nil {
				t.Skipf("symlink unsupported: %v", err)
			}
		}},
		{name: "fifo", setup: func(t *testing.T, root string) {
			if err := syscall.Mkfifo(filepath.Join(root, "pipe"), 0o644); err != nil {
				t.Skipf("mkfifo unsupported: %v", err)
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newIsolatedWorldFixture(t, map[string]string{"a.txt": "a\n"})
			prepared := f.prepare(t)
			tt.setup(t, prepared.Root)
			if _, err := f.world.Delta(context.Background(), prepared); err == nil || !strings.Contains(err.Error(), workspaceDeltaRejectedCode) {
				t.Fatalf("Delta error = %v, want %s", err, workspaceDeltaRejectedCode)
			}
		})
	}
}

func TestApplyAttemptWorkspaceDelta(t *testing.T) {
	f := newIsolatedWorldFixture(t, map[string]string{"keep.txt": "keep\n", "edit.txt": "old\n", "gone/remove.txt": "bye\n"})
	prepared := f.prepare(t)
	writeFixtureFile(t, prepared.Root, "edit.txt", "new\n")
	writeFixtureFile(t, prepared.Root, "added/new.txt", "fresh\n")
	if err := os.Remove(filepath.Join(prepared.Root, "gone", "remove.txt")); err != nil {
		t.Fatal(err)
	}
	if got := readFixtureFile(t, f.subject, "edit.txt"); got != "old\n" {
		t.Fatalf("the canonical project changed before apply: %q", got)
	}
	delta := f.delta(t, prepared)
	result, err := applyAttemptWorkspaceDelta(context.Background(), f.subject, prepared.Root, delta)
	if err != nil {
		t.Fatal(err)
	}
	if result.FilesWritten != 2 || result.FilesDeleted != 1 {
		t.Fatalf("apply result = %#v", result)
	}
	if readFixtureFile(t, f.subject, "edit.txt") != "new\n" || readFixtureFile(t, f.subject, "added/new.txt") != "fresh\n" {
		t.Fatal("apply did not write the final state")
	}
	if _, err := os.Stat(filepath.Join(f.subject, "gone")); !os.IsNotExist(err) {
		t.Fatalf("the directory emptied by a delete still exists: %v", err)
	}
	entries, _ := os.ReadDir(f.subject)
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".hufu-materialize-") {
			t.Fatalf("apply left a temp file %s", entry.Name())
		}
	}
	// Re-running converges without writing anything.
	again, err := applyAttemptWorkspaceDelta(context.Background(), f.subject, prepared.Root, delta)
	if err != nil || again.FilesWritten != 0 || again.FilesDeleted != 0 {
		t.Fatalf("re-entrant apply = %#v, %v", again, err)
	}
}

func TestApplyAttemptWorkspaceDeltaConflictWritesNothing(t *testing.T) {
	f := newIsolatedWorldFixture(t, map[string]string{"a.txt": "a\n", "b.txt": "b\n"})
	prepared := f.prepare(t)
	writeFixtureFile(t, prepared.Root, "a.txt", "attempt a\n")
	writeFixtureFile(t, prepared.Root, "b.txt", "attempt b\n")
	delta := f.delta(t, prepared)
	writeFixtureFile(t, f.subject, "b.txt", "someone else\n")
	_, err := applyAttemptWorkspaceDelta(context.Background(), f.subject, prepared.Root, delta)
	var conflict *AttemptWorkspaceConflictError
	if !errors.As(err, &conflict) || len(conflict.Paths) != 1 || conflict.Paths[0] != "b.txt" {
		t.Fatalf("apply error = %v, want a conflict on b.txt", err)
	}
	if got := readFixtureFile(t, f.subject, "a.txt"); got != "a\n" {
		t.Fatalf("a conflicting apply wrote a non-conflicting path: %q", got)
	}
}

func TestApplyAttemptWorkspaceDeltaRejectsWorldEditedAfterDelta(t *testing.T) {
	f := newIsolatedWorldFixture(t, map[string]string{"a.txt": "a\n"})
	prepared := f.prepare(t)
	writeFixtureFile(t, prepared.Root, "a.txt", "reviewed\n")
	delta := f.delta(t, prepared)
	writeFixtureFile(t, prepared.Root, "a.txt", "unreviewed\n")
	if _, err := applyAttemptWorkspaceDelta(context.Background(), f.subject, prepared.Root, delta); err == nil || !strings.Contains(err.Error(), workspaceApplyIncompleteCode) {
		t.Fatalf("apply error = %v, want %s", err, workspaceApplyIncompleteCode)
	}
	if got := readFixtureFile(t, f.subject, "a.txt"); got != "a\n" {
		t.Fatalf("unreviewed content reached the project: %q", got)
	}
}

func TestParallelIsolatedWorldsApply(t *testing.T) {
	f := newIsolatedWorldFixture(t, map[string]string{"shared.txt": "base\n", "left.txt": "l\n", "right.txt": "r\n"})
	left, right := f.prepare(t), f.prepare(t)
	writeFixtureFile(t, left.Root, "left.txt", "left done\n")
	writeFixtureFile(t, right.Root, "right.txt", "right done\n")
	leftDelta, rightDelta := f.delta(t, left), f.delta(t, right)
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i, pair := range []struct {
		root  string
		delta *AttemptWorkspaceDelta
	}{{left.Root, leftDelta}, {right.Root, rightDelta}} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = applyAttemptWorkspaceDelta(context.Background(), f.subject, pair.root, pair.delta)
		}()
	}
	wg.Wait()
	if errs[0] != nil || errs[1] != nil {
		t.Fatalf("disjoint parallel applies failed: %v", errs)
	}
	if readFixtureFile(t, f.subject, "left.txt") != "left done\n" || readFixtureFile(t, f.subject, "right.txt") != "right done\n" {
		t.Fatal("disjoint parallel applies lost a change")
	}

	first, second := f.prepare(t), f.prepare(t)
	writeFixtureFile(t, first.Root, "shared.txt", "first\n")
	writeFixtureFile(t, second.Root, "shared.txt", "second\n")
	if _, err := applyAttemptWorkspaceDelta(context.Background(), f.subject, first.Root, f.delta(t, first)); err != nil {
		t.Fatal(err)
	}
	var conflict *AttemptWorkspaceConflictError
	if _, err := applyAttemptWorkspaceDelta(context.Background(), f.subject, second.Root, f.delta(t, second)); !errors.As(err, &conflict) {
		t.Fatalf("second apply to the same file = %v, want a conflict", err)
	}
	if got := readFixtureFile(t, f.subject, "shared.txt"); got != "first\n" {
		t.Fatalf("shared.txt = %q, want the first apply", got)
	}
}

func TestRemoveAttemptWorldDirRequiresOwnership(t *testing.T) {
	control := t.TempDir()
	worlds := filepath.Join(control, attemptWorldsDirName)
	stranger := filepath.Join(worlds, "aw-0000000000000000")
	if err := os.MkdirAll(stranger, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := removeAttemptWorldDir(stranger, "aw-0000000000000000"); err == nil {
		t.Fatal("a directory without an owner marker was removed")
	}
	if _, err := os.Stat(stranger); err != nil {
		t.Fatalf("stranger directory was deleted: %v", err)
	}
	outside := filepath.Join(control, "aw-0000000000000001")
	if err := removeAttemptWorldDir(outside, "aw-0000000000000001"); err == nil {
		t.Fatal("a directory outside attempt-worlds was accepted")
	}
	if err := removeAttemptWorldDir(filepath.Join(worlds, "aw-missing"), "aw-missing"); err != nil {
		t.Fatalf("removing an absent world = %v", err)
	}
}

func TestIsolatedWorldGitModeCopiesDirtyTreeAndHonorsGitignore(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git unavailable")
	}
	f := newIsolatedWorldFixture(t, map[string]string{"tracked.txt": "committed\n", ".gitignore": ".env\nbuild/\n"})
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = f.subject
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q")
	run("add", ".")
	run("commit", "-qm", "init")
	writeFixtureFile(t, f.subject, "tracked.txt", "uncommitted edit\n")
	writeFixtureFile(t, f.subject, "untracked.txt", "new file\n")
	writeFixtureFile(t, f.subject, ".env", "SECRET=1\n")
	prepared := f.prepare(t)
	if readFixtureFile(t, prepared.Root, "tracked.txt") != "uncommitted edit\n" || readFixtureFile(t, prepared.Root, "untracked.txt") != "new file\n" {
		t.Fatal("the world did not copy the dirty working tree")
	}
	if _, err := os.Stat(filepath.Join(prepared.Root, ".env")); !os.IsNotExist(err) {
		t.Fatal("an ignored .env was copied into the world")
	}
	if _, err := os.Stat(filepath.Join(prepared.Root, ".git")); !os.IsNotExist(err) {
		t.Fatal("the user's .git was copied into the world")
	}
	writeFixtureFile(t, prepared.Root, "build/output.o", "obj\n")
	writeFixtureFile(t, prepared.Root, "feature.go", "package feature\n")
	delta := f.delta(t, prepared)
	if got := changeOps(delta); len(got) != 1 || got["feature.go"] != AttemptPathAdd {
		t.Fatalf("git-mode delta = %v, want only feature.go", got)
	}

	nested := newIsolatedWorldFixture(t, map[string]string{"a.txt": "a\n"})
	for _, dir := range []string{nested.subject, filepath.Join(nested.subject, "vendor", "lib")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command("git", "init", "-q")
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git init: %v\n%s", err, out)
		}
	}
	writeFixtureFile(t, nested.subject, "vendor/lib/x.go", "package lib\n")
	if _, err := nested.world.Prepare(context.Background(), ExecutionWorldSpec{RunID: "r", TaskID: "1", Attempt: 1, Root: nested.subject, ControlWorkspace: nested.control}); err == nil || !strings.Contains(err.Error(), "nested repositories") {
		t.Fatalf("Prepare with a nested repository = %v", err)
	}
}
