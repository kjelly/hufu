package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	contextstore "github.com/kjelly/hufu/internal/context"
	"github.com/kjelly/hufu/internal/memory"
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

func TestContextRebuildVectorRequiresProjectScope(t *testing.T) {
	contextProject = ""
	contextTeam = ""
	contextRebuildVector = false
	workspace := t.TempDir()
	root := newRootCommand()
	root.SetArgs([]string{"context", "rebuild", "--workspace", workspace, "--vector"})
	if err := root.Execute(); err == nil || !strings.Contains(err.Error(), "--project is required") {
		t.Fatalf("vector rebuild error = %v, want project scope validation", err)
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

func TestContextListShowsSharedLifecycleAndProjectionEligibility(t *testing.T) {
	workspace := t.TempDir()
	repo, err := contextstore.OpenSQLite(filepath.Join(workspace, "context.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.Append(t.Context(), contextstore.ContextItem{ID: "shared-candidate", Kind: contextstore.ContextDecision, Content: "shared candidate content", Scope: contextstore.Scope{ProjectID: "p", TeamID: "team"}, Lifecycle: contextstore.LifecycleCandidate, Metadata: map[string]string{"visibility": "shared", "memory_lifetime": "persistent"}}); err != nil {
		t.Fatal(err)
	}
	if err := repo.Close(); err != nil {
		t.Fatal(err)
	}
	contextProject, contextTeam, contextAgent, contextTier, contextLifecycle, contextAllAgents = "", "", "", "", "", false
	root := newRootCommand()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetArgs([]string{"context", "list", "--workspace", workspace, "--project", "p", "--team", "team", "--lifecycle", "candidate", "--json"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	var got contextReadOutput
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Results) != 1 || got.Results[0].ID != "shared-candidate" || got.Results[0].Lifetime != "persistent" || got.Results[0].ProjectionEligible {
		t.Fatalf("shared lifecycle list = %#v", got)
	}
}

func TestContextLifecycleCommandsShowCandidatesAndHistory(t *testing.T) {
	workspace := t.TempDir()
	repo, err := contextstore.OpenSQLite(filepath.Join(workspace, "context.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	scope := contextstore.Scope{ProjectID: "p", TeamID: "team"}
	if err := repo.Append(t.Context(),
		contextstore.ContextItem{ID: "old", Kind: contextstore.ContextDecision, Content: "old secret-token=do-not-show", Scope: scope},
		contextstore.ContextItem{ID: "new", Kind: contextstore.ContextDecision, Content: "new decision", Scope: scope},
		contextstore.ContextItem{ID: "candidate", Kind: contextstore.ContextPattern, Content: "reviewable candidate", Scope: scope, Lifecycle: contextstore.LifecycleCandidate},
	); err != nil {
		t.Fatal(err)
	}
	if err := repo.MarkSuperseded(t.Context(), []string{"old"}, "new"); err != nil {
		t.Fatal(err)
	}
	if err := repo.Close(); err != nil {
		t.Fatal(err)
	}

	contextShowContent = false
	root := newRootCommand()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetArgs([]string{"context", "show", "--workspace", workspace, "--project", "p", "--team", "team", "old"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "secret-token") || !strings.Contains(out.String(), "old") {
		t.Fatalf("show output=%q", out.String())
	}

	root = newRootCommand()
	out.Reset()
	root.SetOut(&out)
	root.SetArgs([]string{"context", "candidates", "--workspace", workspace, "--project", "p", "--team", "team"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "candidate") || strings.Contains(out.String(), "old\t") {
		t.Fatalf("candidates output=%q", out.String())
	}

	root = newRootCommand()
	out.Reset()
	root.SetOut(&out)
	root.SetArgs([]string{"context", "history", "--workspace", workspace, "--project", "p", "--team", "team", "old"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "old") || !strings.Contains(out.String(), "new") {
		t.Fatalf("history output=%q", out.String())
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
	for _, command := range []string{"list", "show", "candidates", "history", "consolidate", "explain", "query", "confirm", "reject", "supersede", "migrate-memory"} {
		if !strings.Contains(out.String(), command) {
			t.Fatalf("context help missing %q: %s", command, out.String())
		}
	}
}

func TestLegacyMemoryItemPreservesLifecycleAndProvenance(t *testing.T) {
	item := legacyMemoryItem(memory.MemoryRecord{
		ID: "legacy-1", Content: "legacy architecture fact", Category: "architecture", Status: memory.StatusCandidate,
		SourceTaskID: "task-1", FilePaths: []string{"internal/app.go"}, Confidence: .7, Supersedes: []string{"legacy-0"},
	}, contextstore.Scope{ProjectID: "p", TeamID: "team"})
	if item.Kind != contextstore.ContextArchitecture || item.Lifecycle != contextstore.LifecycleCandidate || item.Metadata["legacy_memory_id"] != "legacy-1" {
		t.Fatalf("legacy conversion = %#v", item)
	}
	if len(item.Evidence) != 2 || item.Evidence[0].Ref != "task-1" || item.Evidence[1].Ref != "internal/app.go" {
		t.Fatalf("legacy evidence = %#v", item.Evidence)
	}
	if item.Metadata["supersedes_ids"] != legacyMemoryContextID("legacy-0") {
		t.Fatalf("legacy supersession metadata = %#v", item.Metadata)
	}
}

func TestContextConfirmAndRejectCandidateLifecycle(t *testing.T) {
	workspace := t.TempDir()
	repo, err := contextstore.OpenSQLite(filepath.Join(workspace, "context.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.Append(t.Context(),
		contextstore.ContextItem{ID: "confirm-me", Kind: contextstore.ContextPattern, Content: "approved candidate", Scope: contextstore.Scope{ProjectID: "p", TeamID: "team"}, Lifecycle: contextstore.LifecycleCandidate},
		contextstore.ContextItem{ID: "reject-me", Kind: contextstore.ContextPattern, Content: "rejected candidate", Scope: contextstore.Scope{ProjectID: "p", TeamID: "team"}, Lifecycle: contextstore.LifecycleCandidate},
	); err != nil {
		t.Fatal(err)
	}
	if err := repo.Close(); err != nil {
		t.Fatal(err)
	}

	root := newRootCommand()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetArgs([]string{"context", "confirm", "--workspace", workspace, "--project", "p", "--team", "team", "--evidence", "manifest-1", "confirm-me"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "1 item(s) confirmed") {
		t.Fatalf("confirm output = %q", out.String())
	}
	root = newRootCommand()
	out.Reset()
	root.SetOut(&out)
	root.SetArgs([]string{"context", "reject", "--workspace", workspace, "--project", "p", "--team", "team", "--reason", "operator review", "reject-me"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}

	repo, err = contextstore.OpenSQLite(filepath.Join(workspace, "context.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	confirmed, err := repo.Get(t.Context(), "confirm-me")
	if err != nil {
		t.Fatal(err)
	}
	rejected, err := repo.Get(t.Context(), "reject-me")
	if err != nil {
		t.Fatal(err)
	}
	if confirmed.Lifecycle != contextstore.LifecycleConfirmed || rejected.Lifecycle != contextstore.LifecycleRejected || rejected.Metadata["rejection_reason"] != "operator review" {
		t.Fatalf("unexpected lifecycle state confirmed=%#v rejected=%#v", confirmed, rejected)
	}
}
