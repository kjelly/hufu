package team

import (
	"os"
	"path/filepath"
	"strings"
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
		{"../../../etc", "ltm-..-..-..-etc.md"},     // ".." preserved (dots allowed), "/" → "-"
		{"..\\..\\windows", "ltm-..-..-windows.md"}, // ".." preserved, "\\" → "-"
		{"team/name/with/slashes", "ltm-team-name-with-slashes.md"},
		{"", "ltm-default.md"},           // empty name → default
		{"foo\x00bar", "ltm-foo-bar.md"}, // null byte → "-"
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

func TestFormatLTMEntry_StripsExistingDatePrefixes(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{"single existing prefix", "[2026-07-11] netplan 已修正", "netplan 已修正"},
		{"stacked prefixes", "[2026-07-11] [2026-07-11] virt-install 需要 bridge.conf", "virt-install 需要 bridge.conf"},
		{"bulleted prefix", "- [2026-07-11] OVS bridge L2 連通正常", "OVS bridge L2 連通正常"},
		{"date mid-content untouched", "deployed on [2026-07-11] to prod", "deployed on [2026-07-11] to prod"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := formatLTMEntry(tc.input)
			// Exactly one "- [date] " prefix followed by the cleaned content.
			if !ltmDatePrefixRe.MatchString(got) {
				t.Fatalf("missing date prefix: %q", got)
			}
			rest := ltmDatePrefixRe.ReplaceAllString(got, "")
			if rest != tc.want {
				t.Errorf("content = %q, want %q", rest, tc.want)
			}
		})
	}
}

func TestNormalizeLTREntry_CollapsesStackedDatePrefixes(t *testing.T) {
	a := normalizeLTREntry("- [2026-07-10] [2026-07-11] bash tool blocked")
	b := normalizeLTREntry("- [2026-07-12] bash tool blocked")
	if a != b {
		t.Errorf("stacked-date entry should normalize equal: %q vs %q", a, b)
	}
}

func TestTruncateLTMDropsPartialFirstLine(t *testing.T) {
	head := "- " + strings.Repeat("x", 500) + "\n"
	var lines []string
	for range maxLTMChars / 20 {
		lines = append(lines, "- entry line padding out the file")
	}
	content := head + strings.Join(lines, "\n")
	got := TruncateLTM(content)
	if len([]rune(got)) > maxLTMChars {
		t.Fatalf("length %d exceeds cap %d", len([]rune(got)), maxLTMChars)
	}
	if !strings.HasPrefix(got, "- ") {
		t.Errorf("truncated content starts mid-line: %q", got[:min(40, len(got))])
	}
}

// A raw tail-of-string cut on a properly sectioned file (Conventions,
// Architecture, Patterns, ...) would wipe out the earliest sections first
// purely because of where they render, regardless of how much content is in
// them. TruncateLTM must instead drop entries from whichever section is
// largest, so an early section with real content survives over a late
// section that's mostly padding.
func TestTruncateLTMDropsFromLargestSectionNotEarliestSection(t *testing.T) {
	var padding []string
	for range maxLTMChars / 10 {
		padding = append(padding, "- padding entry to force truncation")
	}
	content := ltmSectionConventions + "\n- important rule to keep\n\n" +
		ltmSectionTools + "\n" + strings.Join(padding, "\n")

	got := TruncateLTM(content)
	if len([]rune(got)) > maxLTMChars {
		t.Fatalf("length %d exceeds cap %d", len([]rune(got)), maxLTMChars)
	}
	if !strings.Contains(got, "important rule to keep") {
		t.Errorf("TruncateLTM dropped the small early section instead of trimming the large late one:\n%s", got[:min(200, len(got))])
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
		// persistReflexionLesson's un-rescued lessons always end in "avoid
		// this approach" — "approach" alone used to match the Patterns
		// keyword check before Issues ever got a look, since that check ran
		// ahead of the source=="error" routing below.
		{`agent deployer: "cleanup" fails: deliverable verification failed — avoid this approach`, "error", ltmSectionIssues},
		{"context canceled", "error", ltmSectionIssues},
		// An explicit convention/rule signal still wins over the generic
		// error-source default.
		{"always retry on timeout", "error", ltmSectionConventions},
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
	os.WriteFile(histDir+"/chat_history.md", []byte("irrelevant"), 0o644)

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
