package inspect

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	operatorpkg "github.com/kjelly/hufu/internal/operator"
	"github.com/kjelly/hufu/internal/team"
)

func TestBindReadTargetSelectionMatrix(t *testing.T) {
	lineage := Lineage{
		BranchID: "main", ActiveBranchID: "main",
		Events: []IndexedEvent{
			{Ordinal: 1, Event: team.RunEvent{RunID: "run-1", SessionID: "session-1"}},
			{Ordinal: 2, Event: team.RunEvent{RunID: "run-2", SessionID: "session-2"}},
		},
	}
	request := operatorpkg.BindingRequest{RunID: "run-1"}
	if runID, source, err := selectBindingRun(lineage, request, nil); err != nil || runID != "run-1" || source != "explicit" {
		t.Fatalf("explicit selection = (%q, %q, %v)", runID, source, err)
	}
	session := &team.SessionData{
		ActiveRunInputSnapshotID: "input-2",
		RunInputSnapshots:        []team.RunInputSnapshot{{ID: "input-2", RunID: "run-2"}},
		RunResult:                &team.RunResult{RunID: "run-1"},
	}
	if runID, source, err := selectBindingRun(lineage, operatorpkg.BindingRequest{}, session); err != nil || runID != "run-2" || source != "active_binding" {
		t.Fatalf("active selection = (%q, %q, %v)", runID, source, err)
	}
	if _, _, err := selectBindingRun(lineage, operatorpkg.BindingRequest{}, nil); !errors.Is(err, ErrAmbiguous) {
		t.Fatalf("ambiguous selection error = %v", err)
	}
	lineage.Events = lineage.Events[:1]
	if runID, source, err := selectBindingRun(lineage, operatorpkg.BindingRequest{}, nil); err != nil || runID != "run-1" || source != "single_candidate" {
		t.Fatalf("single selection = (%q, %q, %v)", runID, source, err)
	}
}

func TestBindReadTargetRejectsSessionAndPersistedScopeCollisions(t *testing.T) {
	lineage := Lineage{Events: []IndexedEvent{
		{Event: team.RunEvent{RunID: "run-1", SessionID: "session-a", Payload: []byte(`{"scope":{"project_id":"project-a","team_id":"team-a"}}`)}},
		{Event: team.RunEvent{RunID: "run-1", SessionID: "session-b", Payload: []byte(`{"project_id":"project-b","team_id":"team-a"}`)}},
	}}
	if _, err := selectBindingSession(lineage, "run-1", ""); !errors.Is(err, ErrAmbiguous) {
		t.Fatalf("session collision error = %v", err)
	}
	if _, _, err := persistedScopeIDs(lineage, "run-1"); !errors.Is(err, ErrScopeConflict) {
		t.Fatalf("project collision error = %v", err)
	}
}

func TestBindReadTargetDoesNotJoinSameRunAcrossSessions(t *testing.T) {
	workspace := t.TempDir()
	store, err := team.NewEventStore(workspace, "run-1", "session-a")
	if err != nil {
		t.Fatal(err)
	}
	appendOverviewEvent(t, store, team.RunEvent{Type: "run_started", Actor: "coordinator", Payload: []byte(`{"goal":"first"}`)})
	appendOverviewEvent(t, store, team.RunEvent{RunID: "run-1", SessionID: "session-b", Type: "run_started", Actor: "coordinator", Payload: []byte(`{"goal":"second"}`)})
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	resolution, err := operatorpkg.ResolveWorkspacePath(operatorpkg.WorkspaceRequest{RequestedPath: workspace, Mode: "exact"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = BindReadTarget(t.Context(), operatorpkg.BindingRequest{Workspace: resolution, RunID: "run-1"})
	if !errors.Is(err, ErrAmbiguous) {
		t.Fatalf("cross-session collision error = %v, want ambiguous", err)
	}
}

func TestBindReadTargetRejectsExplicitProjectConflict(t *testing.T) {
	workspace := t.TempDir()
	store, err := team.NewEventStore(workspace, "run-1", "session-1")
	if err != nil {
		t.Fatal(err)
	}
	appendOverviewEvent(t, store, team.RunEvent{Type: "run_started", Actor: "coordinator", Payload: []byte(`{"scope":{"project_id":"project-a"}}`)})
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	resolution, err := operatorpkg.ResolveWorkspacePath(operatorpkg.WorkspaceRequest{RequestedPath: workspace, Mode: "exact"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = BindReadTarget(t.Context(), operatorpkg.BindingRequest{Workspace: resolution, RunID: "run-1", ProjectID: "project-b"})
	if !errors.Is(err, ErrScopeConflict) {
		t.Fatalf("project conflict error = %v, want scope conflict", err)
	}
}

func TestBindReadTargetDoesNotPromoteUnverifiedProjectSelector(t *testing.T) {
	fixture := buildRunFixture(t)
	resolution, err := operatorpkg.ResolveWorkspacePath(operatorpkg.WorkspaceRequest{RequestedPath: fixture.workspace, Mode: "exact"})
	if err != nil {
		t.Fatal(err)
	}
	bound, err := BindReadTarget(t.Context(), operatorpkg.BindingRequest{
		Workspace: resolution, RunID: fixture.runID, ProjectID: "caller-project",
	})
	if err != nil {
		t.Fatal(err)
	}
	if bound.Scope.ProjectID != "" {
		t.Fatalf("unpersisted project selector became canonical identity: %#v", bound.Scope)
	}
}

func TestBindReadTargetRejectsMalformedPersistedScope(t *testing.T) {
	lineage := Lineage{Events: []IndexedEvent{{Event: team.RunEvent{
		ID: "event-1", RunID: "run-1", Payload: []byte(`{"project_id":42}`),
	}}}}
	if _, _, err := persistedScopeIDs(lineage, "run-1"); !errors.Is(err, ErrIntegrity) {
		t.Fatalf("malformed scope error = %v, want integrity", err)
	}
}

func TestInspectOverviewReturnsCompleteStableSnapshotForFailedRun(t *testing.T) {
	workspace := filepath.Join(t.TempDir(), "workspace-name-is-not-team-identity")
	store, err := team.NewEventStore(workspace, "run-failed", "session-1")
	if err != nil {
		t.Fatal(err)
	}
	appendOverviewEvent(t, store, team.RunEvent{Type: "run_started", Actor: "coordinator", Payload: jsonBytes(t, map[string]any{
		"scope": map[string]string{"project_id": "project-1", "team_id": "team-1"},
	})})
	appendOverviewEvent(t, store, team.RunEvent{Type: "run_finished", Actor: "coordinator", Payload: jsonBytes(t, team.RunResult{
		RunID: "run-failed", Outcome: team.RunOutcomeFailed, StopReason: team.StopReasonRunFailed,
	})})
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	firstEnvelope, err := InspectOverview(t.Context(), InspectQuery{Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	secondEnvelope, err := InspectOverview(t.Context(), InspectQuery{Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	first := firstEnvelope.Data.(OverviewData)
	second := secondEnvelope.Data.(OverviewData)
	if first.Operation.Status != "succeeded" || first.Error != nil || first.Snapshot == nil {
		t.Fatalf("overview data = %#v", first)
	}
	snapshot := first.Snapshot
	if snapshot.Outcome.RunOutcome != string(team.RunOutcomeFailed) || snapshot.Activity.State != operatorpkg.ActivityFinished || snapshot.Attention != operatorpkg.AttentionReviewRequired {
		t.Fatalf("terminal failed snapshot = %#v", snapshot)
	}
	if snapshot.Scope.ProjectID != "project-1" || snapshot.Scope.TeamName != "team-1" || snapshot.Scope.SelectionSource != "single_candidate" {
		t.Fatalf("persisted scope = %#v", snapshot.Scope)
	}
	if snapshot.SnapshotID != second.Snapshot.SnapshotID || snapshot.Freshness.QueriedAt == "" {
		t.Fatalf("snapshot identity changed across query time: %q != %q", snapshot.SnapshotID, second.Snapshot.SnapshotID)
	}
	if len(snapshot.LatestChanges) != 2 || len(snapshot.RoleTargets) != 6 || snapshot.Learning.Exposures != nil || snapshot.Learning.Status != "unavailable" {
		t.Fatalf("bounded/unavailable adapters = changes:%#v roles:%#v learning:%#v", snapshot.LatestChanges, snapshot.RoleTargets, snapshot.Learning)
	}
	encoded, err := json.Marshal(firstEnvelope)
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{`"blockers":[]`, `"secondary_actions":[]`, `"warnings":[]`, `"error":null`, `"eligible_promotions":null`} {
		if !strings.Contains(string(encoded), required) {
			t.Fatalf("overview JSON missing %s: %s", required, encoded)
		}
	}
}

func TestInspectOverviewUsesGlobalOrdinalsAndLimitsLatestChanges(t *testing.T) {
	fixture := buildRunFixture(t)
	envelope, err := InspectOverview(t.Context(), InspectQuery{Workspace: fixture.workspace, RunID: fixture.runID})
	if err != nil {
		t.Fatal(err)
	}
	snapshot := envelope.Data.(OverviewData).Snapshot
	if snapshot.Scope.TeamName != "" {
		t.Fatalf("overview inferred team from workspace basename: %#v", snapshot.Scope)
	}
	if len(snapshot.LatestChanges) != 3 {
		t.Fatalf("latest changes = %d, want 3", len(snapshot.LatestChanges))
	}
	if snapshot.LatestChanges[0].EventOrdinal != 2 || snapshot.LatestChanges[2].EventOrdinal != 4 || snapshot.Freshness.EventOrdinal != 4 {
		t.Fatalf("global ordinals = changes %#v freshness %#v", snapshot.LatestChanges, snapshot.Freshness)
	}
}

func TestInspectOverviewDoesNotModifySuccessfulWorkspace(t *testing.T) {
	fixture := buildRunFixture(t)
	before := readWorkspaceFiles(t, fixture.workspace)
	if _, err := InspectOverview(t.Context(), InspectQuery{Workspace: fixture.workspace, RunID: fixture.runID}); err != nil {
		t.Fatal(err)
	}
	after := readWorkspaceFiles(t, fixture.workspace)
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("read-only overview changed workspace files: before=%v after=%v", mapKeys(before), mapKeys(after))
	}
}

func TestInspectOverviewNonterminalVerifyingIsNotInferredInterrupted(t *testing.T) {
	workspace := t.TempDir()
	store, err := team.NewEventStore(workspace, "run-active", "session-active")
	if err != nil {
		t.Fatal(err)
	}
	appendOverviewEvent(t, store, team.RunEvent{Type: "run_started", Actor: "coordinator", Payload: []byte(`{"goal":"verify"}`)})
	appendOverviewEvent(t, store, team.RunEvent{Type: "task_created", Actor: "coordinator", TaskID: "task-1", Payload: []byte(`{"id":"task-1","status":"pending"}`)})
	appendOverviewEvent(t, store, team.RunEvent{Type: "task_verifying", Actor: "worker", TaskID: "task-1", Payload: []byte(`{"id":"task-1","status":"verifying"}`)})
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if err := team.SaveSession(workspace, &team.SessionData{}); err != nil {
		t.Fatal(err)
	}
	envelope, err := InspectOverview(t.Context(), InspectQuery{Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	if got := envelope.Data.(OverviewData).Snapshot.Activity.State; got != operatorpkg.ActivityVerifying {
		t.Fatalf("activity = %q, want verifying", got)
	}
}

func TestInspectOverviewMissingWorkspaceIsReadOnly(t *testing.T) {
	workspace := filepath.Join(t.TempDir(), "missing")
	_, err := InspectOverview(t.Context(), InspectQuery{Workspace: workspace})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("error = %v, want not found", err)
	}
	if _, statErr := os.Stat(workspace); !os.IsNotExist(statErr) {
		t.Fatalf("read-only overview created workspace: %v", statErr)
	}
}

func appendOverviewEvent(t *testing.T, store *team.EventStore, event team.RunEvent) {
	t.Helper()
	if _, err := store.AppendPersisted(event); err != nil {
		t.Fatal(err)
	}
}

func readWorkspaceFiles(t *testing.T, workspace string) map[string]string {
	t.Helper()
	files := make(map[string]string)
	err := filepath.WalkDir(workspace, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		contents, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(workspace, path)
		if err != nil {
			return err
		}
		files[relative] = string(contents)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

func mapKeys(values map[string]string) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}
