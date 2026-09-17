package workspace

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDiscoverSubjectRootUsesNearestGitMarker(t *testing.T) {
	root := t.TempDir()
	outer := filepath.Join(root, "outer")
	inner := filepath.Join(outer, "nested", "inner")
	start := filepath.Join(inner, "a", "b")
	for _, directory := range []string{filepath.Join(outer, ".git"), inner, start} {
		if err := os.MkdirAll(directory, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(inner, ".git"), []byte("gitdir: elsewhere\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := DiscoverSubjectRoot(start)
	if err != nil {
		t.Fatal(err)
	}
	if got != inner {
		t.Fatalf("subject root = %q, want %q", got, inner)
	}
}

func TestDiscoverSubjectRootDoesNotFollowGitSymlink(t *testing.T) {
	root := t.TempDir()
	start := filepath.Join(root, "project", "nested")
	if err := os.MkdirAll(start, 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "actual-git")
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(root, "project", ".git")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := DiscoverSubjectRoot(start)
	if err != nil {
		t.Fatal(err)
	}
	if got != root {
		t.Fatalf("subject root = %q, want enclosing repository %q", got, root)
	}
}

func TestDiscoverSubjectRootCanonicalizesStart(t *testing.T) {
	root := t.TempDir()
	repository := filepath.Join(root, "repository")
	start := filepath.Join(repository, "nested")
	if err := os.MkdirAll(filepath.Join(repository, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(start, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(start, link); err != nil {
		t.Fatal(err)
	}
	got, err := DiscoverSubjectRoot(link)
	if err != nil {
		t.Fatal(err)
	}
	if got != repository {
		t.Fatalf("subject root = %q, want %q", got, repository)
	}
}

func TestDiscoverSubjectRootRejectsFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := DiscoverSubjectRoot(path); err == nil {
		t.Fatal("expected non-directory error")
	}
}
