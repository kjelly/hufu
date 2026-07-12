package team

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestExecutionEventLoggerWritesStructuredEvent(t *testing.T) {
	workspace := t.TempDir()
	logger, err := newExecutionEventLogger(workspace)
	if err != nil {
		t.Fatal(err)
	}
	event := ExecutionEvent{Version: 1, Timestamp: "2026-07-12T12:00:00Z", RunID: "run-test", Team: "dev", TaskID: "42", Agent: "developer", Attempt: 2, Status: "done", Usage: ExecutionUsage{InputTokens: 3, OutputTokens: 5, TotalTokens: 8}}
	if err := logger.append(event); err != nil {
		t.Fatal(err)
	}
	logger.close()
	f, err := os.Open(filepath.Join(workspace, "logs", executionEventsFile))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	if !scanner.Scan() {
		t.Fatal("expected an event")
	}
	var got ExecutionEvent
	if err := json.Unmarshal(scanner.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.RunID != event.RunID || got.TaskID != event.TaskID || got.Attempt != 2 || got.Usage.TotalTokens != 8 {
		t.Fatalf("event = %+v", got)
	}
}

func TestTeamDefinitionRevisionChangesWithDefinition(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "team.yaml"), []byte("name: dev\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "developer.md"), []byte("first"), 0o644); err != nil {
		t.Fatal(err)
	}
	first := teamDefinitionRevision(dir)
	if first == "" {
		t.Fatal("expected a definition revision")
	}
	if err := os.WriteFile(filepath.Join(dir, "developer.md"), []byte("second"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := teamDefinitionRevision(dir); got == first {
		t.Fatalf("revision = %q, want a new hash after definition change", got)
	}
}
