package main

import (
	"bytes"
	json "encoding/json/v2"
	"path/filepath"
	"testing"

	contextstore "github.com/kjelly/hufu/internal/context"
	"github.com/kjelly/hufu/internal/team"
)

func TestContextExplainMemoryHistoricalQueryRequiresNoPlaintextOrCurrentPolicy(t *testing.T) {
	// Command flags are package-level singletons; isolate the new option from
	// earlier CLI tests and restore their original values after this case.
	for _, name := range []string{"query", "retrieval-id", "policy-version"} {
		flag := contextExplainMemoryCmd.Flags().Lookup(name)
		value, changed := flag.Value.String(), flag.Changed
		t.Cleanup(func() { _ = flag.Value.Set(value); flag.Changed = changed })
		if err := flag.Value.Set(flag.DefValue); err != nil {
			t.Fatal(err)
		}
		flag.Changed = false
	}
	workspace := t.TempDir()
	scope := contextstore.Scope{ProjectID: "project-cli", TeamID: "team-cli"}
	repo, err := contextstore.OpenSQLite(filepath.Join(workspace, "context.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.Append(t.Context(), contextstore.ContextItem{ID: "historical", Content: "secret historical content", Kind: contextstore.ContextPattern, Scope: scope, Lifecycle: contextstore.LifecycleConfirmed}); err != nil {
		t.Fatal(err)
	}
	if err := repo.Close(); err != nil {
		t.Fatal(err)
	}
	plan := team.BuildContextQueryPlan(team.ContextRequest{Goal: "secret prior query"}, team.ContextQueryHints{})
	selection, err := team.SelectContextQuerySources(t.Context(), nil, plan, []team.ContextQuerySource{{Kind: team.QuerySharedPersistent, Request: contextstore.SearchRequest{Scope: scope, Limit: 20}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	manifest := team.MemoryInjectionManifest{RetrievalID: "retrieval-historical", ModelExecutionID: "model-historical", PolicyVersion: "previous-policy", QueryHash: selection.CompositeHash, Query: &selection.Attribution, Items: []team.MemoryInjectionItem{{ContextItemID: "historical", Source: "shared_persistent", Rank: 1, FinalScore: .42}}}
	session := team.NewSession()
	session.Tasks = []*team.TodoItem{{ID: "task", MemoryManifests: []team.MemoryInjectionManifest{manifest}}}
	if err := team.SaveSession(workspace, session); err != nil {
		t.Fatal(err)
	}
	root := newRootCommand()
	var output bytes.Buffer
	root.SetOut(&output)
	root.SetErr(&output)
	root.SetArgs([]string{"context", "explain-memory", "historical", "--workspace", workspace, "--project", "project-cli", "--team", "team-cli", "--retrieval-id", manifest.RetrievalID, "--json"})
	if err := root.ExecuteContext(t.Context()); err != nil {
		t.Fatal(err)
	}
	var explanation team.MemoryExplanation
	if err := json.Unmarshal(output.Bytes(), &explanation); err != nil {
		t.Fatal(err)
	}
	if explanation.Recomputed || explanation.QueryStatus != "query_plan_required" || explanation.QueryPolicy == nil || explanation.RetrievalID != manifest.RetrievalID || explanation.FinalScore != .42 {
		t.Fatalf("CLI historical query: %+v", explanation)
	}
	if bytes.Contains(output.Bytes(), []byte("secret")) {
		t.Fatal("historical CLI exposed content or query plaintext")
	}
	if !bytes.Contains(output.Bytes(), []byte(`"exposure_count":null`)) || !bytes.Contains(output.Bytes(), []byte(`"counter_status":"unavailable"`)) {
		t.Fatal("historical counters rendered as measured zeros")
	}
}
