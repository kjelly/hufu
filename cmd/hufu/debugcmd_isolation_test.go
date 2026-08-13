package main

import (
	"archive/tar"
	"compress/gzip"

	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDebugCmd_RunIsolation(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "TestDebugCmd_RunIsolation")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	workspaceDir := filepath.Join(tmpDir, "workspace")
	if err := os.MkdirAll(filepath.Join(workspaceDir, "logs", "task-output"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(workspaceDir, "logs", "artifacts", "meta"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(workspaceDir, "logs", "artifacts", "data"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(workspaceDir, "runtime", "receipts"), 0755); err != nil {
		t.Fatal(err)
	}

	// Write events for two runs
	events := `{"run_id":"run-A", "task_id":"task-1", "attempt": 1}
{"run_id":"run-B", "task_id":"task-1", "attempt": 1}
{"run_id":"run-B", "task_id":"task-2", "attempt": 1}
`
	if err := os.WriteFile(filepath.Join(workspaceDir, "logs", "execution-events.jsonl"), []byte(events), 0644); err != nil {
		t.Fatal(err)
	}

	// Write transcripts for both runs
	os.WriteFile(filepath.Join(workspaceDir, "logs", "task-output", "task-1-run-A-attempt-1.jsonl"), []byte(`transcript A`), 0644)
	os.WriteFile(filepath.Join(workspaceDir, "logs", "task-output", "task-1-run-B-attempt-1.jsonl"), []byte(`transcript B1`), 0644)
	os.WriteFile(filepath.Join(workspaceDir, "logs", "task-output", "task-2-run-B-attempt-1.jsonl"), []byte(`transcript B2`), 0644)

	// Write journal entries using realistic RunID and toolCallIDs
	journal := `{"task_id":"task-1", "run_id": "run-A", "typed_result": {"receipt_ids": ["call-123"]}}
{"task_id":"task-1", "run_id": "run-B", "typed_result": {"receipt_ids": ["call-456"]}}
{"task_id":"task-2", "run_id": "run-B", "typed_result": {"receipt_ids": ["call-789"]}}
{"task_id":"task-1", "typed_result": {"receipt_ids": ["legacy"]}}
`
	if err := os.WriteFile(filepath.Join(workspaceDir, "logs", "task_journal.jsonl"), []byte(journal), 0644); err != nil {
		t.Fatal(err)
	}

	// Write global files that should NOT be in run-scoped debug bundles
	os.WriteFile(filepath.Join(workspaceDir, "session.json"), []byte(`{"version": 1}`), 0644)
	os.WriteFile(filepath.Join(workspaceDir, "stm.md"), []byte(`some memory`), 0644)
	os.WriteFile(filepath.Join(workspaceDir, "logs", "evidence_manifest.json"), []byte(`{"manifest": true}`), 0644)

	// Write artifacts for both runs
	os.WriteFile(filepath.Join(workspaceDir, "logs", "artifacts", "meta", "artA.json"), []byte(`{"run_id":"run-A"}`), 0644)
	os.MkdirAll(filepath.Join(workspaceDir, "logs", "artifacts", "data", "artA"), 0755)
	os.WriteFile(filepath.Join(workspaceDir, "logs", "artifacts", "meta", "artB.json"), []byte(`{"run_id":"run-B"}`), 0644)
	os.MkdirAll(filepath.Join(workspaceDir, "logs", "artifacts", "data", "artB"), 0755)

	// Write runtime contexts
	os.WriteFile(filepath.Join(workspaceDir, "runtime", "receipts", "run-A-context.json"), []byte(`{"run_id":"run-A"}`), 0644)
	os.WriteFile(filepath.Join(workspaceDir, "runtime", "receipts", "run-B-context.json"), []byte(`{"run_id":"run-B"}`), 0644)

	// Run command
	cmd := newRootCommand()
	cmd.SetArgs([]string{"debug", "run-A", "-w", workspaceDir})

	originalWD, _ := os.Getwd()
	os.Chdir(tmpDir)
	defer os.Chdir(originalWD)

	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}

	// Verify tar contents
	f, err := os.Open(filepath.Join(tmpDir, "hufu-debug-run-A.tar.gz"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz, _ := gzip.NewReader(f)
	defer gz.Close()
	tr := tar.NewReader(gz)

	foundA := false
	foundAContext := false
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		name := hdr.Name
		if strings.Contains(name, "run-B") || strings.Contains(name, "artB") || strings.Contains(name, "task-2") {
			t.Errorf("bundle should not contain run-B data: %s", name)
		}
		if name == "session.json" || name == "stm.md" || name == "ltm.md" || name == "logs/evidence_manifest.json" {
			t.Errorf("bundle should not contain global file: %s", name)
		}
		if name == "runtime/receipts/run-B-context.json" {
			t.Errorf("bundle should not contain run-B context")
		}
		if name == "runtime/receipts/run-A-context.json" {
			foundAContext = true
		}
		if name == "bundle-manifest.json" {
			b, _ := io.ReadAll(tr)
			content := string(b)
			if !strings.Contains(content, "1 unscoped legacy rows omitted") {
				t.Errorf("manifest missing legacy row omission report, got %s", content)
			}
		}
		if strings.Contains(name, "task_journal.jsonl") {
			b, _ := io.ReadAll(tr)
			content := string(b)
			if strings.Contains(content, "run-B") {
				t.Errorf("task_journal.jsonl contains run-B content")
			}
			if strings.Contains(content, "legacy") {
				t.Errorf("task_journal.jsonl contains legacy content")
			}
			if !strings.Contains(content, "run-A") {
				t.Errorf("task_journal.jsonl missing run-A content")
			}
		}
		if strings.Contains(name, "run-A") {
			foundA = true
		}
	}
	if !foundA {
		t.Errorf("bundle missing run-A data")
	}
	if !foundAContext {
		t.Errorf("bundle missing run-A context")
	}
}
