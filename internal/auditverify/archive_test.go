package auditverify

import (
	"archive/tar"
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestWriteAndExtractTarArchiveRoundTrip(t *testing.T) {
	entries := []bundleFileEntry{
		{path: "bundle.json", data: []byte(`{"a":1}`)},
		{path: "artifacts/data/sha256-abc", data: []byte("hello world")},
	}
	var buf bytes.Buffer
	if err := writeTarArchive(&buf, entries); err != nil {
		t.Fatalf("write: %v", err)
	}
	destDir := t.TempDir()
	if err := extractTarArchive(&buf, destDir); err != nil {
		t.Fatalf("extract: %v", err)
	}
	for _, entry := range entries {
		data, err := os.ReadFile(filepath.Join(destDir, entry.path))
		if err != nil {
			t.Fatalf("read extracted %s: %v", entry.path, err)
		}
		if !bytes.Equal(data, entry.data) {
			t.Fatalf("extracted %s = %q, want %q", entry.path, data, entry.data)
		}
	}
}

func TestExtractTarArchiveRejectsPathTraversal(t *testing.T) {
	cases := []string{"../escape.txt", "a/../../escape.txt", "/etc/passwd", "a/../../../etc/passwd"}
	for _, name := range cases {
		t.Run(name, func(t *testing.T) {
			var buf bytes.Buffer
			tw := tar.NewWriter(&buf)
			_ = tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Size: 4, Mode: 0o600})
			_, _ = tw.Write([]byte("evil"))
			_ = tw.Close()

			destDir := t.TempDir()
			if err := extractTarArchive(&buf, destDir); err == nil {
				t.Fatalf("extraction of %q was not rejected", name)
			}
			// The traversal target must never have been created.
			if _, statErr := os.Stat(filepath.Join(filepath.Dir(destDir), "escape.txt")); statErr == nil {
				t.Fatal("traversal target was created outside the destination directory")
			}
		})
	}
}

func TestExtractTarArchiveRejectsSymlink(t *testing.T) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	_ = tw.WriteHeader(&tar.Header{Name: "link", Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd", Mode: 0o600})
	_ = tw.Close()

	destDir := t.TempDir()
	if err := extractTarArchive(&buf, destDir); err == nil {
		t.Fatal("symlink entry was not rejected")
	}
}

func TestExtractTarArchiveRejectsHardlink(t *testing.T) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	_ = tw.WriteHeader(&tar.Header{Name: "link", Typeflag: tar.TypeLink, Linkname: "bundle.json", Mode: 0o600})
	_ = tw.Close()

	destDir := t.TempDir()
	if err := extractTarArchive(&buf, destDir); err == nil {
		t.Fatal("hardlink entry was not rejected")
	}
}

func TestExtractTarArchiveRejectsDuplicatePath(t *testing.T) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	_ = tw.WriteHeader(&tar.Header{Name: "a.txt", Typeflag: tar.TypeReg, Size: 1, Mode: 0o600})
	_, _ = tw.Write([]byte("1"))
	_ = tw.WriteHeader(&tar.Header{Name: "a.txt", Typeflag: tar.TypeReg, Size: 1, Mode: 0o600})
	_, _ = tw.Write([]byte("2"))
	_ = tw.Close()

	destDir := t.TempDir()
	if err := extractTarArchive(&buf, destDir); err == nil {
		t.Fatal("duplicate path entry was not rejected")
	}
}

func TestExtractTarArchiveRejectsOversizedEntry(t *testing.T) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	_ = tw.WriteHeader(&tar.Header{Name: "huge.bin", Typeflag: tar.TypeReg, Size: maxBundleEntryBytes + 1, Mode: 0o600})
	_ = tw.Close()

	destDir := t.TempDir()
	if err := extractTarArchive(&buf, destDir); err == nil {
		t.Fatal("oversized entry was not rejected")
	}
}

func TestSafeJoinRejectsEscape(t *testing.T) {
	destDir := t.TempDir()
	for _, name := range []string{"../x", "a/../../x", "/abs/x", "..", "C:\\x"} {
		if _, err := safeJoin(destDir, name); err == nil {
			t.Fatalf("safeJoin(%q) was not rejected", name)
		}
	}
}

func TestSafeJoinAcceptsNestedPath(t *testing.T) {
	destDir := t.TempDir()
	got, err := safeJoin(destDir, "artifacts/data/sha256-abc")
	if err != nil {
		t.Fatalf("safeJoin: %v", err)
	}
	want := filepath.Join(destDir, "artifacts", "data", "sha256-abc")
	if got != want {
		t.Fatalf("safeJoin = %q, want %q", got, want)
	}
}
