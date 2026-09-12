package inspect

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/kjelly/hufu/internal/agent"
	contextstore "github.com/kjelly/hufu/internal/context"
	"github.com/kjelly/hufu/internal/team"
)

func TestInspectReplayMatchesEligibleSessionProjection(t *testing.T) {
	fixture := buildRunFixture(t)
	saveCanonicalReplaySession(t, fixture)
	paths := []string{
		filepath.Join(fixture.workspace, "logs", "event_store.jsonl"),
		filepath.Join(fixture.workspace, "session.json"),
	}
	before := readExistingFiles(t, paths)
	envelope, err := InspectReplay(t.Context(), InspectQuery{Workspace: fixture.workspace, RunID: fixture.runID, SessionID: "session-inspect"})
	if err != nil {
		t.Fatal(err)
	}
	data := envelope.Data.(ReplayData)
	if data.OverallStatus != "match" {
		t.Fatalf("replay = %#v", data)
	}
	for _, name := range []string{"session.run", "session.tasks"} {
		check := findProjectionCheck(t, data.Checks, name)
		if check.Status != "match" {
			t.Fatalf("%s check = %#v", name, check)
		}
	}
	after := readExistingFiles(t, paths)
	for path, content := range before {
		if !bytes.Equal(content, after[path]) {
			t.Fatalf("replay changed %s", path)
		}
	}
}

func TestInspectReplayComparesMemoryAggregatesInMemory(t *testing.T) {
	workspace, observations := buildMemoryReplayFixture(t)
	repo, err := contextstore.OpenSQLite(filepath.Join(workspace, "context.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.Append(t.Context(), contextstore.ContextItem{ID: "memory-1", Kind: contextstore.ContextPattern, Content: "safe", Scope: contextstore.Scope{ProjectID: "project-1"}}); err != nil {
		t.Fatal(err)
	}
	if err := repo.RebuildExperienceAggregates(t.Context(), observations); err != nil {
		t.Fatal(err)
	}
	if err := repo.Close(); err != nil {
		t.Fatal(err)
	}
	envelope, err := InspectReplay(t.Context(), InspectQuery{Workspace: workspace, RunID: "run-memory"})
	if err != nil {
		t.Fatal(err)
	}
	if check := findProjectionCheck(t, envelope.Data.(ReplayData).Checks, "memory_aggregates"); check.Status != "match" {
		t.Fatalf("memory check = %#v", check)
	}
}

func TestInspectReplayDetectsMemoryAggregateDrift(t *testing.T) {
	workspace, observations := buildMemoryReplayFixture(t)
	repo, err := contextstore.OpenSQLite(filepath.Join(workspace, "context.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.Append(t.Context(), contextstore.ContextItem{ID: "memory-1", Kind: contextstore.ContextPattern, Content: "safe", Scope: contextstore.Scope{ProjectID: "project-1"}}); err != nil {
		t.Fatal(err)
	}
	drift := observations[0]
	drift.IdempotencyKey += ":projection-only"
	drift.ExposureDelta++
	if err := repo.RebuildExperienceAggregates(t.Context(), append(observations, drift)); err != nil {
		t.Fatal(err)
	}
	if err := repo.Close(); err != nil {
		t.Fatal(err)
	}

	envelope, err := InspectReplay(t.Context(), InspectQuery{Workspace: workspace, RunID: "run-memory"})
	if err != nil {
		t.Fatal(err)
	}
	check := findProjectionCheck(t, envelope.Data.(ReplayData).Checks, "memory_aggregates")
	if check.Status != "drift" || len(check.DiffPaths) == 0 {
		t.Fatalf("memory drift check = %#v", check)
	}
	if envelope.Data.(ReplayData).OverallStatus != "drift" {
		t.Fatalf("replay status = %#v", envelope.Data)
	}
}

func buildMemoryReplayFixture(t *testing.T) (string, []contextstore.ExperienceObservation) {
	t.Helper()
	workspace := t.TempDir()
	store, err := team.NewEventStore(workspace, "run-memory", "session-memory")
	if err != nil {
		t.Fatal(err)
	}
	appendEvent := func(event team.RunEvent) {
		t.Helper()
		if _, err := store.AppendPersisted(event); err != nil {
			t.Fatal(err)
		}
	}
	appendEvent(team.RunEvent{Type: "run_started", Actor: "coordinator", Payload: jsonBytes(t, map[string]any{"goal": "memory replay"})})
	appendEvent(team.RunEvent{
		Type: string(team.EventMemoryRetrieved), Actor: "runtime", TaskID: "task-memory", Attempt: 1,
		IdempotencyKey: "memory:retrieved:1", Timestamp: time.Date(2026, 2, 3, 4, 5, 6, 0, time.UTC).Format(time.RFC3339Nano),
		Payload: jsonBytes(t, map[string]any{"context_item_id": "memory-1", "policy_version": "memory-policy-v1", "project_id": "project-1", "prior_alpha": 1, "prior_beta": 1, "utility_percentile": 0.1}),
	})
	appendEvent(team.RunEvent{Type: "run_finished", Actor: "coordinator", Payload: jsonBytes(t, team.RunResult{RunID: "run-memory", Outcome: team.RunOutcomePartial, StopReason: team.StopReasonUnresolvedTasks})})
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	lineage, err := LoadLineage(t.Context(), InspectQuery{Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	events := make([]team.RunEvent, len(lineage.GlobalEvents))
	for index := range lineage.GlobalEvents {
		events[index] = lineage.GlobalEvents[index].Event
	}
	observations := team.ExperienceObservationsFromEvents(events, agent.DefaultMemoryLearningPolicy())
	return workspace, observations
}

func TestInspectReplayComparesTerminalLifecycleWithoutChangingTaskStatus(t *testing.T) {
	workspace := t.TempDir()
	store, err := team.NewEventStore(workspace, "run-terminal", "session-terminal")
	if err != nil {
		t.Fatal(err)
	}
	appendEvent := func(event team.RunEvent) {
		t.Helper()
		if _, err := store.AppendPersisted(event); err != nil {
			t.Fatal(err)
		}
	}
	appendEvent(team.RunEvent{Type: "run_started", Actor: "coordinator", Payload: jsonBytes(t, map[string]any{"goal": "terminal replay"})})
	terminal := team.TerminalSession{ID: "terminal-1", RunID: "run-terminal", OwnerTaskID: "task-terminal", ControllerTaskID: "task-terminal", Agent: "worker", State: team.TerminalSessionExited}
	appendEvent(team.RunEvent{Type: "terminal_session_exited", Actor: "terminal", TaskID: "task-terminal", Payload: jsonBytes(t, map[string]any{
		"session_id": terminal.ID, "run_id": terminal.RunID, "owner_task_id": terminal.OwnerTaskID, "controller_task_id": terminal.ControllerTaskID, "agent": terminal.Agent, "state": terminal.State, "output_refs": []team.ArtifactRef{},
	})})
	appendEvent(team.RunEvent{Type: "run_finished", Actor: "coordinator", Payload: jsonBytes(t, team.RunResult{RunID: "run-terminal", Outcome: team.RunOutcomePartial, StopReason: team.StopReasonUnresolvedTasks})})
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	terminalJSON, err := json.Marshal([]team.TerminalSession{terminal})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "logs", "terminal_sessions.json"), terminalJSON, 0o600); err != nil {
		t.Fatal(err)
	}
	envelope, err := InspectReplay(t.Context(), InspectQuery{Workspace: workspace, RunID: "run-terminal"})
	if err != nil {
		t.Fatal(err)
	}
	if check := findProjectionCheck(t, envelope.Data.(ReplayData).Checks, "terminal_sessions"); check.Status != "match" {
		t.Fatalf("terminal check = %#v", check)
	}
}

func TestInspectReplayReportsBoundedFieldPathsWithoutValues(t *testing.T) {
	fixture := buildRunFixture(t)
	saveCanonicalReplaySession(t, fixture)
	checkpoint, exists, err := team.LoadSessionReadOnly(fixture.workspace)
	if err != nil || !exists {
		t.Fatalf("load checkpoint: exists=%t err=%v", exists, err)
	}
	checkpoint.Tasks[0].Status = team.TaskError
	checkpoint.Tasks[0].Output = "private drift value"
	if err := team.SaveSession(fixture.workspace, checkpoint); err != nil {
		t.Fatal(err)
	}
	envelope, err := InspectReplay(t.Context(), InspectQuery{Workspace: fixture.workspace, RunID: fixture.runID, SessionID: "session-inspect"})
	if err != nil {
		t.Fatal(err)
	}
	data := envelope.Data.(ReplayData)
	if data.OverallStatus != "drift" || envelope.Integrity.Projection != "drift" {
		t.Fatalf("replay status = %#v integrity=%#v", data, envelope.Integrity)
	}
	taskCheck := findProjectionCheck(t, data.Checks, "session.tasks")
	if !slices.Contains(taskCheck.DiffPaths, "tasks[0].status") {
		t.Fatalf("task drift paths = %#v", taskCheck.DiffPaths)
	}
	encoded, err := json.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encoded, []byte("private drift value")) {
		t.Fatalf("replay leaked values: %s", encoded)
	}
}

func TestInspectReplayRejectsHistoricalSessionProjectionComparison(t *testing.T) {
	fixture := buildRunFixture(t)
	saveCanonicalReplaySession(t, fixture)
	store, err := team.NewEventStore(fixture.workspace, "run-later", "session-later")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.AppendPersisted(team.RunEvent{Type: "run_started", Actor: "coordinator", Payload: jsonBytes(t, map[string]any{"goal": "later"})}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	envelope, err := InspectReplay(t.Context(), InspectQuery{Workspace: fixture.workspace, RunID: fixture.runID, SessionID: "session-inspect"})
	if err != nil {
		t.Fatal(err)
	}
	data := envelope.Data.(ReplayData)
	for _, name := range []string{"session.run", "session.tasks"} {
		check := findProjectionCheck(t, data.Checks, name)
		if check.Status != "unavailable" || check.ReasonCode != ReasonProjectionNotRunScoped {
			t.Fatalf("historical %s check = %#v", name, check)
		}
	}
}

func TestInspectReplayReportsUnavailableOptionalSessionProjection(t *testing.T) {
	fixture := buildRunFixture(t)
	envelope, err := InspectReplay(t.Context(), InspectQuery{Workspace: fixture.workspace, RunID: fixture.runID})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"session.run", "session.tasks"} {
		check := findProjectionCheck(t, envelope.Data.(ReplayData).Checks, name)
		if check.Status != "unavailable" || check.ReasonCode != ReasonOptionalProjectionAbsent {
			t.Fatalf("optional %s check = %#v", name, check)
		}
	}
}

func TestInspectReplayReportsNonActiveSessionProjectionUnavailable(t *testing.T) {
	fixture := buildRunFixture(t)
	saveCanonicalReplaySession(t, fixture)
	lineage, err := LoadLineage(t.Context(), InspectQuery{Workspace: fixture.workspace})
	if err != nil {
		t.Fatal(err)
	}
	terminalID := lineage.Events[len(lineage.Events)-1].Event.ID
	tree := team.NewSessionTree()
	tree.Branches["feature"] = &team.SessionBranch{ID: "feature", Name: "feature", ParentID: "main", ForkEventID: terminalID}
	tree.ActiveBranch = "feature"
	if err := team.SaveSessionTree(fixture.workspace, tree); err != nil {
		t.Fatal(err)
	}
	envelope, err := InspectReplay(t.Context(), InspectQuery{Workspace: fixture.workspace, RunID: fixture.runID, BranchID: "main"})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"session.run", "session.tasks"} {
		check := findProjectionCheck(t, envelope.Data.(ReplayData).Checks, name)
		if check.Status != "unavailable" || check.ReasonCode != ReasonProjectionNotRunScoped {
			t.Fatalf("non-active %s check = %#v", name, check)
		}
	}
}

func TestInspectReplayReportsProjectionChangedDuringRead(t *testing.T) {
	fixture := buildRunFixture(t)
	saveCanonicalReplaySession(t, fixture)
	query := InspectQuery{Workspace: fixture.workspace, RunID: fixture.runID}
	lineage, err := LoadLineage(t.Context(), query)
	if err != nil {
		t.Fatal(err)
	}
	selected, err := selectRun(lineage, query)
	if err != nil {
		t.Fatal(err)
	}
	checks := replaySessionChecksWithReaders(query, lineage, selected, sessionProjectionReaders{
		loadSession: team.LoadSessionReadOnly,
		loadTree: func(string) (*team.SessionTree, error) {
			return &team.SessionTree{ActiveBranch: "changed"}, nil
		},
	})
	for _, check := range checks {
		if check.Status != "unavailable" || check.ReasonCode != ReasonProjectionChangedOnRead {
			t.Fatalf("changed projection check = %#v", check)
		}
	}
}

func TestInspectReplayRejectsBrokenGlobalHashChain(t *testing.T) {
	fixture := buildRunFixture(t)
	path := filepath.Join(fixture.workspace, "logs", "event_store.jsonl")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := bytes.Split(bytes.TrimSpace(data), []byte("\n"))
	if len(lines) < 2 {
		t.Fatalf("event fixture has %d lines", len(lines))
	}
	var event team.RunEvent
	if err := json.Unmarshal(lines[1], &event); err != nil {
		t.Fatal(err)
	}
	event.Payload = json.RawMessage(`{"tampered":true}`)
	lines[1], err = json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(bytes.Join(lines, []byte("\n")), '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = InspectReplay(t.Context(), InspectQuery{Workspace: fixture.workspace, RunID: fixture.runID})
	if !errors.Is(err, ErrIntegrity) {
		t.Fatalf("broken chain error = %v, want ErrIntegrity", err)
	}
}

func TestInspectReplayDoesNotInferTaskStatusFromTerminalProjection(t *testing.T) {
	fixture := buildRunFixture(t)
	saveCanonicalReplaySession(t, fixture)
	terminalJSON, err := json.Marshal([]team.TerminalSession{{
		ID: "orphan-terminal", RunID: fixture.runID, OwnerTaskID: fixture.taskID,
		State: team.TerminalSessionExited, Running: false,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fixture.workspace, "logs", "terminal_sessions.json"), terminalJSON, 0o600); err != nil {
		t.Fatal(err)
	}
	envelope, err := InspectReplay(t.Context(), InspectQuery{Workspace: fixture.workspace, RunID: fixture.runID})
	if err != nil {
		t.Fatal(err)
	}
	data := envelope.Data.(ReplayData)
	if findProjectionCheck(t, data.Checks, "terminal_sessions").Status != "drift" {
		t.Fatalf("terminal check = %#v", data.Checks)
	}
	if findProjectionCheck(t, data.Checks, "session.tasks").Status != "match" {
		t.Fatal("terminal process facts changed task projection result")
	}
}

func saveCanonicalReplaySession(t *testing.T, fixture runFixture) {
	t.Helper()
	lineage, err := LoadLineage(t.Context(), InspectQuery{Workspace: fixture.workspace})
	if err != nil {
		t.Fatal(err)
	}
	selected, err := selectRun(lineage, InspectQuery{RunID: fixture.runID})
	if err != nil {
		t.Fatal(err)
	}
	canonical := team.ReduceToSessionData(selected.raw[:selected.terminalIndex+1])
	if err := team.SaveSession(fixture.workspace, canonical); err != nil {
		t.Fatal(err)
	}
}

func findProjectionCheck(t *testing.T, checks []ProjectionCheck, name string) ProjectionCheck {
	t.Helper()
	for _, check := range checks {
		if check.Name == name {
			return check
		}
	}
	t.Fatalf("projection check %q not found", name)
	return ProjectionCheck{}
}

func readExistingFiles(t *testing.T, paths []string) map[string][]byte {
	t.Helper()
	out := make(map[string][]byte, len(paths))
	for _, path := range paths {
		content, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		out[path] = content
	}
	return out
}
