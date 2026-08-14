package team

import (
	"context"
	"path/filepath"
	"testing"

	"charm.land/fantasy"

	"github.com/kjelly/hufu/internal/agent"
	contextstore "github.com/kjelly/hufu/internal/context"
	"github.com/kjelly/hufu/internal/memory"
)

func TestMemorySavePreservesExplicitZeroConfidence(t *testing.T) {
	workspace := t.TempDir()
	repo, err := contextstore.OpenSQLite(filepath.Join(workspace, "context.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	c := &Coordinator{
		contextRepo: repo, projectDir: "project", executionRunID: "run-1",
		session: &TeamSession{Workspace: workspace, Config: agent.TeamConfig{Name: "team"}},
	}
	save := &memorySaveLTMWrapper{original: memory.NewMemorySaveTool(nil), coordinator: c}
	response, err := save.Run(context.Background(), fantasy.ToolCall{Input: `{"content":"low-trust fact","confidence":0}`})
	if err != nil || response.IsError {
		t.Fatalf("memory_save: response=%#v err=%v", response, err)
	}
	items, err := repo.Query(context.Background(), contextstore.RepositoryQuery{Scope: contextstore.Scope{ProjectID: "project", TeamID: "team"}, Visibility: contextstore.VisibilityExact, IncludeCandidates: true})
	if err != nil || len(items) != 1 {
		t.Fatalf("saved items=%#v err=%v", items, err)
	}
	if items[0].Confidence != 0 {
		t.Fatalf("explicit confidence 0 was rewritten to %v", items[0].Confidence)
	}
}

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

func TestMemorySaveSchemaAdvertisesAtomicSupersedes(t *testing.T) {
	tool := &memorySaveLTMWrapper{original: memory.NewMemorySaveTool(nil)}
	if _, ok := tool.Info().Parameters["supersedes"]; !ok {
		t.Fatal("memory_save does not advertise its supported atomic supersedes parameter")
	}
}

func TestMemorySaveRejectsUnknownCategory(t *testing.T) {
	workspace := t.TempDir()
	repo, err := contextstore.OpenSQLite(filepath.Join(workspace, "context.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	c := &Coordinator{contextRepo: repo, projectDir: "project", executionRunID: "run-1", session: &TeamSession{Workspace: workspace, Config: agent.TeamConfig{Name: "team"}}}
	tool := &memorySaveLTMWrapper{original: memory.NewMemorySaveTool(nil), coordinator: c}
	response, err := tool.Run(context.Background(), fantasy.ToolCall{Input: `{"content":"ambiguous fact","category":"api-discovery"}`})
	if err != nil || !response.IsError {
		t.Fatalf("unknown category = %#v, %v; want validation error", response, err)
	}
}

func TestCanonicalMemorySavePreservesContractFieldsAndQueryFilters(t *testing.T) {
	workspace := t.TempDir()
	repo, err := contextstore.OpenSQLite(filepath.Join(workspace, "context.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	c := &Coordinator{
		contextRepo: repo, projectDir: "project", executionRunID: "run-1",
		session: &TeamSession{Workspace: workspace, Config: agent.TeamConfig{Name: "team"}},
	}
	save := &memorySaveLTMWrapper{original: memory.NewMemorySaveTool(nil), coordinator: c}
	response, err := save.Run(context.Background(), fantasy.ToolCall{Input: `{"content":"use the database adapter","category":"architecture","confidence":0.7,"file_paths":["internal/db/adapter.go"]}`})
	if err != nil || response.IsError {
		t.Fatalf("memory_save: response=%#v err=%v", response, err)
	}
	items, err := repo.Query(context.Background(), contextstore.RepositoryQuery{Scope: contextstore.Scope{ProjectID: "project", TeamID: "team"}, Visibility: contextstore.VisibilityExact, IncludeCandidates: true})
	if err != nil || len(items) != 1 {
		t.Fatalf("saved items=%#v err=%v", items, err)
	}
	item := items[0]
	if item.Metadata["category"] != "architecture" || item.Confidence != 0.7 {
		t.Fatalf("memory_save lost category/confidence: %#v", item)
	}
	foundPath := false
	for _, evidence := range item.Evidence {
		if evidence.Type == "file_path" && evidence.Ref == "internal/db/adapter.go" {
			foundPath = true
		}
	}
	if !foundPath {
		t.Fatalf("memory_save lost file path evidence: %#v", item.Evidence)
	}
	if err := repo.UpdateLifecycle(context.Background(), []string{item.ID}, contextstore.LifecycleConfirmed); err != nil {
		t.Fatal(err)
	}
	query := &canonicalMemoryQueryTool{coordinator: c}
	response, err = query.Run(context.Background(), fantasy.ToolCall{Input: `{"query":"adapter","category":"architecture","min_confidence":0.7,"file_paths":["internal/db/adapter.go"]}`})
	if err != nil || response.IsError || response.Content == "No relevant memories found." {
		t.Fatalf("memory_query filters did not return saved memory: response=%#v err=%v", response, err)
	}
	response, err = query.Run(context.Background(), fantasy.ToolCall{Input: `{"query":"adapter","min_confidence":0.8}`})
	if err != nil || response.IsError || response.Content != "No relevant memories found." {
		t.Fatalf("memory_query ignored min_confidence: response=%#v err=%v", response, err)
	}
}
