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
