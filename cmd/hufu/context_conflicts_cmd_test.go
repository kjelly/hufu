package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	contextstore "github.com/kjelly/hufu/internal/context"
	"github.com/kjelly/hufu/internal/memoryconflict"
)

type staticTextGenerator struct{ reply string }

func (g staticTextGenerator) GenerateText(context.Context, string) (string, error) {
	return g.reply, nil
}

func helperRunConflictsCLI(args ...string) (string, error) {
	conflictsWorkspace, conflictsProject, conflictsTeam, conflictsSearchPath, conflictsModel, conflictsDismissReason = "", "", "", "", "", ""
	conflictsJSON, conflictsDryRun, conflictsVector, conflictsRetryUndetermined, conflictsAll, conflictsShowContent = false, false, false, false, false, false
	conflictsTopK, conflictsMaxPairs, conflictsMaxItems, conflictsVectorTopK = 5, 20, 2000, 5
	conflictsMinVectorSimilarity = 0.45
	root := newRootCommand()
	out := new(bytes.Buffer)
	root.SetOut(out)
	root.SetErr(out)
	root.SetArgs(args)
	err := root.Execute()
	return out.String(), err
}

func seedConflictMemories(t *testing.T, workspace string) {
	t.Helper()
	repo, err := contextstore.OpenSQLite(filepath.Join(workspace, "context.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	for id, content := range map[string]string{
		"storage-sqlite":   "Use SQLite database storage for session state",
		"storage-postgres": "Use PostgreSQL database storage for session state",
	} {
		item := contextstore.ContextItem{ID: id, Kind: contextstore.ContextDecision, Content: content, Scope: contextstore.Scope{ProjectID: "proj1", TeamID: "demo"}, Lifecycle: contextstore.LifecycleConfirmed, Metadata: map[string]string{"memory_lifetime": "persistent"}}
		if err = repo.Append(context.Background(), item); err != nil {
			t.Fatal(err)
		}
	}
}

func stubConflictJudge(t *testing.T, reply string) *int {
	t.Helper()
	calls := 0
	original := conflictJudgeFactory
	conflictJudgeFactory = func(context.Context, string, string) (memoryconflict.TextGenerator, string, func(), error) {
		calls++
		return staticTextGenerator{reply: reply}, "judge-model", func() {}, nil
	}
	t.Cleanup(func() { conflictJudgeFactory = original })
	return &calls
}

func TestConflictsCLIScanListShowDismiss(t *testing.T) {
	workspace, search := t.TempDir(), t.TempDir()
	helperSetupTeam(t, search, "demo")
	seedConflictMemories(t, workspace)
	stubConflictJudge(t, `{"verdict":"contradicts","rationale":"Different database engines."}`)
	scope := []string{"--workspace", workspace, "--project", "proj1", "--team", "demo"}

	out, err := helperRunConflictsCLI(append([]string{"context", "conflicts", "scan", "--json", "--team-search-path", search}, scope...)...)
	if err != nil {
		t.Fatalf("scan: %v\n%s", err, out)
	}
	var scan struct {
		Scan memoryconflict.ScanReport `json:"scan"`
	}
	if err = json.Unmarshal([]byte(out), &scan); err != nil || scan.Scan.Contradictions != 1 {
		t.Fatalf("scan output %s err=%v", out, err)
	}

	out, err = helperRunConflictsCLI(append([]string{"context", "conflicts", "list"}, scope...)...)
	if err != nil || strings.Count(strings.TrimSpace(out), "\n") != 0 || !strings.Contains(out, "\topen\tstorage-postgres\tstorage-sqlite") {
		t.Fatalf("list = %q err=%v", out, err)
	}
	id := strings.Fields(out)[0]

	out, err = helperRunConflictsCLI(append([]string{"context", "conflicts", "show", id}, scope...)...)
	if err != nil || strings.Contains(out, "PostgreSQL") || !strings.Contains(out, "resolve: hufu context supersede") {
		t.Fatalf("default show must be content-free with resolution hints: %q err=%v", out, err)
	}
	out, err = helperRunConflictsCLI(append([]string{"context", "conflicts", "show", id, "--show-content"}, scope...)...)
	if err != nil || !strings.Contains(out, "PostgreSQL") || !strings.Contains(out, "Different database engines.") {
		t.Fatalf("show --show-content = %q err=%v", out, err)
	}

	if _, err = helperRunConflictsCLI(append([]string{"context", "conflicts", "dismiss", id}, scope...)...); err == nil {
		t.Fatal("dismiss without --reason succeeded")
	}
	out, err = helperRunConflictsCLI(append([]string{"context", "conflicts", "dismiss", id, "--reason", "Separate services."}, scope...)...)
	if err != nil || !strings.Contains(out, "\tdismissed") {
		t.Fatalf("dismiss = %q err=%v", out, err)
	}
	out, err = helperRunConflictsCLI(append([]string{"context", "conflicts", "dismiss", id, "--reason", "Again."}, scope...)...)
	if err != nil || !strings.Contains(out, "already dismissed") {
		t.Fatalf("second dismiss = %q err=%v", out, err)
	}
	out, err = helperRunConflictsCLI(append([]string{"context", "conflicts", "list"}, scope...)...)
	if err != nil || strings.TrimSpace(out) != "No open memory conflicts." {
		t.Fatalf("list after dismiss = %q err=%v", out, err)
	}
	out, err = helperRunConflictsCLI(append([]string{"context", "conflicts", "list", "--all"}, scope...)...)
	if err != nil || !strings.Contains(out, "\tdismissed\t") {
		t.Fatalf("list --all = %q err=%v", out, err)
	}
	events, err := os.ReadFile(filepath.Join(workspace, "logs", "event_store.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"memory_conflict_detected", "memory_conflict_dismissed"} {
		if !strings.Contains(string(events), want) {
			t.Fatalf("event store missing %s", want)
		}
	}
	if strings.Contains(string(events), "PostgreSQL") || strings.Contains(string(events), "Separate services.") {
		t.Fatal("event store leaked memory content or dismiss reason")
	}
}

func TestConflictsScanVectorUnavailableFallsBack(t *testing.T) {
	workspace, search := t.TempDir(), t.TempDir()
	helperSetupTeam(t, search, "demo")
	seedConflictMemories(t, workspace)
	stubConflictJudge(t, `{"verdict":"compatible","rationale":""}`)
	original := conflictVectorFactory
	conflictVectorFactory = func(context.Context, string, contextstore.Repository, contextstore.Scope) (memoryconflict.SimilarSearcher, error) {
		return nil, errors.New("ollama unavailable")
	}
	t.Cleanup(func() { conflictVectorFactory = original })

	out, err := helperRunConflictsCLI("context", "conflicts", "scan", "--vector", "--team-search-path", search, "--workspace", workspace, "--project", "proj1", "--team", "demo")
	if err != nil {
		t.Fatalf("scan with unavailable vector must still succeed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "vector=unavailable") || !strings.Contains(out, "diagnostic\t-\tvector_unavailable") || !strings.Contains(out, "judged=1") {
		t.Fatalf("scan output = %q", out)
	}
}

func TestConflictsScanDryRunNeedsNoJudge(t *testing.T) {
	workspace := t.TempDir()
	seedConflictMemories(t, workspace)
	calls := stubConflictJudge(t, "unused")
	out, err := helperRunConflictsCLI("context", "conflicts", "scan", "--dry-run", "--workspace", workspace, "--project", "proj1", "--team", "demo")
	if err != nil || *calls != 0 || !strings.Contains(out, "judged=1") || !strings.Contains(out, "dry_run") {
		t.Fatalf("dry run = %q err=%v judge factory calls=%d", out, err, *calls)
	}
}

func TestConflictsListOnUnmigratedReadOnlyStore(t *testing.T) {
	workspace := t.TempDir()
	seedConflictMemories(t, workspace)
	db, err := sql.Open("sqlite", filepath.Join(workspace, "context.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`DROP TABLE context_pair_judgments; DELETE FROM schema_migrations WHERE version >= 11`); err != nil {
		t.Fatal(err)
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	out, err := helperRunConflictsCLI("context", "conflicts", "list", "--workspace", workspace, "--project", "proj1", "--team", "demo")
	if err != nil || strings.TrimSpace(out) != conflictsUnavailableMessage {
		t.Fatalf("list on schema 10 = %q err=%v", out, err)
	}
}
