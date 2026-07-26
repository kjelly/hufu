package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	contextstore "github.com/anomalyco/hufu/internal/context"
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
