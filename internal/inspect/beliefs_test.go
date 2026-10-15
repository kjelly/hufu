package inspect

import (
	"context"
	"encoding/json/v2"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/kjelly/hufu/internal/belief"
	"github.com/kjelly/hufu/internal/team"
)

type beliefTestJournal struct{ store *team.EventStore }

func (j beliefTestJournal) Append(ctx context.Context, event team.RunEvent) (team.RunEvent, error) {
	return j.store.AppendPersistedContext(ctx, event)
}
func (j beliefTestJournal) ReadEvents(context.Context) ([]team.RunEvent, error) {
	return j.store.ReadEvents()
}
func (j beliefTestJournal) VerifyHashChain(context.Context) error { return j.store.VerifyHashChain() }

func beliefInspectFixture(t *testing.T) (workspace, digest, artifactID string) {
	t.Helper()
	workspace = t.TempDir()
	store, err := team.NewEventStore(workspace, "run-belief", "session-belief")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	created, err := store.AppendPersisted(team.RunEvent{Type: "task_created", Actor: "coordinator", TaskID: "task-1", Payload: []byte(`{"id":"task-1","agent":"worker","status":"pending"}`)})
	if err != nil {
		t.Fatal(err)
	}
	tree := team.NewSessionTree()
	tree.Branches["early-fork"] = &team.SessionBranch{ID: "early-fork", Name: "early-fork", ParentID: "main", ForkEventID: created.ID}
	if err := team.SaveSessionTree(workspace, tree); err != nil {
		t.Fatal(err)
	}
	artifacts, err := team.NewFileArtifactStore(workspace, workspace)
	if err != nil {
		t.Fatal(err)
	}
	journal, err := team.NewBeliefJournal(beliefTestJournal{store}, artifacts, tree, "main", "run-belief", "session-belief")
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile("../belief/testdata/synthetic-model.json")
	if err != nil {
		t.Fatal(err)
	}
	model, err := belief.DecodeModel(data)
	if err != nil {
		t.Fatal(err)
	}
	if err := journal.BindVerificationModel(t.Context(), model, "comparison"); err != nil {
		t.Fatal(err)
	}
	receipt := team.ExecutionReceipt{RunID: "run-belief", TaskID: "task-1", Attempt: 1, ModelExecutionID: "execution-1", RunInputSnapshotHash: "input-revision-1", VerifyResult: &team.VerificationResult{ExitCode: 0, Fingerprint: "verify-1", Command: "never-execute-private-command", Stdout: "private-output", WorkDir: "/private/workdir"}}
	payload, err := json.Marshal(map[string]any{"id": "task-1", "status": "done", "attempt": 1, "execution_receipt": receipt})
	if err != nil {
		t.Fatal(err)
	}
	source, err := store.AppendPersisted(team.RunEvent{Type: "task_completed", Actor: "worker", TaskID: "task-1", Payload: payload})
	if err != nil {
		t.Fatal(err)
	}
	epoch, err := team.BeliefVerificationEpoch(model, belief.Scope{RunID: "run-belief", BranchID: "main", TaskID: "task-1", SessionID: "session-belief", Attempt: 1}, source, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := journal.OpenEpoch(t.Context(), epoch); err != nil {
		t.Fatal(err)
	}
	if err := journal.AdmitVerification(t.Context(), epoch.ID, source.ID, "comparison", ""); err != nil {
		t.Fatal(err)
	}
	states, err := journal.Replay(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	digest = states[0].State.Digest
	events, err := store.ReadEvents()
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event.Type == team.EventBeliefModelBound {
			var binding struct {
				Ref team.ArtifactRef `json:"model_ref"`
			}
			if err := json.Unmarshal(event.Payload, &binding); err != nil {
				t.Fatal(err)
			}
			artifactID = binding.Ref.ID
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	return workspace, digest, artifactID
}

func TestBeliefInspectReconstructsWithoutWritingOrExposingRawEvidence(t *testing.T) {
	workspace, digest, _ := beliefInspectFixture(t)
	before := snapshotInspectWorkspace(t, workspace)
	result, err := InspectBeliefs(t.Context(), InspectQuery{Workspace: workspace, RunID: "run-belief", TaskID: "task-1", SessionID: "session-belief"})
	if err != nil {
		t.Fatal(err)
	}
	data := result.Data.(BeliefsData)
	if len(data.Epochs) != 1 || data.Epochs[0].State.Digest != digest || len(data.Execution) != 1 || data.Execution[0].Status != "done" || data.Models[0].Synthetic == nil || !*data.Models[0].Synthetic || len(data.Epochs[0].Observations) != 1 || data.Epochs[0].Observations[0].OutcomeID != "pass" {
		t.Fatalf("inspection=%+v", data)
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"never-execute-private-command", "private-output", "/private/workdir"} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("inspection exposed %q", forbidden)
		}
	}
	if !reflect.DeepEqual(before, snapshotInspectWorkspace(t, workspace)) {
		t.Fatal("belief inspection changed workspace bytes, modes or timestamps")
	}
	fork, err := InspectBeliefs(t.Context(), InspectQuery{Workspace: workspace, RunID: "run-belief", BranchID: "early-fork"})
	if err != nil || len(fork.Data.(BeliefsData).Epochs) != 0 {
		t.Fatalf("post-fork observations entered child: result=%+v error=%v", fork, err)
	}
	for _, query := range []InspectQuery{
		{Workspace: workspace, RunID: "run-belief", SessionID: "other-session"},
		{Workspace: workspace, RunID: "run-belief", TaskID: "other-task"},
	} {
		if _, err := InspectBeliefs(t.Context(), query); !errors.Is(err, ErrNotFound) {
			t.Fatalf("invalid scope error=%v", err)
		}
	}
}

func TestBeliefInspectMissingModelIsUnavailableWithSourceRefs(t *testing.T) {
	workspace, _, artifactID := beliefInspectFixture(t)
	if err := os.Remove(filepath.Join(workspace, "logs", "artifacts", "data", artifactID)); err != nil {
		t.Fatal(err)
	}
	result, err := InspectBeliefs(t.Context(), InspectQuery{Workspace: workspace, RunID: "run-belief"})
	if err != nil {
		t.Fatal(err)
	}
	data := result.Data.(BeliefsData)
	if result.Integrity.Projection != "unavailable" || data.Models[0].Synthetic != nil || data.Epochs[0].State != nil || len(data.Epochs[0].Sources) != 1 {
		t.Fatalf("missing model=%+v", data)
	}
}
