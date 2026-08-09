package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	contextstore "github.com/kjelly/hufu/internal/context"
)

func TestContextRepairCommandNoPendingFile(t *testing.T) {
	workspace := t.TempDir()
	root := newRootCommand()
	out := new(bytes.Buffer)
	root.SetOut(out)
	root.SetArgs([]string{"context", "repair", "--workspace", workspace})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "no pending writes") {
		t.Fatalf("unexpected output: %s", out.String())
	}
}

func TestContextRepairCommandDrainsPendingWrites(t *testing.T) {
	workspace := t.TempDir()
	dbPath := filepath.Join(workspace, "context.sqlite")
	repo, err := contextstore.OpenSQLite(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	item := contextstore.ContextItem{ID: "pending-cli", Kind: contextstore.ContextProgress, Content: "queued while store was unavailable", Scope: contextstore.Scope{ProjectID: workspace}}
	pendingPath := filepath.Join(workspace, "context-pending.jsonl")
	if err := contextstore.AppendPendingWrite(pendingPath, item, os.ErrClosed); err != nil {
		t.Fatal(err)
	}
	if err := repo.Close(); err != nil {
		t.Fatal(err)
	}

	root := newRootCommand()
	out := new(bytes.Buffer)
	root.SetOut(out)
	root.SetArgs([]string{"context", "repair", "--workspace", workspace})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "1 recovered, 0 still pending") {
		t.Fatalf("unexpected output: %s", out.String())
	}

	repo, err = contextstore.OpenSQLite(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	got, err := repo.Get(t.Context(), "pending-cli")
	if err != nil {
		t.Fatalf("expected repaired item to be queryable: %v", err)
	}
	if got.Content != item.Content {
		t.Fatalf("unexpected repaired content: %q", got.Content)
	}
}

func TestContextInspectShowsTraceMetadataWithoutPromptContent(t *testing.T) {
	workspace := t.TempDir()
	trace := `{"id":"trace-1","kind":"worker","fingerprint":"abc","legacy_tokens":12,"canonical_tokens":10,"budget_tokens":100,"selected_items":2}` + "\n"
	if err := os.WriteFile(filepath.Join(workspace, "context-shadow-traces.jsonl"), []byte(trace), 0o600); err != nil {
		t.Fatal(err)
	}
	root := newRootCommand()
	out := new(bytes.Buffer)
	root.SetOut(out)
	root.SetArgs([]string{"context", "inspect", "--workspace", workspace})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if !strings.Contains(got, "trace-1") || !strings.Contains(got, "canonical tokens: 10") {
		t.Fatalf("unexpected inspect output: %q", got)
	}
	if strings.Contains(got, "prompt") {
		t.Fatalf("inspect must not render prompt content: %q", got)
	}
}

func TestContextQueryUsesHybridRetrieval(t *testing.T) {
	workspace := t.TempDir()
	repo, err := contextstore.OpenSQLite(filepath.Join(workspace, "context.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	if err = repo.Append(t.Context(), contextstore.ContextItem{ID: "path", Kind: contextstore.ContextPattern, Content: "edit internal/context/retrieval.go", Scope: contextstore.Scope{ProjectID: "p"}}); err != nil {
		t.Fatal(err)
	}
	repo.Close()
	root := newRootCommand()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetArgs([]string{"context", "query", "--workspace", workspace, "--project", "p", "internal/context/retrieval.go"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "path") {
		t.Fatalf("query output=%q", out.String())
	}
}

func TestContextRebuildRestoresFTSIndex(t *testing.T) {
	workspace := t.TempDir()
	repo, err := contextstore.OpenSQLite(filepath.Join(workspace, "context.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.Append(t.Context(), contextstore.ContextItem{ID: "fts", Kind: contextstore.ContextPattern, Content: "restore lexical index", Scope: contextstore.Scope{ProjectID: "p"}}); err != nil {
		t.Fatal(err)
	}
	repo.Close()
	root := newRootCommand()
	out := new(bytes.Buffer)
	root.SetOut(out)
	root.SetArgs([]string{"context", "rebuild", "--workspace", workspace})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "FTS5 index rebuilt") {
		t.Fatalf("output=%q", out.String())
	}
}

func TestContextQueryDefaultsToSharedMetadataOnly(t *testing.T) {
	workspace := t.TempDir()
	repo, err := contextstore.OpenSQLite(filepath.Join(workspace, "context.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	if err := repo.Append(t.Context(),
		contextstore.ContextItem{ID: "shared", Kind: contextstore.ContextDecision, Content: "shared query marker", Scope: contextstore.Scope{ProjectID: "p"}},
		contextstore.ContextItem{ID: "private-a", Kind: contextstore.ContextObservation, Content: "private query marker must stay hidden", Scope: contextstore.Scope{ProjectID: "p", AgentID: "agent-a"}, Metadata: map[string]string{"visibility": "private", "memory_tier": "session"}},
	); err != nil {
		t.Fatal(err)
	}

	root := newRootCommand()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetArgs([]string{"context", "query", "--workspace", workspace, "--project", "p", "--json", "query marker"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "shared query marker") || strings.Contains(out.String(), "private query marker") || strings.Contains(out.String(), "private-a") {
		t.Fatalf("default query leaked content or private subtree: %s", out.String())
	}
	var got contextReadOutput
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.SchemaVersion != contextOutputSchemaVersion || got.Stats.ResultCount != 1 || len(got.Results) != 1 || got.Results[0].ID != "shared" || got.Results[0].Content != "" {
		t.Fatalf("unexpected redacted output: %#v", got)
	}
}

func TestContextListFiltersWorkerMemoryAndUsesStableRedactedJSON(t *testing.T) {
	workspace := t.TempDir()
	repo, err := contextstore.OpenSQLite(filepath.Join(workspace, "context.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	scope := contextstore.Scope{ProjectID: "p", TeamID: "team", AgentID: "agent-a"}
	if err := repo.Append(t.Context(),
		contextstore.ContextItem{ID: "candidate-persistent", Kind: contextstore.ContextPattern, Content: "candidate private content", Scope: scope, Lifecycle: contextstore.LifecycleCandidate, Metadata: map[string]string{"visibility": "private", "memory_tier": "persistent"}},
		contextstore.ContextItem{ID: "confirmed-session", Kind: contextstore.ContextPattern, Content: "confirmed private content", Scope: contextstore.Scope{ProjectID: "p", TeamID: "team", SessionID: "s", AgentID: "agent-a"}, Lifecycle: contextstore.LifecycleConfirmed, Metadata: map[string]string{"visibility": "private", "memory_tier": "session"}},
		contextstore.ContextItem{ID: "other-worker", Kind: contextstore.ContextPattern, Content: "other worker content", Scope: contextstore.Scope{ProjectID: "p", TeamID: "team", AgentID: "agent-b"}, Metadata: map[string]string{"visibility": "private", "memory_tier": "persistent"}},
	); err != nil {
		t.Fatal(err)
	}

	root := newRootCommand()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetArgs([]string{"context", "list", "--workspace", workspace, "--project", "p", "--team", "team", "--agent", "agent-a", "--tier", "persistent", "--lifecycle", "candidate", "--json"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "candidate private content") || strings.Contains(out.String(), "confirmed-session") || strings.Contains(out.String(), "other-worker") {
		t.Fatalf("list leaked content or ignored filters: %s", out.String())
	}
	var got contextReadOutput
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.SchemaVersion != 1 || len(got.Results) != 1 || got.Results[0].ID != "candidate-persistent" || got.Results[0].Tier != "persistent" || got.Results[0].Lifecycle != contextstore.LifecycleCandidate || got.Results[0].Content != "" {
		t.Fatalf("unexpected filtered output: %#v", got)
	}
}

func TestContextQuerySessionTierUsesExplicitWorkerMaintenanceScope(t *testing.T) {
	workspace := t.TempDir()
	repo, err := contextstore.OpenSQLite(filepath.Join(workspace, "context.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	if err := repo.Append(t.Context(),
		contextstore.ContextItem{ID: "agent-a-session", Kind: contextstore.ContextPattern, Content: "session tier query marker", Scope: contextstore.Scope{ProjectID: "p", TeamID: "team", SessionID: "session-1", BranchID: "main", AgentID: "agent-a"}, Metadata: map[string]string{"visibility": "private", "memory_tier": "session"}},
		contextstore.ContextItem{ID: "agent-b-session", Kind: contextstore.ContextPattern, Content: "session tier query marker", Scope: contextstore.Scope{ProjectID: "p", TeamID: "team", SessionID: "session-1", BranchID: "main", AgentID: "agent-b"}, Metadata: map[string]string{"visibility": "private", "memory_tier": "session"}},
	); err != nil {
		t.Fatal(err)
	}

	root := newRootCommand()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetArgs([]string{"context", "query", "--workspace", workspace, "--project", "p", "--team", "team", "--agent", "agent-a", "--tier", "session", "--lifecycle", "confirmed", "--json", "session tier query marker"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	var got contextReadOutput
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Results) != 1 || got.Results[0].ID != "agent-a-session" || got.Results[0].Tier != "session" || got.Results[0].Content != "" {
		t.Fatalf("session-tier query must return only the explicitly selected worker's redacted record: %#v", got)
	}
}

func TestContextHelpDocumentsReadOnlyMemoryCommands(t *testing.T) {
	root := newRootCommand()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetArgs([]string{"context", "--help"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	for _, command := range []string{"list", "explain", "query"} {
		if !strings.Contains(out.String(), command) {
			t.Fatalf("context help missing %q: %s", command, out.String())
		}
	}
}
