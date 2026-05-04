package team

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadLTMNonexistent(t *testing.T) {
	dir := t.TempDir()
	if got := LoadLTM(dir); got != "" {
		t.Errorf("LoadLTM() = %q, want empty", got)
	}
}

func TestSaveAndLoadLTM(t *testing.T) {
	dir := t.TempDir()
	content := "## project conventions\n- use conventional commits"
	if err := SaveLTM(dir, content); err != nil {
		t.Fatalf("SaveLTM() error: %v", err)
	}
	got := LoadLTM(dir)
	if got != content {
		t.Errorf("LoadLTM() = %q, want %q", got, content)
	}
}

func TestTruncateLTMUnderLimit(t *testing.T) {
	s := "hello"
	if got := TruncateLTM(s); got != s {
		t.Errorf("TruncateLTM() = %q, want %q", got, s)
	}
}

func TestTruncateLTMOverLimit(t *testing.T) {
	runes := make([]rune, maxLTMChars+100)
	for i := range runes {
		runes[i] = 'y'
	}
	long := string(runes)
	got := TruncateLTM(long)
	if len(got) != maxLTMChars {
		t.Errorf("TruncateLTM() len = %d, want %d", len(got), maxLTMChars)
	}
}

func TestInitLTMCreatesFile(t *testing.T) {
	dir := t.TempDir()
	if err := InitLTM(dir); err != nil {
		t.Fatalf("InitLTM() error: %v", err)
	}
	if _, err := os.Stat(LTMPath(dir)); err != nil {
		t.Errorf("ltm.md should exist after InitLTM: %v", err)
	}
}

func TestInitLTMIdempotent(t *testing.T) {
	dir := t.TempDir()
	SaveLTM(dir, "existing knowledge")
	if err := InitLTM(dir); err != nil {
		t.Fatalf("InitLTM() error: %v", err)
	}
	got := LoadLTM(dir)
	if got != "existing knowledge" {
		t.Errorf("InitLTM() clobbered existing content: got %q", got)
	}
}

func TestLTMPath(t *testing.T) {
	got := LTMPath("/tmp/team")
	want := filepath.Join("/tmp/team", "ltm.md")
	if got != want {
		t.Errorf("LTMPath() = %q, want %q", got, want)
	}
}
