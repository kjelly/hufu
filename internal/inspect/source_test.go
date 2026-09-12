package inspect

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kjelly/hufu/internal/team"
)

func TestLoadLineageMissingEventStoreDoesNotCreateWorkspaceFiles(t *testing.T) {
	workspace := filepath.Join(t.TempDir(), "missing-workspace")
	lineage, err := LoadLineage(t.Context(), InspectQuery{Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	if lineage.BranchID != "main" || lineage.ActiveBranchID != "main" || len(lineage.Events) != 0 || len(lineage.GlobalEvents) != 0 {
		t.Fatalf("missing lineage = %#v", lineage)
	}
	if _, err := os.Stat(workspace); !os.IsNotExist(err) {
		t.Fatalf("lineage load created workspace: %v", err)
	}
}

func TestLoadLineageUsesOnlySelectedBranchAndPreservesGlobalOrdinals(t *testing.T) {
	workspace := t.TempDir()
	store, err := team.NewEventStore(workspace, "run-main", "session-1")
	if err != nil {
		t.Fatal(err)
	}
	mainEvent, err := store.AppendPersisted(team.RunEvent{ID: "event-main", Type: "run_started", Actor: "coordinator", Payload: []byte(`{"goal":"main"}`)})
	if err != nil {
		t.Fatal(err)
	}
	store.SetBranchID("feature")
	if _, err := store.AppendPersisted(team.RunEvent{ID: "event-feature", RunID: "run-feature", Type: "run_started", Actor: "coordinator", Payload: []byte(`{"goal":"feature"}`)}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	tree := team.NewSessionTree()
	tree.Branches["feature"] = &team.SessionBranch{
		ID:          "feature",
		Name:        "feature",
		ParentID:    "main",
		ForkEventID: mainEvent.ID,
		CreatedAt:   time.Now().UTC().Format(time.RFC3339),
	}
	tree.ActiveBranch = "main"
	if err := team.SaveSessionTree(workspace, tree); err != nil {
		t.Fatal(err)
	}

	eventPath := filepath.Join(workspace, "logs", "event_store.jsonl")
	treePath := filepath.Join(workspace, "session_tree.json")
	eventsBefore, err := os.ReadFile(eventPath)
	if err != nil {
		t.Fatal(err)
	}
	treeBefore, err := os.ReadFile(treePath)
	if err != nil {
		t.Fatal(err)
	}

	active, err := LoadLineage(t.Context(), InspectQuery{Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	if active.BranchID != "main" || len(active.Events) != 1 || active.Events[0].Event.ID != "event-main" {
		t.Fatalf("active lineage = %#v", active)
	}
	if active.ActiveBranchID != "main" || len(active.GlobalEvents) != 2 || active.GlobalEvents[1].Ordinal != 2 {
		t.Fatalf("active global lineage metadata = %#v", active)
	}
	feature, err := LoadLineage(t.Context(), InspectQuery{Workspace: workspace, BranchID: "feature"})
	if err != nil {
		t.Fatal(err)
	}
	if len(feature.Events) != 2 || feature.Events[0].Ordinal != 1 || feature.Events[1].Ordinal != 2 {
		t.Fatalf("feature lineage = %#v", feature)
	}

	eventsAfter, err := os.ReadFile(eventPath)
	if err != nil {
		t.Fatal(err)
	}
	treeAfter, err := os.ReadFile(treePath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(eventsBefore, eventsAfter) || !bytes.Equal(treeBefore, treeAfter) {
		t.Fatal("lineage loading changed persisted state")
	}
}

func TestLoadLineageRejectsAmbiguousBranchName(t *testing.T) {
	tree := team.NewSessionTree()
	tree.Branches["left"] = &team.SessionBranch{ID: "left", Name: "duplicate"}
	tree.Branches["right"] = &team.SessionBranch{ID: "right", Name: "duplicate"}
	if _, err := resolveBranch(tree, "duplicate"); err == nil {
		t.Fatal("ambiguous branch name was accepted")
	}
}
