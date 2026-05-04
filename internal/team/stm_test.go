package team

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadSTMNonexistent(t *testing.T) {
	dir := t.TempDir()
	if got := LoadSTM(dir); got != "" {
		t.Errorf("LoadSTM() = %q, want empty", got)
	}
}

func TestSaveAndLoadSTM(t *testing.T) {
	dir := t.TempDir()
	content := "## findings\n- API is /v2"
	if err := SaveSTM(dir, content); err != nil {
		t.Fatalf("SaveSTM() error: %v", err)
	}
	got := LoadSTM(dir)
	if got != content {
		t.Errorf("LoadSTM() = %q, want %q", got, content)
	}
}

func TestTruncateSTMUnderLimit(t *testing.T) {
	s := "hello"
	if got := TruncateSTM(s); got != s {
		t.Errorf("TruncateSTM() = %q, want %q", got, s)
	}
}

func TestTruncateSTMOverLimit(t *testing.T) {
	runes := make([]rune, maxSTMChars+100)
	for i := range runes {
		runes[i] = 'x'
	}
	long := string(runes)
	got := TruncateSTM(long)
	if len(got) != maxSTMChars {
		t.Errorf("TruncateSTM() len = %d, want %d", len(got), maxSTMChars)
	}
}

func TestArchiveSTMEmpty(t *testing.T) {
	dir := t.TempDir()
	path, err := ArchiveSTM(dir)
	if err != nil {
		t.Fatalf("ArchiveSTM() error: %v", err)
	}
	if path != "" {
		t.Errorf("ArchiveSTM() = %q, want empty", path)
	}
}

func TestArchiveSTMWithContent(t *testing.T) {
	dir := t.TempDir()
	SaveSTM(dir, "some working memory")

	path, err := ArchiveSTM(dir)
	if err != nil {
		t.Fatalf("ArchiveSTM() error: %v", err)
	}
	if path == "" {
		t.Fatal("ArchiveSTM() returned empty path")
	}

	if _, err := os.Stat(STMPath(dir)); !os.IsNotExist(err) {
		t.Error("stm.md should be removed after archive")
	}

	archived, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("failed to read archived file: %v", err)
	}
	if string(archived) != "some working memory" {
		t.Errorf("archived content = %q, want %q", string(archived), "some working memory")
	}
}

func TestInitSTMCreatesFile(t *testing.T) {
	dir := t.TempDir()
	if err := InitSTM(dir); err != nil {
		t.Fatalf("InitSTM() error: %v", err)
	}
	if _, err := os.Stat(STMPath(dir)); err != nil {
		t.Errorf("stm.md should exist after InitSTM: %v", err)
	}
}

func TestInitSTMIdempotent(t *testing.T) {
	dir := t.TempDir()
	SaveSTM(dir, "existing content")
	if err := InitSTM(dir); err != nil {
		t.Fatalf("InitSTM() error: %v", err)
	}
	got := LoadSTM(dir)
	if got != "existing content" {
		t.Errorf("InitSTM() clobbered existing content: got %q", got)
	}
}

func TestSTMPath(t *testing.T) {
	got := STMPath("/tmp/ws")
	want := filepath.Join("/tmp/ws", "stm.md")
	if got != want {
		t.Errorf("STMPath() = %q, want %q", got, want)
	}
}
