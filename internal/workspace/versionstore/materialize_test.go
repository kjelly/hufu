package versionstore

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func readFile(t *testing.T, root, rel string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(data)
}

func assertAbsent(t *testing.T, root, rel string) {
	t.Helper()
	if _, err := os.Lstat(filepath.Join(root, filepath.FromSlash(rel))); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("%s should be absent, got %v", rel, err)
	}
}

func mustMaterialize(t *testing.T, store *Store, req MaterializeRequest) MaterializeResult {
	t.Helper()
	result, err := store.Materialize(context.Background(), req)
	if err != nil {
		t.Fatalf("Materialize: %v", err)
	}
	return result
}

// TestPhase1Acceptance is the §36 Phase 1 acceptance flow.
func TestPhase1Acceptance(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	root := t.TempDir()
	writeFiles(t, root, map[string]string{"a.txt": "one", "dir/b.txt": "two"})
	s1 := mustCapture(t, store, baseRequest(root))
	writeFiles(t, root, map[string]string{"a.txt": "ONE", "c.txt": "three"})
	if err := os.Remove(filepath.Join(root, "dir", "b.txt")); err != nil {
		t.Fatal(err)
	}
	s2 := mustCapture(t, store, baseRequest(root))
	delta, err := store.Diff(ctx, s1.ID, s2.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(delta.Added) != 1 || delta.Added[0].Path != "c.txt" || len(delta.Modified) != 1 || delta.Modified[0].Path != "a.txt" || len(delta.Deleted) != 1 || delta.Deleted[0].Path != "dir/b.txt" {
		t.Fatalf("delta = %#v", delta)
	}
	target := t.TempDir()
	mustMaterialize(t, store, MaterializeRequest{Root: target, SnapshotID: s1.ID})
	recaptured := mustCapture(t, store, baseRequest(target))
	if recaptured.RootTreeHash != s1.RootTreeHash {
		t.Fatalf("tree(temp) = %s, want S1 %s", recaptured.RootTreeHash, s1.RootTreeHash)
	}
}

func TestMaterializeSwitchesManagedPathsAndPreservesUnmanaged(t *testing.T) {
	store := openTestStore(t)
	root := t.TempDir()
	writeFiles(t, root, map[string]string{".hufuignore": "ignored/\n", "a.txt": "main", "old/deep/gone.txt": "bye"})
	s1 := mustCapture(t, store, baseRequest(root))
	if err := os.RemoveAll(filepath.Join(root, "old")); err != nil {
		t.Fatal(err)
	}
	writeFiles(t, root, map[string]string{"a.txt": "exp", "b.txt": "new"})
	s2 := mustCapture(t, store, baseRequest(root))
	writeFiles(t, root, map[string]string{"ignored/keep.txt": "unmanaged", "untracked-later.txt": "x"})

	result := mustMaterialize(t, store, MaterializeRequest{Root: root, SnapshotID: s1.ID, CurrentSnapshotID: s2.ID})
	if readFile(t, root, "a.txt") != "main" || readFile(t, root, "old/deep/gone.txt") != "bye" {
		t.Fatal("S1 content was not restored")
	}
	assertAbsent(t, root, "b.txt")
	if readFile(t, root, "ignored/keep.txt") != "unmanaged" || readFile(t, root, "untracked-later.txt") != "x" {
		t.Fatal("unmanaged paths were not preserved")
	}
	if result.FilesWritten != 2 || result.FilesDeleted != 1 {
		t.Fatalf("result = %#v", result)
	}

	mustMaterialize(t, store, MaterializeRequest{Root: root, SnapshotID: s2.ID, CurrentSnapshotID: s1.ID})
	if readFile(t, root, "a.txt") != "exp" || readFile(t, root, "b.txt") != "new" {
		t.Fatal("S2 content was not restored")
	}
	assertAbsent(t, root, "old/deep/gone.txt")
	assertAbsent(t, root, "old")
}

func TestMaterializeIsIdempotent(t *testing.T) {
	store := openTestStore(t)
	root := t.TempDir()
	writeFiles(t, root, map[string]string{"a": "1", "b/c": "2"})
	snapshot := mustCapture(t, store, baseRequest(root))
	target := t.TempDir()
	first := mustMaterialize(t, store, MaterializeRequest{Root: target, SnapshotID: snapshot.ID})
	second := mustMaterialize(t, store, MaterializeRequest{Root: target, SnapshotID: snapshot.ID})
	if first.FilesWritten != 2 || second.FilesWritten != 0 {
		t.Fatalf("first = %#v second = %#v", first, second)
	}
}

func TestMaterializeRecoversFromPartialWrite(t *testing.T) {
	store := openTestStore(t)
	root := t.TempDir()
	writeFiles(t, root, map[string]string{"a": "1", "b": "2", "c": "3"})
	snapshot := mustCapture(t, store, baseRequest(root))
	target := t.TempDir()
	// Simulate a crash after "a" was written and a temp file was left behind.
	writeFiles(t, target, map[string]string{"a": "1", ".hufu-materialize-deadbeef": "partial"})
	if err := os.Chmod(filepath.Join(target, "a"), 0o644); err != nil {
		t.Fatal(err)
	}
	result := mustMaterialize(t, store, MaterializeRequest{Root: target, SnapshotID: snapshot.ID})
	if result.FilesWritten != 2 || readFile(t, target, "c") != "3" {
		t.Fatalf("result = %#v", result)
	}
	if recaptured := mustCapture(t, store, baseRequest(target)); recaptured.RootTreeHash != snapshot.RootTreeHash {
		t.Fatal("stale temp file leaked into the managed tree")
	}
}

func TestMaterializeCollisions(t *testing.T) {
	outside := t.TempDir()
	tests := []struct {
		name  string
		setup func(t *testing.T, target string)
	}{
		{name: "unmanaged file at target path", setup: func(t *testing.T, target string) {
			writeFiles(t, target, map[string]string{"foo/bar.txt": "someone else's"})
		}},
		{name: "symlink where a directory is needed", setup: func(t *testing.T, target string) {
			if err := os.Symlink(outside, filepath.Join(target, "foo")); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "unmanaged file where a directory is needed", setup: func(t *testing.T, target string) {
			writeFiles(t, target, map[string]string{"foo": "file"})
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := openTestStore(t)
			source := t.TempDir()
			writeFiles(t, source, map[string]string{"foo/bar.txt": "managed"})
			snapshot := mustCapture(t, store, baseRequest(source))
			target := t.TempDir()
			tt.setup(t, target)
			_, err := store.Materialize(context.Background(), MaterializeRequest{Root: target, SnapshotID: snapshot.ID})
			if !errors.Is(err, ErrMaterializationCollision) {
				t.Fatalf("err = %v, want ErrMaterializationCollision", err)
			}
			entries, readErr := os.ReadDir(outside)
			if readErr != nil || len(entries) != 0 {
				t.Fatalf("materialize followed a symlink outside the root: %v %v", entries, readErr)
			}
		})
	}
}

func TestMaterializeSymlinks(t *testing.T) {
	store := openTestStore(t)
	root := t.TempDir()
	writeFiles(t, root, map[string]string{"real.txt": "r"})
	if err := os.Symlink("real.txt", filepath.Join(root, "internal")); err != nil {
		t.Fatal(err)
	}
	internal := mustCapture(t, store, baseRequest(root))
	target := t.TempDir()
	mustMaterialize(t, store, MaterializeRequest{Root: target, SnapshotID: internal.ID})
	if link, err := os.Readlink(filepath.Join(target, "internal")); err != nil || link != "real.txt" {
		t.Fatalf("internal symlink = %q, %v", link, err)
	}

	if err := os.Symlink(t.TempDir(), filepath.Join(root, "escaping")); err != nil {
		t.Fatal(err)
	}
	escaping := mustCapture(t, store, baseRequest(root))
	if _, err := store.Materialize(context.Background(), MaterializeRequest{Root: t.TempDir(), SnapshotID: escaping.ID}); !errors.Is(err, ErrSnapshotNotMaterializable) {
		t.Fatalf("escaping snapshot err = %v, want ErrSnapshotNotMaterializable", err)
	}
}

func TestMaterializeTypeChanges(t *testing.T) {
	store := openTestStore(t)
	root := t.TempDir()
	writeFiles(t, root, map[string]string{"node": "i am a file", "dir/child": "c"})
	fileVersion := mustCapture(t, store, baseRequest(root))
	if err := os.Remove(filepath.Join(root, "node")); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(root, "dir")); err != nil {
		t.Fatal(err)
	}
	writeFiles(t, root, map[string]string{"node/inner": "now a dir", "dir": "now a file"})
	dirVersion := mustCapture(t, store, baseRequest(root))

	mustMaterialize(t, store, MaterializeRequest{Root: root, SnapshotID: fileVersion.ID, CurrentSnapshotID: dirVersion.ID})
	if readFile(t, root, "node") != "i am a file" || readFile(t, root, "dir/child") != "c" {
		t.Fatal("dir→file / file→dir switch failed")
	}
	mustMaterialize(t, store, MaterializeRequest{Root: root, SnapshotID: dirVersion.ID, CurrentSnapshotID: fileVersion.ID})
	if readFile(t, root, "node/inner") != "now a dir" || readFile(t, root, "dir") != "now a file" {
		t.Fatal("switching back failed")
	}
}

func TestMaterializeRespectsExcludedSubtrees(t *testing.T) {
	store := openTestStore(t)
	root := t.TempDir()
	writeFiles(t, root, map[string]string{"a": "1", "control/session_tree.json": "authority v1"})
	req := baseRequest(root)
	req.ExcludeSubtrees = []string{"control"}
	s1 := mustCapture(t, store, req)
	writeFiles(t, root, map[string]string{"a": "2", "control/session_tree.json": "authority v2"})
	s2 := mustCapture(t, store, req)
	mustMaterialize(t, store, MaterializeRequest{Root: root, SnapshotID: s1.ID, CurrentSnapshotID: s2.ID, ExcludeSubtrees: []string{"control"}})
	if readFile(t, root, "a") != "1" || readFile(t, root, "control/session_tree.json") != "authority v2" {
		t.Fatal("excluded control subtree was touched")
	}
}

func TestMaterializeDetectsCorruptBlob(t *testing.T) {
	store := openTestStore(t)
	root := t.TempDir()
	writeFiles(t, root, map[string]string{"a": "payload"})
	snapshot := mustCapture(t, store, baseRequest(root))
	path, _ := store.layout.objectPath(ObjectBlob, sha("payload"))
	if err := os.WriteFile(path, []byte("PAYLOAD"), 0o600); err != nil {
		t.Fatal(err)
	}
	target := t.TempDir()
	if _, err := store.Materialize(context.Background(), MaterializeRequest{Root: target, SnapshotID: snapshot.ID}); !errors.Is(err, ErrCASCorrupt) {
		t.Fatalf("err = %v, want ErrCASCorrupt", err)
	}
	assertAbsent(t, target, "a")
}
