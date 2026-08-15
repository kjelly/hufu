package team

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"charm.land/fantasy"

	"github.com/kjelly/hufu/internal/agent"
	contextstore "github.com/kjelly/hufu/internal/context"
)

func TestCompatibilityMemoryMutationsFailClosedWithoutCanonicalRepository(t *testing.T) {
	workspace := t.TempDir()
	c := &Coordinator{projectDir: "project", session: &TeamSession{Workspace: workspace, Config: agent.TeamConfig{Name: "team"}}}
	cases := []struct {
		name  string
		tool  fantasy.AgentTool
		input string
	}{
		{"stm_write", &stmWriteTool{coordinator: c}, `{"content":"secret finding","kind":"observation"}`},
		{"ltm_update", &ltmUpdateTool{coordinator: c}, `{"content":"secret finding","section":"# 常見模式"}`},
		{"memory_save", &memorySaveLTMWrapper{coordinator: c}, `{"content":"secret finding"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			response, err := tc.tool.Run(context.Background(), fantasy.ToolCall{Input: tc.input})
			if err != nil || !response.IsError || response.Content != "canonical context unavailable" {
				t.Fatalf("response=%#v err=%v", response, err)
			}
		})
	}
	for _, name := range []string{"stm.md", "ltm.md", "context-stm.md", "context-ltm.md"} {
		if _, err := os.Stat(filepath.Join(workspace, name)); !os.IsNotExist(err) {
			t.Fatalf("fail-closed alias created %s: %v", name, err)
		}
	}
}

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
	save := &memorySaveLTMWrapper{coordinator: c}
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

func TestCanonicalMemoryQueryFailsClosedWhenContextRepositoryIsUnavailable(t *testing.T) {
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
		t.Fatal("expected canonical-unavailable response")
	}
}

func TestMemorySaveSchemaAdvertisesAtomicSupersedes(t *testing.T) {
	tool := &memorySaveLTMWrapper{}
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
	tool := &memorySaveLTMWrapper{coordinator: c}
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
	save := &memorySaveLTMWrapper{coordinator: c}
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

func TestMemorySaveProjectionRebuildFailureRetainsCanonicalWriteAndNoDuplicateCandidate(t *testing.T) {
	workspace := t.TempDir()
	repo, err := contextstore.OpenSQLite(filepath.Join(workspace, "context.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	// Force rebuildLegacyContextProjections to fail by making the
	// workspace a path that already exists as a regular file. The
	// projection writes stm.md / ltm-TEAM.md into the workspace, so
	// opening a file path will fail the projection step while the
	// canonical SQLite write still succeeds.
	notADir := filepath.Join(workspace, "not-a-dir")
	if err := os.WriteFile(notADir, []byte("block"), 0o644); err != nil {
		t.Fatal(err)
	}
	c := &Coordinator{
		contextRepo: repo, projectDir: "project", executionRunID: "run-1",
		session: &TeamSession{Workspace: notADir, Config: agent.TeamConfig{Name: "team"}},
	}
	save := &memorySaveLTMWrapper{coordinator: c}

	first, err := save.Run(context.Background(), fantasy.ToolCall{Input: `{"content":"first attempt","confidence":0.5}`})
	if err != nil {
		t.Fatal(err)
	}
	if first.IsError {
		t.Fatalf("memory_save first attempt returned error response after a successful canonical write: %#v", first)
	}
	if !strings.Contains(first.Content, "projection rebuild reported an error") {
		t.Fatalf("expected projection-warning message, got %q", first.Content)
	}
	items, err := repo.Query(context.Background(), contextstore.RepositoryQuery{Scope: contextstore.Scope{ProjectID: "project", TeamID: "team"}, Visibility: contextstore.VisibilityExact, IncludeCandidates: true})
	if err != nil || len(items) != 1 {
		t.Fatalf("items after first attempt = %#v, err=%v; want exactly one canonical candidate", items, err)
	}
	firstID := items[0].ID
	if items[0].Lifecycle == "" {
		t.Fatalf("memory_save projection-failure path left candidate with empty lifecycle: %#v", items[0])
	}

	// The model's retry invariant: a second call with the same content
	// must not produce a duplicate candidate. The proposal pipeline
	// performs content-hash dedup; a regression that fails-closed on
	// projection rebuild would create a second candidate.
	second, err := save.Run(context.Background(), fantasy.ToolCall{Input: `{"content":"first attempt","confidence":0.5}`})
	if err != nil {
		t.Fatal(err)
	}
	if second.IsError != first.IsError {
		t.Fatalf("retry response disposition mismatch: first.IsError=%v second.IsError=%v", first.IsError, second.IsError)
	}
	items, err = repo.Query(context.Background(), contextstore.RepositoryQuery{Scope: contextstore.Scope{ProjectID: "project", TeamID: "team"}, Visibility: contextstore.VisibilityExact, IncludeCandidates: true})
	if err != nil {
		t.Fatal(err)
	}
	matches := 0
	var match *contextstore.ContextItem
	for i := range items {
		if items[i].Content == "first attempt" {
			matches++
			match = &items[i]
		}
	}
	if matches != 1 {
		t.Fatalf("retry produced duplicate candidates: matches=%d items=%#v", matches, items)
	}
	if match != nil && match.ID != firstID {
		t.Fatalf("retry superseded the original candidate unexpectedly: firstID=%s latestID=%s", firstID, match.ID)
	}
}
