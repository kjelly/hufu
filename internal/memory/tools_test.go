package memory

import (
	"strings"
	"testing"

	"charm.land/fantasy"
)

func TestMemorySaveToolInfo(t *testing.T) {
	tool := NewMemorySaveTool(nil)
	info := tool.Info()
	if info.Name != "memory_save" {
		t.Errorf("expected tool name 'memory_save', got %q", info.Name)
	}
	if len(info.Required) != 1 || info.Required[0] != "content" {
		t.Errorf("expected required=[content], got %v", info.Required)
	}
}

func TestMemoryQueryToolInfo(t *testing.T) {
	tool := NewMemoryQueryTool(nil)
	info := tool.Info()
	if info.Name != "memory_query" {
		t.Errorf("expected tool name 'memory_query', got %q", info.Name)
	}
	if len(info.Required) != 1 || info.Required[0] != "query" {
		t.Errorf("expected required=[query], got %v", info.Required)
	}
}

func TestMemorySaveToolNilStore(t *testing.T) {
	tool := &memorySaveTool{store: nil}
	resp, err := tool.Run(nil, fantasy.ToolCall{Input: `{"content": "test knowledge"}`})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(resp.Content, "not available") {
		t.Errorf("expected 'not available' message, got: %s", resp.Content)
	}
}

func TestMemoryQueryToolNilStore(t *testing.T) {
	tool := &memoryQueryTool{store: nil}
	resp, err := tool.Run(nil, fantasy.ToolCall{Input: `{"query": "test"}`})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(resp.Content, "not available") {
		t.Errorf("expected 'not available' message, got: %s", resp.Content)
	}
}