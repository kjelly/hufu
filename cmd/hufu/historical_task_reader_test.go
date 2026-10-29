package main

import (
	"bytes"
	"encoding/json/v2"
	"os"
	"path/filepath"
	"strings"
	"testing"

	inspectpkg "github.com/kjelly/hufu/internal/inspect"
	"github.com/kjelly/hufu/internal/team"
)

func TestCanonicalHistoricalTaskReaderHonorsForkScope(t *testing.T) {
	workspace, ids := resultsTestFixture(t)
	before, err := inspectpkg.LoadHistoricalResults(t.Context(), inspectpkg.HistoricalResultScope{Workspace: workspace, Team: "review", Branch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	var anchor inspectpkg.HistoricalResult
	for _, row := range before.Results {
		if row.ResultID == ids[0] {
			anchor = row
		}
	}
	if anchor.SourceEventID == "" {
		t.Fatal("fixture has no canonical fork anchor")
	}
	store, err := team.OpenEventStore(workspace)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	tree, err := team.LoadSessionTree(workspace)
	if err != nil {
		t.Fatal(err)
	}
	child, err := team.ForkSessionBranch(t.Context(), workspace, tree, store, "child", anchor.SourceEventID)
	if err != nil {
		t.Fatal(err)
	}
	appendTask := func(branch, run, sentinel string) {
		t.Helper()
		for _, event := range []team.RunEvent{
			{BranchID: branch, RunID: run, Type: "run_started", Actor: "user", Payload: resultsTestJSON(t, map[string]any{"team": "review"})},
			{BranchID: branch, RunID: run, Type: "task_completed", Actor: "helper", TaskID: "9", Payload: resultsTestJSON(t, map[string]any{"id": "9", "agent": "helper", "status": "done", "desc": sentinel, "output": sentinel})},
		} {
			if err := store.Append(event); err != nil {
				t.Fatal(err)
			}
		}
	}
	appendTask("main", "run-later-parent", "POST_FORK_PARENT_SENTINEL")
	sibling, err := team.ForkSessionBranch(t.Context(), workspace, tree, store, "sibling", "main")
	if err != nil {
		t.Fatal(err)
	}
	appendTask(sibling.ID, "run-sibling", "SIBLING_SENTINEL")
	dir := filepath.Join(workspace, "tasks", "review", "helper")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "latest.md"), []byte("UNVERIFIED_MARKDOWN_SENTINEL"), 0o600); err != nil {
		t.Fatal(err)
	}
	rows, err := (canonicalHistoricalTaskReader{}).Load(t.Context(), team.HistoricalTaskScope{Workspace: workspace, Team: "review", Branch: child.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].ResultID != ids[0] || rows[0].SourceHash != anchor.SourceHash || rows[0].SourceEventID != anchor.SourceEventID || rows[0].SourceBranchID != "main" || rows[0].Team != "review" {
		t.Fatalf("reader did not preserve exact child-visible source: %+v", rows)
	}
	if rows[0].PayloadSHA256 == "" || rows[0].PayloadSHA256 != anchor.PayloadSHA256 || !bytes.Equal(rows[0].TypedResultJSON, anchor.TypedResultJSON) || rows[0].Workspace != workspace {
		t.Fatal("adapter lost canonical payload digest, full projection or workspace identity")
	}
	encoded, err := json.Marshal(rows)
	if err != nil {
		t.Fatal(err)
	}
	for _, sentinel := range []string{"POST_FORK_PARENT_SENTINEL", "SIBLING_SENTINEL", "UNVERIFIED_MARKDOWN_SENTINEL", "OTHER_TEAM_SENTINEL"} {
		if strings.Contains(string(encoded), sentinel) {
			t.Fatalf("reader disclosed %s", sentinel)
		}
	}
	if _, err := (canonicalHistoricalTaskReader{}).Load(t.Context(), team.HistoricalTaskScope{Workspace: workspace, Team: "review"}); err == nil {
		t.Fatal("missing branch silently selected a broader scope")
	}
}
