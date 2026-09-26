package main

import (
	"archive/tar"
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type archiveTestEntry struct {
	name     string
	typeflag byte
	body     string
	linkname string
}

func archiveTestTar(t *testing.T, entries []archiveTestEntry) *bytes.Buffer {
	t.Helper()
	var buffer bytes.Buffer
	writer := tar.NewWriter(&buffer)
	for _, entry := range entries {
		header := &tar.Header{Name: entry.name, Typeflag: entry.typeflag, Linkname: entry.linkname, Mode: 0o644, Size: int64(len(entry.body))}
		if entry.typeflag == tar.TypeDir {
			header.Mode = 0o755
		}
		if err := writer.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if entry.body != "" {
			if _, err := writer.Write([]byte(entry.body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return &buffer
}

func TestExtractGitArchiveSkipsLinksThatLeaveTheSnapshot(t *testing.T) {
	root := t.TempDir()
	destination := filepath.Join(root, "snapshot")
	if err := os.Mkdir(destination, 0o755); err != nil {
		t.Fatal(err)
	}
	archive := archiveTestTar(t, []archiveTestEntry{
		{name: "pkg/", typeflag: tar.TypeDir},
		{name: "pkg/a.go", typeflag: tar.TypeReg, body: "package pkg\n"},
		{name: "pkg/alias.go", typeflag: tar.TypeSymlink, linkname: "a.go"},
		{name: ".tool/state.json", typeflag: tar.TypeSymlink, linkname: "/home/someone/.tool/state.json"},
		{name: "pkg/up", typeflag: tar.TypeSymlink, linkname: "../../outside"},
	})
	if err := extractGitArchive(archive, destination); err != nil {
		t.Fatalf("extractGitArchive: %v", err)
	}
	if target, err := os.Readlink(filepath.Join(destination, "pkg", "alias.go")); err != nil || target != "a.go" {
		t.Fatalf("in-snapshot link target = %q, %v; want a.go", target, err)
	}
	for _, skipped := range []string{filepath.Join(destination, ".tool", "state.json"), filepath.Join(destination, "pkg", "up")} {
		if _, err := os.Lstat(skipped); !os.IsNotExist(err) {
			t.Fatalf("unsafe link %s was created (lstat err = %v)", skipped, err)
		}
	}
	if _, err := os.Lstat(filepath.Join(root, "outside")); !os.IsNotExist(err) {
		t.Fatalf("extraction wrote outside the snapshot (lstat err = %v)", err)
	}
}

func TestExtractGitArchiveStillRejectsEntriesOutsideTheSnapshot(t *testing.T) {
	for _, name := range []string{"../evil.go", "/abs/evil.go"} {
		archive := archiveTestTar(t, []archiveTestEntry{{name: name, typeflag: tar.TypeReg, body: "x"}})
		if err := extractGitArchive(archive, t.TempDir()); err == nil || !strings.Contains(err.Error(), "escapes the snapshot") {
			t.Fatalf("entry %q: error = %v, want an escape rejection", name, err)
		}
	}
}

func TestArchiveGitRevisionSkipsAnAbsoluteSymlink(t *testing.T) {
	repo := newFixtureRepo(t)
	writeFile(t, filepath.Join(repo, "go.mod"), "module example.test/review\n\ngo 1.22\n")
	writeFile(t, filepath.Join(repo, "pkg", "pkg.go"), "package pkg\n\nfunc Exported() {}\n")
	if err := os.MkdirAll(filepath.Join(repo, ".tool"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/nonexistent/hufu-reviewprep/state.json", filepath.Join(repo, ".tool", "state.json")); err != nil {
		t.Fatal(err)
	}
	commit(t, repo, "add a package next to a tool state link", "2025-01-02T00:00:00Z")

	destination := t.TempDir()
	if err := archiveGitRevision(context.Background(), repo, "HEAD", destination); err != nil {
		t.Fatalf("archiveGitRevision: %v", err)
	}
	if content, err := os.ReadFile(filepath.Join(destination, "pkg", "pkg.go")); err != nil || !strings.Contains(string(content), "func Exported") {
		t.Fatalf("snapshot source = %q, %v", content, err)
	}
	if _, err := os.Lstat(filepath.Join(destination, ".tool", "state.json")); !os.IsNotExist(err) {
		t.Fatalf("absolute link was created in the snapshot (lstat err = %v)", err)
	}
}
