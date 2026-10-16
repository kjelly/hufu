package inspect

import (
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

func diagnosticInspectFixture(t *testing.T) (string, team.DiagnosticEpisodeState) {
	t.Helper()
	workspace := t.TempDir()
	store, err := team.NewEventStore(workspace, "run-offline", "session-offline")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	artifacts, err := team.NewFileArtifactStore(workspace, workspace)
	if err != nil {
		t.Fatal(err)
	}
	tree := team.NewSessionTree()
	if err := team.SaveSessionTree(workspace, tree); err != nil {
		t.Fatal(err)
	}
	j, err := team.NewDiagnosticJournal(beliefTestJournal{store}, artifacts, tree, "main", "run-offline", "session-offline")
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
	digest, err := belief.ModelDigest(model)
	if err != nil {
		t.Fatal(err)
	}
	f := team.DiagnosticFixture{SchemaVersion: 1, Version: team.DiagnosticFixtureVersion, Model: model,
		Epoch:     belief.Epoch{ID: "offline-episode", ModelDigest: digest, Scope: belief.Scope{RunID: "run-offline", SessionID: "session-offline", BranchID: "main", TaskID: "offline-owner", Attempt: 1}, SubjectRevision: strings.Repeat("a", 64), EnvironmentRevision: strings.Repeat("b", 64)},
		Revisions: belief.DiagnosticRevisions{Subject: strings.Repeat("a", 64), Source: strings.Repeat("c", 64), Environment: strings.Repeat("b", 64)},
		Policy: belief.DiagnosticPolicy{SchemaVersion: 1, Version: "1", Algorithm: belief.DiagnosticGiniAlgorithm, Threshold: 900_000_000,
			Limits:     belief.DiagnosticLimits{MaxRounds: 1, MaxAttempts: 1, MaxCostUnits: 1, ActionTimeoutSeconds: 5, EpisodeTimeoutSeconds: 20, MaxEvents: 8, MaxObservationBytes: 16384, MaxFenceRetries: 3},
			Candidates: []belief.DiagnosticCandidate{{ID: "check", ExperimentID: "comparison", CostUnits: 1}}},
		Measurements: []team.DiagnosticFixtureMeasurement{{ExperimentID: "comparison", OutcomeID: "pass", Digest: strings.Repeat("d", 64)}}}
	if err := j.OpenFixture(t.Context(), f); err != nil {
		t.Fatal(err)
	}
	for range 4 {
		views, err := j.Replay(t.Context())
		if err != nil || len(views) != 1 {
			t.Fatalf("replay=%+v error=%v", views, err)
		}
		view := views[0]
		if view.Status == "stopped" {
			return workspace, view
		}
		if err := j.AdvanceFixture(t.Context(), f.Epoch.ID, view.CursorID); err != nil {
			t.Fatal(err)
		}
	}
	t.Fatal("fixture did not stop")
	return "", team.DiagnosticEpisodeState{}
}

func TestDiagnosticInspectUsesFrozenInputsAndNeverWrites(t *testing.T) {
	workspace, expected := diagnosticInspectFixture(t)
	before := snapshotInspectWorkspace(t, workspace)
	for _, task := range []string{"", "offline-owner"} {
		result, err := InspectBeliefs(t.Context(), InspectQuery{Workspace: workspace, RunID: "run-offline", SessionID: "session-offline", TaskID: task})
		if err != nil {
			t.Fatal(err)
		}
		data := result.Data.(BeliefsData)
		if len(data.Episodes) != 1 || !reflect.DeepEqual(data.Episodes[0], expected) || len(data.Execution) != 0 || len(data.Epochs) != 0 || result.Integrity.Projection != "consistent" {
			t.Fatalf("projection=%+v", result)
		}
		encoded, err := json.Marshal(result)
		if err != nil {
			t.Fatal(err)
		}
		for _, raw := range []string{"likelihood_units", "prior_source", "independence_basis"} {
			if strings.Contains(string(encoded), raw) {
				t.Fatalf("inspect exposed raw fixture field %q", raw)
			}
		}
	}
	if !reflect.DeepEqual(before, snapshotInspectWorkspace(t, workspace)) {
		t.Fatal("inspect changed workspace bytes, permissions or timestamps")
	}
	if _, err := InspectBeliefs(t.Context(), InspectQuery{Workspace: workspace, RunID: "run-offline", TaskID: "unknown"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown owner=%v", err)
	}
}

func TestDiagnosticInspectMissingArtifactIsUnavailableAndReadOnly(t *testing.T) {
	workspace, expected := diagnosticInspectFixture(t)
	if err := os.Remove(filepath.Join(workspace, "logs", "artifacts", "data", expected.FixtureRef.ID)); err != nil {
		t.Fatal(err)
	}
	before := snapshotInspectWorkspace(t, workspace)
	result, err := InspectBeliefs(t.Context(), InspectQuery{Workspace: workspace, RunID: "run-offline"})
	if err != nil {
		t.Fatal(err)
	}
	view := result.Data.(BeliefsData).Episodes[0]
	if result.Integrity.Projection != "unavailable" || view.State != nil || view.Validation != "unavailable" || !reflect.DeepEqual(view.Decisions, expected.Decisions) || view.FixtureRef != expected.FixtureRef {
		t.Fatalf("unavailable=%+v", result)
	}
	if !reflect.DeepEqual(before, snapshotInspectWorkspace(t, workspace)) {
		t.Fatal("inspect repaired missing artifact")
	}
}
