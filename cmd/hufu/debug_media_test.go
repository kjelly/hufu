package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestDebugBundleOmitsPrivateMediaBeforeTextRedaction(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	workspace := filepath.Join(root, "media-workspace")
	private := filepath.Join(workspace, "logs", "artifacts", "media", "data")
	if err := os.MkdirAll(private, 0o700); err != nil {
		t.Fatal(err)
	}
	// Corrupt CAS data must remain private even when it passes the generic
	// exporter's printable-text check; MIME sniffing is not an export grant.
	const sentinel = "private image content that resembles text"
	if err := os.WriteFile(filepath.Join(private, "image-fixture"), []byte(sentinel), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := newRootCommand()
	cmd.SetArgs([]string{"debug", workspace})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	entries := readDebugBundle(t, filepath.Join(root, "hufu-debug-media-workspace.tar.gz"))
	if _, found := entries["logs/artifacts/media/data/image-fixture"]; found {
		t.Fatal("debug bundle exported private media bytes")
	}
	for name, content := range entries {
		if bytes.Contains(content, []byte(sentinel)) {
			t.Fatalf("private media appeared in %s", name)
		}
	}
	if !bytes.Contains(entries["bundle-manifest.json"], []byte("logs/artifacts/media/data/image-fixture")) {
		t.Fatal("debug manifest did not account for the omitted private data")
	}
}
