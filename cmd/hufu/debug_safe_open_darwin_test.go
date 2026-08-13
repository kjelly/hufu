//go:build darwin

package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDebugOpenFileDarwinReadsRegularFileAndRejectsSymlink(t *testing.T) {
	root := t.TempDir()
	regular := filepath.Join(root, "regular.txt")
	if err := os.WriteFile(regular, []byte("darwin debug"), 0644); err != nil {
		t.Fatal(err)
	}
	f, err := debugOpenFile(regular)
	if err != nil {
		t.Fatalf("debugOpenFile regular file: %v", err)
	}
	data, err := io.ReadAll(f)
	_ = f.Close()
	if err != nil || string(data) != "darwin debug" {
		t.Fatalf("regular file read = %q, err=%v", data, err)
	}

	external := filepath.Join(root, "external.txt")
	if err := os.WriteFile(external, []byte("outside"), 0644); err != nil {
		t.Fatal(err)
	}
	linked := filepath.Join(root, "linked.txt")
	if err := os.Symlink(external, linked); err != nil {
		t.Skipf("symlink test unavailable: %v", err)
	}
	if _, err := debugOpenFile(linked); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("symlink read was not rejected: %v", err)
	}
}
