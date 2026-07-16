package team

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The archived transcript's slug is derived from a title line like
// "Session — kvmforge-verify": the em-dash is surrounded by spaces, so
// replacing spaces and the em-dash with "-" independently used to stack into
// "---" (e.g. "2026-07-13-session---kvmforge-verify.md") instead of
// collapsing to a single dash.
func TestArchiveSessionMDCollapsesRepeatedDashesInSlug(t *testing.T) {
	dir := t.TempDir()
	md := "# Session — kvmforge-verify\n\n**Started:** x\n\nsome content\n"
	if err := SaveSessionMD(dir, md); err != nil {
		t.Fatalf("SaveSessionMD: %v", err)
	}

	path, err := ArchiveSessionMD(dir)
	if err != nil {
		t.Fatalf("ArchiveSessionMD: %v", err)
	}
	if path == "" {
		t.Fatal("expected a non-empty archived path")
	}
	name := filepath.Base(path)
	if strings.Contains(name, "---") {
		t.Errorf("archived filename still has stacked dashes: %q", name)
	}
}

func TestPruneSessionHistoryKeepsNewestAndDeletesOldest(t *testing.T) {
	dir := t.TempDir()
	histDir := filepath.Join(dir, historyDirName)
	if err := os.MkdirAll(histDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	names := []string{
		"2026-07-01-session-a.md",
		"2026-07-05-session-b.md",
		"2026-07-10-session-c.md",
		"2026-07-12-session-d.md",
		"2026-07-13-session-e.md",
	}
	for _, n := range names {
		if err := os.WriteFile(filepath.Join(histDir, n), []byte("x"), 0o644); err != nil {
			t.Fatalf("write %s: %v", n, err)
		}
	}
	// A *-stm.md snapshot should never be touched by this — that's
	// ExtractLTMFromHistory's job.
	if err := os.WriteFile(filepath.Join(histDir, "2026-06-01-stm.md"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write stm file: %v", err)
	}

	PruneSessionHistory(dir, 2)

	entries, err := os.ReadDir(histDir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	var remaining []string
	for _, e := range entries {
		remaining = append(remaining, e.Name())
	}

	want := map[string]bool{
		"2026-07-12-session-d.md": true,
		"2026-07-13-session-e.md": true,
		"2026-06-01-stm.md":       true,
	}
	if len(remaining) != len(want) {
		t.Fatalf("remaining = %v, want exactly %v", remaining, want)
	}
	for _, n := range remaining {
		if !want[n] {
			t.Errorf("unexpected file survived pruning: %q", n)
		}
	}
}

func TestPruneSessionHistoryNoopUnderLimit(t *testing.T) {
	dir := t.TempDir()
	histDir := filepath.Join(dir, historyDirName)
	if err := os.MkdirAll(histDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(histDir, "2026-07-13-session-a.md"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	PruneSessionHistory(dir, 20)

	entries, err := os.ReadDir(histDir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 1 {
		t.Errorf("expected the single file to survive, got %d entries", len(entries))
	}
}

func TestPruneSessionHistoryMissingDirIsNoop(t *testing.T) {
	dir := t.TempDir()
	// history/ doesn't exist — must not panic or create it.
	PruneSessionHistory(dir, 5)
	if _, err := os.Stat(filepath.Join(dir, historyDirName)); err == nil {
		t.Error("PruneSessionHistory should not create history/ when it doesn't exist")
	}
}
