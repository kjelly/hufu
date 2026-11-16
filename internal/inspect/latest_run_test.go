package inspect

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kjelly/hufu/internal/runtimestore"
	"github.com/kjelly/hufu/internal/team"
)

func TestInspectLatestRun(t *testing.T) {
	for _, branch := range []string{"main", "feature"} {
		t.Run(branch, func(t *testing.T) {
			workspace := t.TempDir()
			store, err := team.NewEventStore(workspace, "older", "session-test")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = store.Close() })
			old, err := store.AppendPersisted(team.RunEvent{Type: "run_started", Actor: "coordinator", Payload: []byte(`{"goal":"older"}`)})
			if err != nil {
				t.Fatal(err)
			}
			store.SetBranchID(branch)
			store.SetInvocationID("inv-newest")
			for _, event := range []team.RunEvent{
				{RunID: "newest", Type: "run_started", Actor: "coordinator", Timestamp: "2026-11-16T01:02:03.123456789Z", Payload: []byte(`{"goal":"latest"}`)},
				{RunID: "newest", Type: "task_created", Actor: "coordinator", TaskID: "pending", Payload: []byte(`{"id":"pending","agent":"worker","status":"pending"}`)},
				// New activity on an older run must not select it as the latest start.
				{RunID: "older", Type: "note", Actor: "coordinator", Payload: []byte(`{}`)},
			} {
				if _, err := store.AppendPersisted(event); err != nil {
					t.Fatal(err)
				}
			}
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			tree := team.NewSessionTree()
			if branch != "main" {
				tree.Branches[branch] = &team.SessionBranch{ID: branch, Name: branch, ParentID: "main", ForkEventID: old.ID, CreatedAt: time.Now().UTC().Format(time.RFC3339)}
			}
			if err := team.SaveSessionTree(workspace, tree); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(runtimestore.Path(workspace))
			if err != nil {
				t.Fatal(err)
			}
			latest, err := InspectLatestRun(t.Context(), workspace)
			if err != nil {
				t.Fatal(err)
			}
			if latest == nil || latest.Run.RunID != "newest" || latest.InvocationID != "inv-newest" || latest.BranchID != branch || latest.TaskStates["pending"] != 1 || latest.Run.TaskSummary.Total != 1 || latest.Run.TaskSummary.Unresolved != 1 || latest.Run.Outcome != "" {
				t.Fatalf("latest = %+v", latest)
			}
			if latest.StartedAt.Format(time.RFC3339Nano) != "2026-11-16T01:02:03.123456789Z" {
				t.Fatal(latest.StartedAt)
			}
			after, err := os.ReadFile(runtimestore.Path(workspace))
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(before, after) {
				t.Fatal("read changed runtime state")
			}
		})
	}
}

func TestInspectLatestRunMissingAndCorrupt(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing")
	latest, err := InspectLatestRun(t.Context(), missing)
	if err != nil || latest != nil {
		t.Fatalf("missing = %v, %v", latest, err)
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Fatalf("created missing workspace: %v", err)
	}
	workspace := t.TempDir()
	if err := os.WriteFile(runtimestore.Path(workspace), []byte("broken database"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := InspectLatestRun(t.Context(), workspace); !errors.Is(err, ErrIntegrity) {
		t.Fatalf("corrupt error = %v", err)
	}
}

func TestInspectLatestRunPreservesTerminalOutcome(t *testing.T) {
	fixture := buildRunFixture(t)
	latest, err := InspectLatestRun(t.Context(), fixture.workspace)
	if err != nil {
		t.Fatal(err)
	}
	envelope, err := InspectRun(t.Context(), InspectQuery{Workspace: fixture.workspace, RunID: fixture.runID})
	if err != nil {
		t.Fatal(err)
	}
	expected := envelope.Data.(RunData)
	if latest == nil || latest.Run.Outcome != expected.Outcome || latest.Run.Acceptance != expected.Acceptance || latest.Run.Completion != expected.Completion || latest.Run.TaskSummary != expected.TaskSummary || latest.TaskStates["done"] != expected.TaskSummary.Done {
		t.Fatalf("latest = %+v, expected = %+v", latest, expected)
	}
}
