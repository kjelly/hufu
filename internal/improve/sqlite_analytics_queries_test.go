package improve

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/kjelly/hufu/internal/team"
)

func loadFixtureEvents(t *testing.T, session *sqliteAnalyticsSession, events []team.ExecutionEvent) {
	t.Helper()
	lines := make([]string, 0, len(events))
	for _, event := range events {
		data, err := json.Marshal(event)
		if err != nil {
			t.Fatal(err)
		}
		lines = append(lines, string(data))
	}
	path := writeJSONLFile(t, lines)
	if _, err := session.loadExecutionEvents(context.Background(), path); err != nil {
		t.Fatalf("loadExecutionEvents: %v", err)
	}
}

func setTestSelectedRuns(t *testing.T, session *sqliteAnalyticsSession, runIDs ...string) {
	t.Helper()
	for ordinal, runID := range runIDs {
		if _, err := session.conn.ExecContext(t.Context(), insertSelectedRunSQL, runID, ordinal, "test", nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	session.selectedRunsReady = true
}

func TestSQLExecutionMetricsFixedRegression(t *testing.T) {
	events := []team.ExecutionEvent{
		{Version: 1, Timestamp: "2026-07-12T10:00:00Z", RunID: "old", Team: "dev", TaskID: "old-task", Agent: "developer", Attempt: 1, Status: "done"},
		{Version: 1, Timestamp: "2026-07-12T11:00:00Z", RunID: "latest", Team: "dev", TaskID: "task-1", Agent: "developer", Attempt: 1, Status: "in_progress"},
		{Version: 1, Timestamp: "2026-07-12T11:00:03Z", RunID: "latest", Team: "dev", TaskID: "task-1", Agent: "developer", Attempt: 1, Status: "error", Usage: team.ExecutionUsage{TotalTokens: 30}},
		{Version: 1, Timestamp: "2026-07-12T11:00:04Z", RunID: "latest", Team: "dev", TaskID: "task-1", Agent: "developer", Attempt: 2, Status: "in_progress"},
		{Version: 1, Timestamp: "2026-07-12T11:00:08Z", RunID: "latest", Team: "dev", TaskID: "task-1", Agent: "developer", Attempt: 2, Status: "done", Usage: team.ExecutionUsage{TotalTokens: 50}},
	}
	session := newTestSession(t)
	loadFixtureEvents(t, session, events)
	ctx := context.Background()
	gotTeam, gotRunIDs, err := session.sqlSelectRecentRuns(ctx, "", 1)
	if err != nil {
		t.Fatalf("sqlSelectRecentRuns: %v", err)
	}
	if gotTeam != "dev" || !reflect.DeepEqual(gotRunIDs, []string{"latest"}) {
		t.Fatalf("selection = %q/%v, want dev/[latest]", gotTeam, gotRunIDs)
	}

	got, err := session.sqlCollectExecutionMetrics(ctx)
	if err != nil {
		t.Fatalf("sqlCollectExecutionMetrics: %v", err)
	}
	want := Metrics{
		RunID: "latest", RunCount: 1,
		StartedAt: "2026-07-12T11:00:00Z", EndedAt: "2026-07-12T11:00:08Z",
		TotalTasks: 1, Done: 1, TotalAttempts: 2, RetriedTasks: 1, TotalTokens: 80,
		TokensByAgent:    map[string]int{"developer": 80},
		ToolCallsByAgent: map[string]int{}, ToolErrorsByAgent: map[string]int{},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("metrics mismatch:\n  got  = %+v\n  want = %+v", got, want)
	}
}

func TestSQLSelectRecentRuns_NoMatchingTeamReturnsNoRuns(t *testing.T) {
	events := []team.ExecutionEvent{
		{Timestamp: "2026-07-12T09:00:00Z", RunID: "r1", Team: "alpha", TaskID: "1", Status: "done"},
	}
	session := newTestSession(t)
	loadFixtureEvents(t, session, events)
	_, runIDs, err := session.sqlSelectRecentRuns(context.Background(), "does-not-exist", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(runIDs) != 0 {
		t.Fatalf("runIDs = %v, want empty", runIDs)
	}
	if !session.selectedRunsReady {
		t.Fatal("zero-row selection did not freeze the selected scope")
	}
	if _, _, err := session.sqlSelectRecentRuns(context.Background(), "alpha", 1); err == nil {
		t.Fatal("expected second selection after zero rows to fail")
	}
}

func TestSQLSelectRecentRunsUsesFirstQualifyingTeamAndReturnsOnlyRequestedRows(t *testing.T) {
	session := newTestSession(t)
	loadFixtureEvents(t, session, []team.ExecutionEvent{
		{Timestamp: "2026-07-12T09:00:00Z", RunID: "mixed", Team: "alpha", TaskID: "1", Status: "in_progress"},
		{Timestamp: "2026-07-12T12:00:00Z", RunID: "mixed", Team: "beta", TaskID: "1", Status: "done"},
		{Timestamp: "2026-07-12T10:00:00Z", RunID: "alpha-newer", Team: "alpha", TaskID: "2", Status: "done"},
		{Timestamp: "2026-07-12T11:00:00Z", RunID: "beta-only", Team: "beta", TaskID: "3", Status: "done"},
	})
	teamName, runs, err := session.sqlSelectRecentRunSummaries(t.Context(), "alpha", 10)
	if err != nil {
		t.Fatal(err)
	}
	if teamName != "alpha" || len(runs) != 2 || runs[0].RunID != "alpha-newer" || runs[1].RunID != "mixed" {
		t.Fatalf("selection = %q/%+v, want alpha/[alpha-newer mixed]", teamName, runs)
	}
	for ordinal, run := range runs {
		if run.Ordinal != ordinal {
			t.Fatalf("run %q ordinal = %d, want %d", run.RunID, run.Ordinal, ordinal)
		}
	}
}

func TestSQLiteAnalyticsSelectedRunLifecycleConstraints(t *testing.T) {
	t.Run("duplicate ordinal", func(t *testing.T) {
		session := newTestSession(t)
		setTestSelectedRuns(t, session, "first")
		if _, err := session.conn.ExecContext(t.Context(), insertSelectedRunSQL, "second", 0, "test", nil, nil); err == nil {
			t.Fatal("expected duplicate selected-run ordinal to fail")
		}
	})

	t.Run("execution ingestion after selection", func(t *testing.T) {
		session := newTestSession(t)
		loadFixtureEvents(t, session, []team.ExecutionEvent{{RunID: "r1", Team: "dev", TaskID: "1", Status: "done"}})
		if _, _, err := session.sqlSelectRecentRunSummaries(t.Context(), "dev", 1); err != nil {
			t.Fatal(err)
		}
		path := writeJSONLFile(t, []string{`{"run_id":"r2","team":"dev"}`})
		if _, err := session.loadExecutionEvents(t.Context(), path); err == nil {
			t.Fatal("expected ingestion after selection to fail")
		}
	})

	t.Run("task projection before selection", func(t *testing.T) {
		session := newTestSession(t)
		if err := session.materializeTaskViews(t.Context()); err == nil {
			t.Fatal("expected task projection before selection to fail")
		}
	})
}

func TestMaterializeTaskViewsScopesProjectionToSelectedRuns(t *testing.T) {
	session := newTestSession(t)
	loadFixtureEvents(t, session, []team.ExecutionEvent{
		{Timestamp: "2026-07-12T09:00:00Z", RunID: "old", Team: "dev", TaskID: "old-1", Status: "done"},
		{Timestamp: "2026-07-12T09:00:01Z", RunID: "old", Team: "dev", TaskID: "old-2", Status: "done"},
		{Timestamp: "2026-07-12T10:00:00Z", RunID: "latest", Team: "dev", TaskID: "selected", Status: "done"},
	})
	var diagnostics AnalyticsDiagnostics
	session.diagnostics = &diagnostics
	if _, _, err := session.sqlSelectRecentRunSummaries(t.Context(), "dev", 1); err != nil {
		t.Fatal(err)
	}
	if err := session.materializeTaskViews(t.Context()); err != nil {
		t.Fatal(err)
	}
	if diagnostics.ProjectedTasks != 1 {
		t.Fatalf("projected tasks = %d, want 1 selected task", diagnostics.ProjectedTasks)
	}
}
