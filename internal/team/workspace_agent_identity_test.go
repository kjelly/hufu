package team

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAgentWorkspacePathsUseOneCanonicalCaseInsensitiveKey(t *testing.T) {
	workspace := t.TempDir()
	if err := writeTaskFile(workspace, "team", "Helper", "20260804-000001", "working", "first", ""); err != nil {
		t.Fatal(err)
	}
	if err := writeTaskFile(workspace, "team", "helper", "20260804-000002", "done", "second", "ok"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(workspace, tasksDir, "team", "Helper")); !os.IsNotExist(err) {
		t.Fatalf("mixed-case task directory exists, stat error = %v", err)
	}
	entries, err := taskHistoryEntries(workspace, "team", "HELPER")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("canonical task history entries = %#v, want 2", entries)
	}

	writeLLMLog(workspace, "team", "Helper", "one\n")
	writeLLMLog(workspace, "team", "helper", "two\n")
	data, err := os.ReadFile(filepath.Join(workspace, llmLogsDir, "team", "helper", "llm.log"))
	if err != nil {
		t.Fatal(err)
	}
	if got := string(data); !strings.Contains(got, "one") || !strings.Contains(got, "two") {
		t.Fatalf("canonical LLM log = %q, want both writes", got)
	}
	if err := writeStatus(workspace, "Helper", "working", "task"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(workspace, statusDir, "helper.yml")); err != nil {
		t.Fatalf("canonical status file: %v", err)
	}
}

func TestTaskHistoryEntriesMergeLegacyCaseVariantsDeterministically(t *testing.T) {
	workspace := t.TempDir()
	if err := writeTaskFile(workspace, "team", "helper", "20260804-000002", "done", "canonical", "ok"); err != nil {
		t.Fatal(err)
	}
	legacyDir := filepath.Join(workspace, tasksDir, "team", "Helper")
	if err := os.MkdirAll(legacyDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacyDir, "20260804-000003.md"), []byte("legacy"), 0o644); err != nil {
		t.Fatal(err)
	}

	entries, err := taskHistoryEntries(workspace, "team", "helper")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 || entries[0].name != "20260804-000003.md" || entries[1].name != "20260804-000002.md" {
		t.Fatalf("merged history = %#v, want newest legacy then canonical", entries)
	}
}

func TestCanonicalAgentWorkspaceKeyRejectsUnsafeNames(t *testing.T) {
	for _, name := range []string{"", "../helper", "/tmp/helper", "helper/child", "helper\\child"} {
		if _, err := canonicalAgentWorkspaceKey(name); err == nil {
			t.Fatalf("canonicalAgentWorkspaceKey(%q) succeeded, want error", name)
		}
	}
}
