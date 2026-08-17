package tools

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"charm.land/fantasy"
)

func TestWriteHandlerDeniesSideEffectNoneBeforeFilesystemMutation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "must-not-exist.txt")
	tool := NewWriteTool()
	resp, err := tool.Run(context.WithValue(context.Background(), AgentReadOnlyExecutionKey, true), fantasy.ToolCall{
		Name:  "write",
		Input: `{"file_path":"` + path + `","content":"secret"}`,
	})
	if err != nil {
		t.Fatalf("write returned error: %v", err)
	}
	if !resp.IsError {
		t.Fatalf("read-only write should return a tool error: %#v", resp)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("read-only write created %q: %v", path, err)
	}
}
