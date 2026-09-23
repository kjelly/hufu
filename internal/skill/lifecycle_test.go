package skill

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeUsageStats(t *testing.T, dir string, stats map[string]UsageStats) {
	t.Helper()
	data, err := json.MarshalIndent(stats, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, usageFileName), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLoadUsageStats_MissingFile(t *testing.T) {
	dir := t.TempDir()
	stats, err := LoadUsageStats(dir)
	if err != nil {
		t.Fatalf("LoadUsageStats: %v", err)
	}
	if len(stats) != 0 {
		t.Errorf("got %d stats, want 0", len(stats))
	}
}

func TestLoadUsageStats_ExistingFile(t *testing.T) {
	dir := t.TempDir()
	writeUsageStats(t, dir, map[string]UsageStats{
		"foo": {Name: "foo", UsedCount: 3, FirstUsed: time.Now(), LastUsed: time.Now()},
	})
	stats, err := LoadUsageStats(dir)
	if err != nil {
		t.Fatalf("LoadUsageStats: %v", err)
	}
	if len(stats) != 1 {
		t.Errorf("got %d stats, want 1", len(stats))
	}
	if stats["foo"].UsedCount != 3 {
		t.Errorf("UsedCount = %d, want 3", stats["foo"].UsedCount)
	}
}

func TestRecordUsage(t *testing.T) {
	dir := t.TempDir()
	if err := RecordUsage(dir, "foo", "agent1"); err != nil {
		t.Fatal(err)
	}
	if err := RecordUsage(dir, "foo", "agent1"); err != nil {
		t.Fatal(err)
	}
	if err := RecordUsage(dir, "foo", "agent2"); err != nil {
		t.Fatal(err)
	}
	stats, err := LoadUsageStats(dir)
	if err != nil {
		t.Fatal(err)
	}
	if stats["foo"].UsedCount != 3 {
		t.Errorf("UsedCount = %d, want 3", stats["foo"].UsedCount)
	}
	if len(stats["foo"].Agents) != 2 {
		t.Errorf("got %d agents, want 2", len(stats["foo"].Agents))
	}
}

func TestPromoteDraft(t *testing.T) {
	dir := t.TempDir()
	draftDir := filepath.Join(dir, "drafts", "draft-foo")
	if err := os.MkdirAll(draftDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(draftDir, "SKILL.md"),
		[]byte("---\nname: draft-foo\ndescription: Foo workflow\n---\n\n# Foo"), 0o644); err != nil {
		t.Fatal(err)
	}

	newPath, err := PromoteDraft(dir, "draft-foo")
	if err != nil {
		t.Fatalf("PromoteDraft: %v", err)
	}
	promoted, err := os.ReadFile(newPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(promoted) != "---\nname: foo\ndescription: Foo workflow\n---\n\n# Foo" {
		t.Errorf("promoted SKILL.md = %q, want the frontmatter name rewritten to foo", promoted)
	}

	want := filepath.Join(dir, "foo", "SKILL.md")
	if newPath != want {
		t.Errorf("PromoteDraft returned %q, want %q", newPath, want)
	}
	if _, err := os.Stat(want); err != nil {
		t.Errorf("promoted file missing: %v", err)
	}
	if _, err := os.Stat(draftDir); !os.IsNotExist(err) {
		t.Errorf("source draft dir still exists: %v", err)
	}
}

func TestPromoteDraft_NotFound(t *testing.T) {
	dir := t.TempDir()
	_, err := PromoteDraft(dir, "nonexistent")
	if err == nil {
		t.Error("expected error for nonexistent draft")
	}
}

func TestPromoteDraftRejectsRuntimeControlName(t *testing.T) {
	dir := t.TempDir()
	draftDir := filepath.Join(dir, "drafts", "draft-submit-result")
	if err := os.MkdirAll(draftDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(draftDir, "SKILL.md"),
		[]byte("---\nname: draft-submit-result\n---\n\n# Submit result"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := PromoteDraft(dir, "draft-submit-result"); err == nil {
		t.Fatal("PromoteDraft() succeeded for runtime-owned submit-result")
	}
	if _, err := os.Stat(draftDir); err != nil {
		t.Fatalf("rejected draft was moved: %v", err)
	}
}

func TestCleanDrafts_DryRun(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"old", "new"} {
		d := filepath.Join(dir, "drafts", name)
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		ts := time.Now().Add(-48 * time.Hour).UTC().Format(time.RFC3339)
		if name == "new" {
			ts = time.Now().UTC().Format(time.RFC3339)
		}
		content := "---\nname: " + name + "\ncreated_at: " + ts + "\n---\n\n# " + name
		if err := os.WriteFile(filepath.Join(d, "SKILL.md"), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	result, err := CleanDrafts(dir, CleanOpts{OlderThan: 24 * time.Hour, DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Deleted) != 1 || result.Deleted[0] != "old" {
		t.Errorf("Deleted = %v, want [old]", result.Deleted)
	}
	if _, err := os.Stat(filepath.Join(dir, "drafts", "old")); err != nil {
		t.Errorf("dry-run deleted file: %v", err)
	}
}

func TestCleanDrafts_Apply(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"old", "new"} {
		d := filepath.Join(dir, "drafts", name)
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		ts := time.Now().Add(-48 * time.Hour).UTC().Format(time.RFC3339)
		if name == "new" {
			ts = time.Now().UTC().Format(time.RFC3339)
		}
		content := "---\nname: " + name + "\ncreated_at: " + ts + "\n---\n\n# " + name
		if err := os.WriteFile(filepath.Join(d, "SKILL.md"), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	result, err := CleanDrafts(dir, CleanOpts{OlderThan: 24 * time.Hour, DryRun: false})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Deleted) != 1 {
		t.Errorf("Deleted = %v, want 1 entry", result.Deleted)
	}
	if _, err := os.Stat(filepath.Join(dir, "drafts", "old")); !os.IsNotExist(err) {
		t.Errorf("file not deleted: %v", err)
	}
}

func TestCleanDrafts_UnusedOnly(t *testing.T) {
	workspace := t.TempDir()
	skillsDir := filepath.Join(workspace, "skills")
	for _, name := range []string{"unused", "used"} {
		d := filepath.Join(skillsDir, "drafts", name)
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		ts := time.Now().Add(-48 * time.Hour).UTC().Format(time.RFC3339)
		content := "---\nname: " + name + "\ncreated_at: " + ts + "\n---\n\n# " + name
		if err := os.WriteFile(filepath.Join(d, "SKILL.md"), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeUsageStats(t, workspace, map[string]UsageStats{
		"used": {Name: "used", UsedCount: 1},
	})

	result, err := CleanDrafts(skillsDir, CleanOpts{OlderThan: 24 * time.Hour, UnusedOnly: true, DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Deleted) != 1 || result.Deleted[0] != "unused" {
		t.Errorf("Deleted = %v, want [unused]", result.Deleted)
	}
}

func writeDraft(t *testing.T, dir, name, content string) string {
	t.Helper()
	draftDir := filepath.Join(dir, "drafts", name)
	if err := os.MkdirAll(draftDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(draftDir, "SKILL.md"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return draftDir
}

func TestPromoteDraftRewritesFrontmatterNameForRuntime(t *testing.T) {
	dir := t.TempDir()
	writeDraft(t, dir, "draft-foo", "---\nname: draft-foo\ndescription: Foo workflow\ncreated_at: 2026-09-23T00:00:00Z\n---\n\n1. Build.\n2. Test.")
	if _, err := PromoteDraft(dir, "draft-foo"); err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, def := range DiscoverSkills([]string{dir}, false) {
		names = append(names, def.Name)
	}
	if len(names) != 1 || names[0] != "foo" {
		t.Fatalf("discovered skills = %v, want [foo]", names)
	}
}

func TestPromoteDraftKeepsBytesWhenNameMatches(t *testing.T) {
	dir := t.TempDir()
	content := "---\nname: tidy-imports\ndescription: Tidy imports\n---\n\n- Run goimports."
	writeDraft(t, dir, "tidy-imports", content)
	newPath, err := PromoteDraft(dir, "tidy-imports")
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(newPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != content {
		t.Fatalf("promoted bytes changed: %q", got)
	}
}

func TestPromoteDraftRejectsDraftWithoutDescription(t *testing.T) {
	dir := t.TempDir()
	draftDir := writeDraft(t, dir, "draft-bare", "---\nname: draft-bare\n---\n\n# Bare")
	if _, err := PromoteDraft(dir, "draft-bare"); err == nil || !strings.Contains(err.Error(), "description") {
		t.Fatalf("err = %v, want missing description", err)
	}
	got, err := os.ReadFile(filepath.Join(draftDir, "SKILL.md"))
	if err != nil || string(got) != "---\nname: draft-bare\n---\n\n# Bare" {
		t.Fatalf("rejected draft was modified or moved: %q err=%v", got, err)
	}
}

func TestRewriteSkillName(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		want    string
		wantErr bool
	}{
		{name: "rewrites first top-level name", raw: "---\nname: draft-a\ndescription: d\n---\nname: body stays", want: "---\nname: a\ndescription: d\n---\nname: body stays"},
		{name: "ignores indented name", raw: "---\nmeta:\n  name: nested\nname: draft-a\n---\nbody", want: "---\nmeta:\n  name: nested\nname: a\n---\nbody"},
		{name: "no name", raw: "---\ndescription: d\n---\nbody", wantErr: true},
		{name: "no frontmatter", raw: "name: draft-a", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := rewriteSkillName([]byte(tc.raw), "a")
			if tc.wantErr != (err != nil) {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if err == nil && string(got) != tc.want {
				t.Fatalf("rewrite = %q, want %q", got, tc.want)
			}
		})
	}
}
