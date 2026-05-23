package team

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadLTMNonexistent(t *testing.T) {
	dir := t.TempDir()
	if got := LoadLTM(dir, "test-team"); got != "" {
		t.Errorf("LoadLTM() = %q, want empty", got)
	}
}

func TestSaveAndLoadLTM(t *testing.T) {
	dir := t.TempDir()
	content := "## project conventions\n- use conventional commits"
	if err := SaveLTM(dir, "test-team", content); err != nil {
		t.Fatalf("SaveLTM() error: %v", err)
	}
	got := LoadLTM(dir, "test-team")
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
	if err := InitLTM(dir, "test-team"); err != nil {
		t.Fatalf("InitLTM() error: %v", err)
	}
	if _, err := os.Stat(LTMPath(dir, "test-team")); err != nil {
		t.Errorf("ltm.md should exist after InitLTM: %v", err)
	}
}

func TestInitLTMIdempotent(t *testing.T) {
	dir := t.TempDir()
	SaveLTM(dir, "test-team", "existing knowledge")
	if err := InitLTM(dir, "test-team"); err != nil {
		t.Fatalf("InitLTM() error: %v", err)
	}
	got := LoadLTM(dir, "test-team")
	if got != "existing knowledge" {
		t.Errorf("InitLTM() clobbered existing content: got %q", got)
	}
}

func TestLTMPath(t *testing.T) {
	got := LTMPath("/tmp/team", "test-team")
	want := filepath.Join("/tmp/team", "ltm-test-team.md")
	if got != want {
		t.Errorf("LTMPath() = %q, want %q", got, want)
	}
}

func TestLTMPathSanitization(t *testing.T) {
	tests := []struct {
		teamName string
		want     string
	}{
		{"normal-team", "ltm-normal-team.md"},
		{"foo/bar", "ltm-foo-bar.md"},
		{"../../../etc", "ltm-..-..-..-etc.md"},   // ".." preserved (dots allowed), "/" → "-"
		{"..\\..\\windows", "ltm-..-..-windows.md"}, // ".." preserved, "\\" → "-"
		{"team/name/with/slashes", "ltm-team-name-with-slashes.md"},
		{"", "ltm-default.md"},  // empty name → default
		{"foo\x00bar", "ltm-foo-bar.md"},  // null byte → "-"
	}

	for _, tt := range tests {
		got := LTMPath("/tmp/workspace", tt.teamName)
		want := filepath.Join("/tmp/workspace", tt.want)
		if got != want {
			t.Errorf("LTMPath(%q) = %q, want %q", tt.teamName, got, want)
		}
	}
}

func TestFormatLTMEntry(t *testing.T) {
	got := formatLTMEntry("Use bcrypt for password hashing")
	if got == "" {
		t.Fatal("formatLTMEntry returned empty")
	}
	if !stringContains(got, "[20") {
		t.Errorf("formatLTMEntry missing timestamp: %s", got)
	}
	if !stringContains(got, "bcrypt") {
		t.Errorf("formatLTMEntry missing content: %s", got)
	}
}

func TestPruneLTM(t *testing.T) {
	content := "# 專案慣例\n- entry1\n- entry2\n- entry3\n- entry4\n- entry5\n- entry6\n- entry7\n- entry8\n- entry9\n- entry10\n- entry11"
	got := PruneLTM(content)
	sections := ParseSTMSections(got)
	if len(sections) == 0 {
		t.Fatal("PruneLTM returned empty")
	}
	if len(sections[0].Entries) > maxEntriesPerLTMSection {
		t.Errorf("PruneLTM entries = %d, want <= %d", len(sections[0].Entries), maxEntriesPerLTMSection)
	}
}

func TestPruneLTMEmpty(t *testing.T) {
	if got := PruneLTM(""); got != "" {
		t.Errorf("PruneLTM(\"\") = %q, want empty", got)
	}
}

func TestDeduplicateLTREntries(t *testing.T) {
	entries := []string{"- [2026-05-10] use bcrypt", "- [2026-05-11] use bcrypt", "- [2026-05-12] migrate to postgres"}
	got := deduplicateLTREntries(entries)
	if len(got) != 2 {
		t.Errorf("deduplicateLTREntries len = %d, want 2: %v", len(got), got)
	}
}

func TestNormalizeLTREntry(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"- [2026-05-10] Use bcrypt for password", "bcrypt password"},
		{"- [2026-05-10] login.go has SQL injection", "login.go injection"},
	}
	for _, tt := range tests {
		got := normalizeLTREntry(tt.input)
		if got != tt.want {
			t.Errorf("normalizeLTREntry(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestClassifyLTMEntry(t *testing.T) {
	tests := []struct {
		entry  string
		source string
		want   string
	}{
		{"login.go has SQL injection", "finding", ltmSectionFiles},
		{"always run go vet before committing", "finding", ltmSectionConventions},
		{"use the factory pattern for new services", "finding", ltmSectionPatterns},
		{"switch from SQLite to PostgreSQL", "decision", ltmSectionArchitecture},
		{"fixed: timeout by adding retry logic", "error", ltmSectionIssues},
		{"run go build before deploying", "finding", ltmSectionTools},
	}
	for _, tt := range tests {
		got := ClassifyLTMEntry(tt.entry, tt.source)
		if got != tt.want {
			t.Errorf("classifyLTMEntry(%q, %q) = %q, want %q", tt.entry, tt.source, got, tt.want)
		}
	}
}

func TestStripSTMListItem(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"- [FAILED] writer failed: timeout", "writer failed: timeout"},
		{"- researcher: found bug", "researcher: found bug"},
		{"* reviewer: approved", "reviewer: approved"},
		{"plain text", "plain text"},
	}
	for _, tt := range tests {
		got := stripSTMListItem(tt.input)
		if got != tt.want {
			t.Errorf("stripSTMListItem(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestHasLTREntry(t *testing.T) {
	sections := []STMSection{
		{Title: "# 專案慣例", Entries: []string{"- [2026-05-10] use bcrypt"}},
	}
	if !hasLTREntry(sections, "# 專案慣例", "- [2026-05-10] use bcrypt") {
		t.Error("hasLTREntry should return true for matching entry")
	}
	if hasLTREntry(sections, "# 專案慣例", "- [2026-05-10] use argon2") {
		t.Error("hasLTREntry should return false for different entry")
	}
	if hasLTREntry(sections, "# 架構決策", "- [2026-05-10] use bcrypt") {
		t.Error("hasLTREntry should return false for different section")
	}
}

func TestExtractLTMFromHistory(t *testing.T) {
	workspace := t.TempDir()

	histDir := workspace + "/history"
	if err := os.MkdirAll(histDir, 0o755); err != nil {
		t.Fatal(err)
	}

	stm1 := "# 決策\n- use bcrypt for password hashing\n\n# 發現\n- login.go has SQL injection"
	stm2 := "# 錯誤與修復\n- fixed: timeout by adding retry logic"
	os.WriteFile(histDir+"/20260101T120000-stm.md", []byte(stm1), 0o644)
	os.WriteFile(histDir+"/20260102T130000-stm.md", []byte(stm2), 0o644)
	// non-stm file should be ignored
	os.WriteFile(histDir+"/session.md", []byte("irrelevant"), 0o644)

	ExtractLTMFromHistory(workspace, "test-team")

	ltm := LoadLTM(workspace, "test-team")
	if ltm == "" {
		t.Fatal("ExtractLTMFromHistory: ltm.md is empty")
	}
	if !stringContains(ltm, "bcrypt") {
		t.Errorf("missing bcrypt entry: %s", ltm)
	}
	if !stringContains(ltm, "SQL injection") && !stringContains(ltm, "injection") {
		t.Errorf("missing SQL injection entry: %s", ltm)
	}
	if !stringContains(ltm, "retry") {
		t.Errorf("missing retry entry: %s", ltm)
	}

	// History stm files should be deleted
	entries, _ := os.ReadDir(histDir)
	for _, e := range entries {
		if stringContains(e.Name(), "-stm.md") {
			t.Errorf("history file not deleted: %s", e.Name())
		}
	}
}

func TestExtractLTMFromHistoryEmpty(t *testing.T) {
	workspace := t.TempDir()

	// No history directory at all — should not panic
	ExtractLTMFromHistory(workspace, "test-team")

	if ltm := LoadLTM(workspace, "test-team"); ltm != "" {
		t.Errorf("expected empty ltm, got %q", ltm)
	}
}

func stringContains(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
