package team

import (
	"os"
	"path/filepath"
	"strings"
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

func TestTruncateSTMPreservesCriticalSectionsAndAnchors(t *testing.T) {
	content := "# 進度\n- " + strings.Repeat("routine progress ", 300) + "\n\n" +
		"# 錯誤與修復\n- " + strings.Repeat("noise ", 250) + "ERROR: database migration failed; run `go test ./internal/team/...`; inspect /srv/hufu/migrations/0042.sql\n\n" +
		"# 決策\n- Use atomic STM writes\n\n# 待解決\n- Resolve migration failure"
	got := TruncateSTM(content)
	if len([]rune(got)) > maxSTMChars {
		t.Fatalf("length = %d, want <= %d", len([]rune(got)), maxSTMChars)
	}
	for _, want := range []string{stmSectionErrors, stmSectionDecisions, stmSectionQuestions, "go test ./internal/team/...", "/srv/hufu/migrations/0042.sql", "ERROR: database migration failed"} {
		if !strings.Contains(got, want) {
			t.Errorf("truncated STM missing %q:\n%s", want, got)
		}
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

func TestInitSTMClearsExistingContent(t *testing.T) {
	dir := t.TempDir()
	SaveSTM(dir, "previous session content")
	if err := InitSTM(dir); err != nil {
		t.Fatalf("InitSTM() error: %v", err)
	}
	got := LoadSTM(dir)
	if got != "" {
		t.Errorf("InitSTM() should reset stm.md: got %q", got)
	}
}

func TestSTMPath(t *testing.T) {
	got := STMPath("/tmp/ws")
	want := filepath.Join("/tmp/ws", "stm.md")
	if got != want {
		t.Errorf("STMPath() = %q, want %q", got, want)
	}
}

func TestParseSTMSections(t *testing.T) {
	content := "# 進度\n- researcher: checked login.go\n- writer: drafted report\n\n# 發現\n- SQL injection in login.go:123\n\n# 決策\n- Use prepared statements"
	sections := ParseSTMSections(content)
	if len(sections) != 3 {
		t.Fatalf("ParseSTMSections() len = %d, want 3", len(sections))
	}
	if sections[0].Title != "# 進度" {
		t.Errorf("first section title = %q, want %q", sections[0].Title, "# 進度")
	}
	if len(sections[0].Entries) != 2 {
		t.Errorf("first section entries = %d, want 2", len(sections[0].Entries))
	}
	if len(sections[2].Entries) != 1 {
		t.Errorf("last section entries = %d, want 1", len(sections[2].Entries))
	}
}

func TestParseSTMSectionsEmpty(t *testing.T) {
	sections := ParseSTMSections("")
	if len(sections) != 0 {
		t.Errorf("ParseSTMSections(\"\") len = %d, want 0", len(sections))
	}
}

func TestFormatSTMSections(t *testing.T) {
	sections := []STMSection{
		{Title: "# 進度", Entries: []string{"- task1", "- task2"}},
		{Title: "# 發現", Entries: []string{"- found something"}},
	}
	got := FormatSTMSections(sections)
	if got == "" {
		t.Fatal("FormatSTMSections() returned empty string")
	}
	if !contains(got, "# 進度") || !contains(got, "# 發現") {
		t.Errorf("FormatSTMSections() missing section headers: %s", got)
	}
}

func TestAppendSTMEntry(t *testing.T) {
	content := "# 進度\n- old entry"
	newContent := appendSTMEntry(content, "- new entry", "# 進度")
	if !contains(newContent, "- new entry") {
		t.Errorf("appendSTMEntry missing new entry: %s", newContent)
	}
	if !contains(newContent, "- old entry") {
		t.Errorf("appendSTMEntry lost old entry: %s", newContent)
	}
}

func TestAppendSTMEntryNewSection(t *testing.T) {
	content := "# 進度\n- task1"
	newContent := appendSTMEntry(content, "- found bug", "# 發現")
	if !contains(newContent, "# 發現") {
		t.Errorf("appendSTMEntry should create new section: %s", newContent)
	}
	if !contains(newContent, "# 進度") {
		t.Errorf("appendSTMEntry lost existing section: %s", newContent)
	}
}

func TestAppendSTMEntryEmpty(t *testing.T) {
	newContent := appendSTMEntry("", "- first", "# 進度")
	if !contains(newContent, "- first") {
		t.Errorf("appendSTMEntry on empty: %s", newContent)
	}
}

func TestFormatSTMDoneEntry(t *testing.T) {
	got := formatSTMDoneEntry("researcher", "check security", "found SQL injection")
	if got == "" {
		t.Fatal("formatSTMDoneEntry returned empty")
	}
	if !contains(got, "researcher") || !contains(got, "check security") {
		t.Errorf("formatSTMDoneEntry: %s", got)
	}
}

func TestFormatSTMErrorEntry(t *testing.T) {
	got := formatSTMErrorEntry("writer", "draft report", "timeout")
	if got == "" {
		t.Fatal("formatSTMErrorEntry returned empty")
	}
	if !contains(got, "FAILED") || !contains(got, "timeout") {
		t.Errorf("formatSTMErrorEntry: %s", got)
	}
}

func TestFilterSTMSectionsByRole(t *testing.T) {
	sections := []STMSection{
		{Title: "# 進度", Entries: []string{"- task1"}},
		{Title: "# 發現", Entries: []string{"- found bug"}},
		{Title: "# 錯誤與修復", Entries: []string{"- fixed crash"}},
	}

	tests := []struct {
		role       string
		wantTitles []string
	}{
		{"coordinator", []string{"# 進度", "# 發現", "# 錯誤與修復"}},
		// findings are now visible to all roles; writer/coder also see findings
		{"researcher", []string{"# 發現", "# 錯誤與修復"}},
		{"writer", []string{"# 進度", "# 發現"}},
		{"reviewer", []string{"# 進度", "# 發現", "# 錯誤與修復"}},
		// empty role is treated as coordinator-level: all sections visible
		{"", []string{"# 進度", "# 發現", "# 錯誤與修復"}},
	}

	for _, tt := range tests {
		t.Run(tt.role, func(t *testing.T) {
			filtered := filterSTMSectionsByRole(sections, tt.role)
			if len(filtered) != len(tt.wantTitles) {
				t.Errorf("filterSTMSectionsByRole(%q) len = %d, want %d", tt.role, len(filtered), len(tt.wantTitles))
			}
			for i, s := range filtered {
				if i < len(tt.wantTitles) && s.Title != tt.wantTitles[i] {
					t.Errorf("filterSTMSectionsByRole(%q)[%d] title = %q, want %q", tt.role, i, s.Title, tt.wantTitles[i])
				}
			}
		})
	}
}

func TestFilterSTMSectionsByRole_DecisionsAndQuestionsAlwaysVisible(t *testing.T) {
	sections := []STMSection{
		{Title: "# 決策", Entries: []string{"- use postgres"}},
		{Title: "# 待解決", Entries: []string{"- which auth provider?"}},
	}
	for _, role := range []string{"researcher", "writer", "coder", "reviewer", "tester", "explorer", ""} {
		filtered := filterSTMSectionsByRole(sections, role)
		if len(filtered) != 2 {
			t.Errorf("role %q: decisions and questions must be visible, got %d sections", role, len(filtered))
		}
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && (findSubstring(s, substr)))
}

func findSubstring(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
