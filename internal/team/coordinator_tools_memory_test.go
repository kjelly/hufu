package team

import (
	"context"
	"testing"

	"charm.land/fantasy"
)

func TestCanonicalMemoryQueryFallsBackWhenContextRepositoryIsUnavailable(t *testing.T) {
	tool := &canonicalMemoryQueryTool{coordinator: &Coordinator{}}
	defer func() {
		if recovered := recover(); recovered != nil {
			t.Fatalf("memory_query panicked without a canonical repository: %v", recovered)
		}
	}()
	response, err := tool.Run(context.Background(), fantasy.ToolCall{Input: `{"query":"previous finding"}`})
	if err != nil {
		t.Fatal(err)
	}
	if !response.IsError {
		t.Fatal("expected legacy memory fallback response when no memory store is configured")
	}
}
