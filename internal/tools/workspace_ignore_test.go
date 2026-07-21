package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"charm.land/fantasy"
)

func TestIsWorkspaceRecordPath(t *testing.T) {
	tests := []struct {
		name   string
		path   string
		wsName string
		want   bool
	}{
		{"session history under team dir", "workspace/default/session_history.json", "workspace", true},
		{"compaction history under team dir", "workspace/default/compaction_history.json", "workspace", true},
		{"session json under team dir", "workspace/default/session.json", "workspace", true},
		{"session json directly under workspace", "workspace/session.json", "workspace", true},
		{"task journal", "workspace/default/logs/task_journal.jsonl", "workspace", true},
		{"llm log", "workspace/default/logs/llm/team/agent/llm.log", "workspace", true},
		{"any file under logs/llm", "workspace/default/logs/llm/team/agent/other.txt", "workspace", true},
		{"audit jsonl", "workspace/default/logs/audit/audit-2026-07-12.jsonl", "workspace", true},
		{"stm round file", "workspace/default/logs/stm/stm_r3.md", "workspace", true},
		{"absolute path", "/home/user/proj/workspace/default/session_history.json", "workspace", true},
		{"empty wsName falls back to default", "workspace/default/llm.log", "", true},
		{"regular file under workspace kept", "workspace/default/notes.md", "workspace", false},
		{"session json outside workspace kept", "src/session.json", "workspace", false},
		{"llm log outside workspace kept", "vendor/llm.log", "workspace", false},
		{"partial dir name does not match", "myworkspace/default/session.json", "workspace", false},
		{"workspace dir itself", "workspace", "workspace", false},
		{"custom workspace name matches", "ws/default/session_history.json", "ws", true},
		{"custom workspace name ignores default", "workspace/default/session_history.json", "ws", false},
		{"stm-like name outside stm glob kept", "workspace/default/storm.md", "workspace", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isWorkspaceRecordPath(tt.path, tt.wsName); got != tt.want {
				t.Errorf("isWorkspaceRecordPath(%q, %q) = %v, want %v", tt.path, tt.wsName, got, tt.want)
			}
		})
	}
}

func TestFilterWorkspaceRecordLines(t *testing.T) {
	tests := []struct {
		name string
		line string
		kept bool
	}{
		{"match line in record dropped", "workspace/default/session_history.json:3:needle", false},
		{"context line in record dropped", "workspace/default/logs/llm/t/a/llm.log-5-ctx", false},
		{"match line in source kept", "src/main.go:10:needle", true},
		{"regular workspace file kept", "workspace/default/notes.md:1:needle", true},
		{"separator line kept", "--", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := filterWorkspaceRecordLines(tt.line, "workspace")
			if tt.kept && got != tt.line {
				t.Errorf("filterWorkspaceRecordLines(%q) = %q, want line kept", tt.line, got)
			}
			if !tt.kept && got != "" {
				t.Errorf("filterWorkspaceRecordLines(%q) = %q, want line dropped", tt.line, got)
			}
		})
	}
}

// writeWorkspaceFixture creates a project tree containing normal files and
// hufu workspace execution records, all containing the word "needle".
func writeWorkspaceFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	files := []string{
		"main.go",
		"data/config.json",
		"workspace/session.json",
		"workspace/default/notes.md",
		"workspace/default/session_history.json",
		"workspace/default/session.json",
		"workspace/default/compaction_history.json",
		"workspace/default/logs/task_journal.jsonl",
		"workspace/default/logs/llm/team/agent/llm.log",
		"workspace/default/logs/audit/audit-2026-07-12.jsonl",
		"workspace/default/logs/stm/stm_r1.md",
	}
	for _, f := range files {
		path := filepath.Join(root, f)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("creating fixture dir: %v", err)
		}
		if err := os.WriteFile(path, []byte("needle in "+f+"\n"), 0o644); err != nil {
			t.Fatalf("writing fixture file: %v", err)
		}
	}
	return root
}

var workspaceRecordFixtureNames = []string{
	"session_history.json",
	"session.json",
	"compaction_history.json",
	"task_journal.jsonl",
	"llm.log",
	"audit-2026-07-12.jsonl",
	"stm_r1.md",
}

func TestGrepExcludesWorkspaceRecords(t *testing.T) {
	root := writeWorkspaceFixture(t)

	resp, err := executeGrep(context.Background(), fantasy.ToolCall{Input: `{"pattern":"needle"}`}, root, ToolConfig{WorkDir: root})
	if err != nil {
		t.Fatalf("executeGrep: %v", err)
	}

	for _, want := range []string{"main.go", "config.json", "notes.md"} {
		if !strings.Contains(resp.Content, want) {
			t.Errorf("grep output missing regular file %q:\n%s", want, resp.Content)
		}
	}
	for _, record := range workspaceRecordFixtureNames {
		if strings.Contains(resp.Content, record) {
			t.Errorf("grep output contains workspace record %q:\n%s", record, resp.Content)
		}
	}
}

func TestGrepInsideWorkspaceBypassesExclusion(t *testing.T) {
	root := writeWorkspaceFixture(t)
	wsDir := filepath.Join(root, "workspace", "default")

	resp, err := executeGrep(context.Background(), fantasy.ToolCall{Input: `{"pattern":"needle"}`}, wsDir, ToolConfig{WorkDir: wsDir})
	if err != nil {
		t.Fatalf("executeGrep: %v", err)
	}

	for _, record := range []string{"session_history.json", "llm.log", "task_journal.jsonl", "compaction_history.json"} {
		if !strings.Contains(resp.Content, record) {
			t.Errorf("explicit workspace search missing record %q:\n%s", record, resp.Content)
		}
	}
}

func TestGrepCompactionHistoryPathFiltersOutsideWorkspace(t *testing.T) {
	root := t.TempDir()
	compactionPath := filepath.Join(root, "workspace", "default", "compaction_history.json")
	regularPath := filepath.Join(root, "outside.txt")

	for _, p := range []string{compactionPath, regularPath} {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatalf("creating parent dir for %q: %v", p, err)
		}
	}

	compactionContent := "compaction-history-search-keyword: keep-workspace-private\n"
	regularContent := "compaction-history-search-keyword: visible-outside\n"

	if err := os.WriteFile(compactionPath, []byte(compactionContent), 0o644); err != nil {
		t.Fatalf("writing compaction file: %v", err)
	}
	if err := os.WriteFile(regularPath, []byte(regularContent), 0o644); err != nil {
		t.Fatalf("writing outside file: %v", err)
	}

	resp, err := executeGrep(
		context.Background(),
		fantasy.ToolCall{Input: `{"pattern":"compaction-history-search-keyword"}`},
		root,
		ToolConfig{WorkDir: root},
	)
	if err != nil {
		t.Fatalf("executeGrep: %v", err)
	}

	if strings.Contains(resp.Content, "compaction_history.json") {
		t.Fatalf("compaction_history.json should be excluded in workspace-agnostic grep\n%s", resp.Content)
	}
	if strings.Contains(resp.Content, compactionContent) {
		t.Fatalf("compaction history content should not be visible in workspace-agnostic grep\n%s", resp.Content)
	}
	if !strings.Contains(resp.Content, regularContent) {
		t.Fatalf("expected outside workspace match in grep output, got: %q", resp.Content)
	}
	if strings.Contains(resp.Content, "outside.txt") == false {
		t.Fatalf("expected outside.txt to be included in grep output, got: %q", resp.Content)
	}
}

func TestGrepCompactionHistoryVisibleInsideWorkspace(t *testing.T) {
	root := t.TempDir()
	wsDir := filepath.Join(root, "workspace", "default")
	compactionPath := filepath.Join(wsDir, "compaction_history.json")
	if err := os.MkdirAll(wsDir, 0o755); err != nil {
		t.Fatalf("creating workspace dir: %v", err)
	}

	compactionContent := "compaction-history-search-keyword: workspace-visible\n"
	if err := os.WriteFile(compactionPath, []byte(compactionContent), 0o644); err != nil {
		t.Fatalf("writing compaction file: %v", err)
	}

	resp, err := executeGrep(
		context.Background(),
		fantasy.ToolCall{Input: `{"pattern":"compaction-history-search-keyword"}`},
		wsDir,
		ToolConfig{WorkDir: wsDir},
	)
	if err != nil {
		t.Fatalf("executeGrep: %v", err)
	}

	if !strings.Contains(resp.Content, "compaction_history.json") {
		t.Fatalf("expected compaction_history.json in workspace-local grep output, got: %q", resp.Content)
	}
	if !strings.Contains(resp.Content, "workspace-visible") {
		t.Fatalf("expected compaction content in workspace-local grep output, got: %q", resp.Content)
	}
}

func TestGrepFallbackExcludesWorkspaceRecords(t *testing.T) {
	root := writeWorkspaceFixture(t)

	resp, err := grepFallback(context.Background(), grepArgs{Pattern: "needle"}, root, defaultGrepLimit, "workspace", true)
	if err != nil {
		t.Fatalf("grepFallback: %v", err)
	}

	for _, want := range []string{"main.go", "config.json", "notes.md"} {
		if !strings.Contains(resp.Content, want) {
			t.Errorf("fallback output missing regular file %q:\n%s", want, resp.Content)
		}
	}
	for _, record := range workspaceRecordFixtureNames {
		if strings.Contains(resp.Content, record) {
			t.Errorf("fallback output contains workspace record %q:\n%s", record, resp.Content)
		}
	}
}

func TestGrepTruncationNotice(t *testing.T) {
	root := t.TempDir()
	var b strings.Builder
	for i := 0; i < 150; i++ {
		fmt.Fprintf(&b, "needle line %d\n", i)
	}
	for _, f := range []string{"a.txt", "b.txt"} {
		if err := os.WriteFile(filepath.Join(root, f), []byte(b.String()), 0o644); err != nil {
			t.Fatalf("writing fixture file: %v", err)
		}
	}

	resp, err := executeGrep(context.Background(), fantasy.ToolCall{Input: `{"pattern":"needle"}`}, root, ToolConfig{WorkDir: root})
	if err != nil {
		t.Fatalf("executeGrep: %v", err)
	}

	if !strings.Contains(resp.Content, "output truncated") {
		t.Errorf("grep output missing truncation notice:\n...%s", resp.Content[max(0, len(resp.Content)-200):])
	}
}

func TestGlobExcludesWorkspaceRecords(t *testing.T) {
	root := writeWorkspaceFixture(t)

	resp, err := executeGlob(context.Background(), fantasy.ToolCall{Input: `{"pattern":"**/*.json"}`}, root, ToolConfig{WorkDir: root})
	if err != nil {
		t.Fatalf("executeGlob: %v", err)
	}

	if !strings.Contains(resp.Content, "config.json") {
		t.Errorf("glob output missing regular file config.json:\n%s", resp.Content)
	}
	for _, record := range []string{"session.json", "session_history.json", "compaction_history.json"} {
		if strings.Contains(resp.Content, record) {
			t.Errorf("glob output contains workspace record %q:\n%s", record, resp.Content)
		}
	}
}

func TestGlobWalkExcludesWorkspaceRecords(t *testing.T) {
	root := writeWorkspaceFixture(t)

	paths, _, err := globWithWalk("*.json", root, defaultGlobLimit, "workspace", true)
	if err != nil {
		t.Fatalf("globWithWalk: %v", err)
	}

	joined := strings.Join(paths, "\n")
	if !strings.Contains(joined, "config.json") {
		t.Errorf("walk output missing regular file config.json:\n%s", joined)
	}
	for _, record := range []string{"session.json", "session_history.json", "compaction_history.json"} {
		if strings.Contains(joined, record) {
			t.Errorf("walk output contains workspace record %q:\n%s", record, joined)
		}
	}
}

func TestGlobInsideWorkspaceBypassesExclusion(t *testing.T) {
	root := writeWorkspaceFixture(t)
	wsDir := filepath.Join(root, "workspace", "default")

	resp, err := executeGlob(context.Background(), fantasy.ToolCall{Input: `{"pattern":"**/*.json"}`}, wsDir, ToolConfig{WorkDir: wsDir})
	if err != nil {
		t.Fatalf("executeGlob: %v", err)
	}

	if !strings.Contains(resp.Content, "session_history.json") {
		t.Errorf("explicit workspace glob missing session_history.json:\n%s", resp.Content)
	}
}
