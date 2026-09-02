package improve

import (
	"context"
	"reflect"
	"testing"

	"github.com/kjelly/hufu/internal/team"
)

func loadFixtureLines(t *testing.T, session *sqliteAnalyticsSession, lines []string) {
	t.Helper()
	path := writeJSONLFile(t, lines)
	if _, err := session.loadExecutionEvents(context.Background(), path); err != nil {
		t.Fatalf("loadExecutionEvents: %v", err)
	}
}

func TestSQLRunSelectionFixedOrderingAndFiltering(t *testing.T) {
	t.Run("equal_end_uses_run_id_tie_break", func(t *testing.T) {
		session := newTestSession(t)
		loadFixtureEvents(t, session, []team.ExecutionEvent{
			{Timestamp: "2026-07-12T10:00:00Z", RunID: "run-b", Team: "dev", TaskID: "task", Status: "done"},
			{Timestamp: "2026-07-12T10:00:00Z", RunID: "run-a", Team: "dev", TaskID: "task", Status: "done"},
		})
		ctx := context.Background()
		teamName, runIDs, err := session.sqlSelectRecentRuns(ctx, "dev", 2)
		if err != nil {
			t.Fatal(err)
		}
		if teamName != "dev" || !reflect.DeepEqual(runIDs, []string{"run-a", "run-b"}) {
			t.Fatalf("selection = %q/%v, want dev/[run-a run-b]", teamName, runIDs)
		}
		teamName, runIDs, err = session.sqlSelectRecentRuns(ctx, "", 1)
		if err != nil {
			t.Fatal(err)
		}
		if teamName != "dev" || !reflect.DeepEqual(runIDs, []string{"run-b"}) {
			t.Fatalf("default selection = %q/%v, want dev/[run-b]", teamName, runIDs)
		}
	})

	t.Run("malformed_and_invalid_timestamps_are_excluded_from_order_and_window", func(t *testing.T) {
		session := newTestSession(t)
		loadFixtureLines(t, session, []string{
			"{not valid json",
			`{"timestamp":"not-a-timestamp","run_id":"invalid","team":"dev","task_id":"invalid-task","status":"done"}`,
			`{"timestamp":"2026-07-12T09:00:00Z","run_id":"window","team":"dev","status":"run_started"}`,
			`{"timestamp":"2026-07-12T09:00:05Z","run_id":"window","team":"dev","task_id":"task","status":"done"}`,
			`{"timestamp":"not-a-timestamp","run_id":"window","team":"dev","status":"run_finished"}`,
		})
		ctx := context.Background()
		_, runIDs, err := session.sqlSelectRecentRuns(ctx, "dev", 2)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(runIDs, []string{"invalid", "window"}) {
			t.Fatalf("run IDs = %v, want [invalid window]", runIDs)
		}
		metrics, err := session.sqlCollectExecutionMetrics(ctx, []string{"window"})
		if err != nil {
			t.Fatal(err)
		}
		if metrics.StartedAt != "2026-07-12T09:00:00Z" || metrics.EndedAt != "2026-07-12T09:00:05Z" {
			t.Fatalf("window = %s..%s, want 09:00:00..09:00:05", metrics.StartedAt, metrics.EndedAt)
		}
	})

	t.Run("default_team_and_empty_run_or_team_filtering", func(t *testing.T) {
		session := newTestSession(t)
		loadFixtureEvents(t, session, []team.ExecutionEvent{
			{Timestamp: "2026-07-12T09:00:00Z", RunID: "alpha-run", Team: "alpha", TaskID: "task", Status: "done"},
			{Timestamp: "2026-07-12T10:00:00Z", RunID: "beta-run", Team: "beta", TaskID: "task", Status: "done"},
			{Timestamp: "2026-07-12T11:00:00Z", RunID: "", Team: "beta", TaskID: "ignored", Status: "done"},
			{Timestamp: "2026-07-12T12:00:00Z", RunID: "empty-team-run", Team: "", TaskID: "ignored", Status: "done"},
		})
		ctx := context.Background()
		teamName, runIDs, err := session.sqlSelectRecentRuns(ctx, "", 1)
		if err != nil {
			t.Fatal(err)
		}
		if teamName != "beta" || !reflect.DeepEqual(runIDs, []string{"beta-run"}) {
			t.Fatalf("default selection = %q/%v, want beta/[beta-run]", teamName, runIDs)
		}
		_, runIDs, err = session.sqlSelectRecentRuns(ctx, "beta", 5)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(runIDs, []string{"beta-run"}) {
			t.Fatalf("beta runs = %v, want [beta-run]", runIDs)
		}
	})
}

func TestSQLTaskProjectionFixedMetadataRetryTokensAndSkillOverlap(t *testing.T) {
	session := newTestSession(t)
	loadFixtureEvents(t, session, []team.ExecutionEvent{
		{Timestamp: "2026-07-12T10:00:00Z", RunID: "r1", Team: "dev", TaskID: "task-1", Agent: "agent-a", Model: "small", TaskType: "coordinator", Attempt: 1, Status: "in_progress", Skills: []string{"go", "review"}, Usage: team.ExecutionUsage{TotalTokens: 10}},
		{Timestamp: "2026-07-12T10:00:01Z", RunID: "r1", Team: "dev", TaskID: "task-1", Attempt: 1, Status: "error", Skills: []string{"go", "review"}, Usage: team.ExecutionUsage{TotalTokens: 20}},
		{Timestamp: "2026-07-12T10:00:02Z", RunID: "r1", Team: "dev", TaskID: "task-1", Agent: "agent-b", Model: "large", TaskType: "agent", Attempt: 2, Status: "in_progress", Skills: []string{"review"}, Usage: team.ExecutionUsage{TotalTokens: 30}},
		{Timestamp: "2026-07-12T10:00:03Z", RunID: "r1", Team: "dev", TaskID: "task-1", Attempt: 2, Status: "done", Usage: team.ExecutionUsage{TotalTokens: 40}},
		{Timestamp: "2026-07-12T10:00:04Z", RunID: "r1", Team: "dev", TaskID: "task-2", Agent: "agent-a", Model: "small", TaskType: "coordinator", Attempt: 1, Status: "done", Skills: []string{"go", "review"}, Usage: team.ExecutionUsage{TotalTokens: 7}},
		{Timestamp: "2026-07-12T10:00:05Z", RunID: "r1", Team: "dev", TaskID: "task-3", Attempt: 1, Status: "planned", Usage: team.ExecutionUsage{TotalTokens: 5}},
	})
	ctx := context.Background()
	tasks, err := session.sqlTaskSummaries(ctx, []string{"r1"})
	if err != nil {
		t.Fatal(err)
	}
	wantTasks := []sqlTaskSummary{
		{RunID: "r1", TaskID: "task-1", Agent: "agent-b", Model: "large", TaskType: "agent", Skills: []string{"review"}, Terminal: "done", Attempts: 2, TotalAttempts: 2, TotalTokens: 100},
		{RunID: "r1", TaskID: "task-2", Agent: "agent-a", Model: "small", TaskType: "coordinator", Skills: []string{"go", "review"}, Terminal: "done", Attempts: 1, TotalAttempts: 0, TotalTokens: 7},
		{RunID: "r1", TaskID: "task-3", Terminal: "planned", Attempts: 1, TotalAttempts: 0, TotalTokens: 5},
	}
	if !reflect.DeepEqual(tasks, wantTasks) {
		t.Fatalf("task summaries = %+v, want %+v", tasks, wantTasks)
	}

	metrics, err := session.sqlCollectExecutionMetrics(ctx, []string{"r1"})
	if err != nil {
		t.Fatal(err)
	}
	wantMetrics := Metrics{
		RunID: "r1", RunCount: 1, StartedAt: "2026-07-12T10:00:00Z", EndedAt: "2026-07-12T10:00:05Z",
		TotalTasks: 3, Done: 2, Planned: 1, TotalAttempts: 2, RetriedTasks: 1, TotalTokens: 112,
		TokensByAgent:    map[string]int{"agent-a": 17, "agent-b": 30, "unspecified": 65},
		ToolCallsByAgent: map[string]int{}, ToolErrorsByAgent: map[string]int{},
	}
	if !reflect.DeepEqual(metrics, wantMetrics) {
		t.Fatalf("execution metrics = %+v, want %+v", metrics, wantMetrics)
	}

	groups, err := session.sqlCollectGroupedMetrics(ctx, []string{"r1"})
	if err != nil {
		t.Fatal(err)
	}
	wantGroups := GroupedMetrics{
		ByAgent: []GroupMetric{
			{Key: "agent-a", TotalTasks: 1, Done: 1, TotalTokens: 7},
			{Key: "agent-b", TotalTasks: 1, Done: 1, TotalAttempts: 2, RetriedTasks: 1, TotalTokens: 100},
			{Key: "unspecified", TotalTasks: 1, Planned: 1, TotalTokens: 5},
		},
		ByTaskType: []GroupMetric{
			{Key: "agent", TotalTasks: 1, Done: 1, TotalAttempts: 2, RetriedTasks: 1, TotalTokens: 100},
			{Key: "coordinator", TotalTasks: 1, Done: 1, TotalTokens: 7},
			{Key: "legacy/unspecified", TotalTasks: 1, Planned: 1, TotalTokens: 5},
		},
		ByModel: []GroupMetric{
			{Key: "large", TotalTasks: 1, Done: 1, TotalAttempts: 2, RetriedTasks: 1, TotalTokens: 100},
			{Key: "small", TotalTasks: 1, Done: 1, TotalTokens: 7},
			{Key: "unspecified", TotalTasks: 1, Planned: 1, TotalTokens: 5},
		},
		BySkill: []GroupMetric{
			{Key: "go", TotalTasks: 1, Done: 1, TotalTokens: 7},
			{Key: "none", TotalTasks: 1, Planned: 1, TotalTokens: 5},
			{Key: "review", TotalTasks: 2, Done: 2, TotalAttempts: 2, RetriedTasks: 1, TotalTokens: 107},
		},
	}
	if !reflect.DeepEqual(groups, wantGroups) {
		t.Fatalf("grouped metrics = %+v, want %+v", groups, wantGroups)
	}
}

func TestSQLGroupedMetricsZeroTaskRunReturnsNonNilEmptySlices(t *testing.T) {
	session := newTestSession(t)
	loadFixtureEvents(t, session, []team.ExecutionEvent{
		{Timestamp: "2026-07-12T10:00:00Z", RunID: "r1", Team: "dev", Status: "run_started"},
		{Timestamp: "2026-07-12T10:00:01Z", RunID: "r1", Team: "dev", Status: "run_finished"},
	})
	groups, err := session.sqlCollectGroupedMetrics(context.Background(), []string{"r1"})
	if err != nil {
		t.Fatal(err)
	}
	want := GroupedMetrics{
		ByAgent: []GroupMetric{}, ByTaskType: []GroupMetric{}, ByModel: []GroupMetric{}, BySkill: []GroupMetric{},
	}
	if !reflect.DeepEqual(groups, want) {
		t.Fatalf("zero-task groups = %+v, want non-nil empty slices %+v", groups, want)
	}
}
