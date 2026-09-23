package versionstore

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
)

func paths(changes []PathChange) []string {
	out := make([]string, 0, len(changes))
	for _, change := range changes {
		out = append(out, change.Path)
	}
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for index := range a {
		if a[index] != b[index] {
			return false
		}
	}
	return true
}

func TestDiffClassifiesChanges(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	root := t.TempDir()
	writeFiles(t, root, map[string]string{
		"same.txt": "s", "mod.txt": "before", "del.txt": "x", "mode.sh": "m",
		"typechange": "file", "dir/inner.txt": "i", "link-target": "t",
	})
	if err := os.Symlink("link-target", filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	from := mustCapture(t, store, baseRequest(root))

	writeFiles(t, root, map[string]string{"mod.txt": "after", "add.txt": "new"})
	if err := os.Remove(filepath.Join(root, "del.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(root, "mode.sh"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, "typechange")); err != nil {
		t.Fatal(err)
	}
	writeFiles(t, root, map[string]string{"typechange/child": "c"})
	if err := os.RemoveAll(filepath.Join(root, "dir")); err != nil {
		t.Fatal(err)
	}
	writeFiles(t, root, map[string]string{"dir": "file now"})
	if err := os.Remove(filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("same.txt", filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	to := mustCapture(t, store, baseRequest(root))

	delta, err := store.Diff(ctx, from.ID, to.ID)
	if err != nil {
		t.Fatal(err)
	}
	checks := []struct {
		name string
		got  []string
		want []string
	}{
		{name: "added", got: paths(delta.Added), want: []string{"add.txt", "typechange/child"}},
		{name: "modified", got: paths(delta.Modified), want: []string{"link", "mod.txt", "mode.sh"}},
		{name: "deleted", got: paths(delta.Deleted), want: []string{"del.txt", "dir/inner.txt"}},
		{name: "type changed", got: paths(delta.TypeChanged), want: []string{"dir", "typechange"}},
	}
	for _, check := range checks {
		if !equalStrings(check.got, check.want) {
			t.Errorf("%s = %v, want %v", check.name, check.got, check.want)
		}
	}
	if empty, _ := store.Diff(ctx, to.ID, to.ID); !empty.Empty() {
		t.Fatalf("self diff = %#v", empty)
	}
}

func TestDiffSkipsIdenticalSubtrees(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	root := t.TempDir()
	files := map[string]string{}
	for dir := 0; dir < 20; dir++ {
		for file := 0; file < 5; file++ {
			files[fmt.Sprintf("pkg%02d/sub/file%d.go", dir, file)] = fmt.Sprintf("%d-%d", dir, file)
		}
	}
	writeFiles(t, root, files)
	from := mustCapture(t, store, baseRequest(root))
	writeFiles(t, root, map[string]string{"pkg07/sub/file3.go": "changed"})
	to := mustCapture(t, store, baseRequest(root))

	reads := &atomic.Int64{}
	store.treeReads = reads
	delta, err := store.Diff(ctx, from.ID, to.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !equalStrings(paths(delta.Modified), []string{"pkg07/sub/file3.go"}) {
		t.Fatalf("modified = %v", paths(delta.Modified))
	}
	// root, pkg07, and pkg07/sub on each side; the 19 untouched packages are
	// never read.
	if got := reads.Load(); got != 6 {
		t.Fatalf("tree reads = %d, want 6", got)
	}
}
