package memory

import (
	"context"
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
	resp, err := tool.Run(context.TODO(), fantasy.ToolCall{Input: `{"content": "test knowledge"}`})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(resp.Content, "not available") {
		t.Errorf("expected 'not available' message, got: %s", resp.Content)
	}
}

func TestMemoryQueryToolNilStore(t *testing.T) {
	tool := &memoryQueryTool{store: nil}
	resp, err := tool.Run(context.TODO(), fantasy.ToolCall{Input: `{"query": "test"}`})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(resp.Content, "not available") {
		t.Errorf("expected 'not available' message, got: %s", resp.Content)
	}
}

func TestMemoryToolsExecutionWithStore(t *testing.T) {
	ctx := context.Background()
	store := setupTestStore(t)

	saveTool := NewMemorySaveTool(store)
	queryTool := NewMemoryQueryTool(store)

	// Save a memory item
	saveResp, err := saveTool.Run(ctx, fantasy.ToolCall{
		Input: `{"content": "Database pool size is 20", "category": "config", "confidence": 0.95}`,
	})
	if err != nil {
		t.Fatalf("saveTool Run error: %v", err)
	}
	if !strings.Contains(saveResp.Content, "Saved to memory") {
		t.Errorf("unexpected save response: %s", saveResp.Content)
	}

	// Query memory
	queryResp, err := queryTool.Run(ctx, fantasy.ToolCall{
		Input: `{"query": "database pool size", "category": "config"}`,
	})
	if err != nil {
		t.Fatalf("queryTool Run error: %v", err)
	}
	if !strings.Contains(queryResp.Content, "Database pool size is 20") {
		t.Errorf("query response missing expected content, got:\n%s", queryResp.Content)
	}
}
