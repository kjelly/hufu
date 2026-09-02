package improve

// Parity tests (spec.md §14.2): every SQL query in
// sqlite_analytics_queries.go must reproduce the legacy Go aggregation
// exactly for the same fixture, before any legacy code is removed (WP-9).
//
// Timestamps in these fixtures always end in "Z" (UTC), matching how
// production code writes them (time.Now().UTC().Format(time.RFC3339Nano) —
// see internal/team/execution_events.go). Under that condition legacy's
// time.Parse+Format round-trip and the SQL path's
// time.Unix(0,ns).UTC().Format round-trip produce identical strings; a
// fixture using a non-UTC offset would not be representative of real data
// and is out of scope here.

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

func runIDsOf(runs []executionRun) []string {
	ids := make([]string, len(runs))
	for i, r := range runs {
		ids[i] = r.ID
	}
	return ids
}

type executionMetricsParityCase struct {
	name      string
	events    []team.ExecutionEvent
	teamName  string
	runCount  int
	wantEmpty bool // true when neither legacy nor SQL should find a selected run
}

func executionMetricsParityCases() []executionMetricsParityCase {
	return []executionMetricsParityCase{
		{
			name: "single_run_with_retry_and_two_runs_in_workspace",
			events: []team.ExecutionEvent{
				{Version: 1, Timestamp: "2026-07-12T10:00:00Z", RunID: "old", Team: "dev", TaskID: "1", Agent: "developer", Attempt: 1, Status: "done"},
				{Version: 1, Timestamp: "2026-07-12T11:00:00Z", RunID: "latest", Team: "dev", TaskID: "2", Agent: "developer", Attempt: 1, Status: "in_progress"},
				{Version: 1, Timestamp: "2026-07-12T11:00:03Z", RunID: "latest", Team: "dev", TaskID: "2", Agent: "developer", Attempt: 1, Status: "error", Usage: team.ExecutionUsage{TotalTokens: 30}},
				{Version: 1, Timestamp: "2026-07-12T11:00:04Z", RunID: "latest", Team: "dev", TaskID: "2", Agent: "developer", Attempt: 2, Status: "in_progress"},
				{Version: 1, Timestamp: "2026-07-12T11:00:08Z", RunID: "latest", Team: "dev", TaskID: "2", Agent: "developer", Attempt: 2, Status: "done", Usage: team.ExecutionUsage{TotalTokens: 50}},
			},
			teamName: "dev", runCount: 1,
		},
		{
			name: "multi_run_multi_team_workspace",
			events: []team.ExecutionEvent{
				{Version: 2, Timestamp: "2026-07-12T10:00:00Z", RunID: "dev-1", Team: "dev", TaskID: "1", Agent: "developer", Attempt: 1, Status: "in_progress", Model: "small", TaskType: "coordinator", Skills: []string{"go"}, TeamRevision: "rev-a"},
				{Version: 2, Timestamp: "2026-07-12T10:00:03Z", RunID: "dev-1", Team: "dev", TaskID: "1", Agent: "developer", Attempt: 1, Status: "done", Model: "small", TaskType: "coordinator", Skills: []string{"go"}, TeamRevision: "rev-a", Usage: team.ExecutionUsage{TotalTokens: 10}},
				{Version: 2, Timestamp: "2026-07-12T11:00:00Z", RunID: "dev-2", Team: "dev", TaskID: "1", Agent: "developer", Attempt: 1, Status: "in_progress", Model: "large", TaskType: "agent", TeamRevision: "rev-b"},
				{Version: 2, Timestamp: "2026-07-12T11:00:02Z", RunID: "dev-2", Team: "dev", TaskID: "1", Agent: "developer", Attempt: 1, Status: "error", Model: "large", TaskType: "agent", TeamRevision: "rev-b", Usage: team.ExecutionUsage{TotalTokens: 5}},
				{Version: 2, Timestamp: "2026-07-12T11:00:04Z", RunID: "dev-2", Team: "dev", TaskID: "1", Agent: "developer", Attempt: 2, Status: "in_progress", Model: "large", TaskType: "agent", Skills: []string{"go", "review"}, TeamRevision: "rev-b"},
				{Version: 2, Timestamp: "2026-07-12T11:00:06Z", RunID: "dev-2", Team: "dev", TaskID: "1", Agent: "developer", Attempt: 2, Status: "done", Model: "large", TaskType: "agent", Skills: []string{"go", "review"}, TeamRevision: "rev-b", Usage: team.ExecutionUsage{TotalTokens: 15}},
				{Version: 2, Timestamp: "2026-07-12T12:00:00Z", RunID: "other-1", Team: "other", TaskID: "1", Agent: "helper", Attempt: 1, Status: "done", Usage: team.ExecutionUsage{TotalTokens: 99}},
			},
			teamName: "dev", runCount: 2,
		},
		{
			name: "late_metadata_attributes_to_whole_task",
			events: []team.ExecutionEvent{
				{Timestamp: "2026-07-12T10:00:00Z", RunID: "r1", Team: "dev", TaskID: "1", Attempt: 1, Status: "in_progress"},
				{Timestamp: "2026-07-12T10:00:01Z", RunID: "r1", Team: "dev", TaskID: "1", Attempt: 1, Status: "error"},
				{Timestamp: "2026-07-12T10:00:02Z", RunID: "r1", Team: "dev", TaskID: "1", Attempt: 2, Status: "in_progress", Agent: "developer", Model: "large", TaskType: "agent"},
				{Timestamp: "2026-07-12T10:00:03Z", RunID: "r1", Team: "dev", TaskID: "1", Attempt: 2, Status: "done"},
			},
			teamName: "dev", runCount: 1,
		},
		{
			name: "skill_only_on_retry",
			events: []team.ExecutionEvent{
				{Timestamp: "2026-07-12T10:00:00Z", RunID: "r1", Team: "dev", TaskID: "1", Attempt: 1, Status: "in_progress"},
				{Timestamp: "2026-07-12T10:00:01Z", RunID: "r1", Team: "dev", TaskID: "1", Attempt: 1, Status: "error"},
				{Timestamp: "2026-07-12T10:00:02Z", RunID: "r1", Team: "dev", TaskID: "1", Attempt: 2, Status: "in_progress", Skills: []string{"go"}},
				{Timestamp: "2026-07-12T10:00:03Z", RunID: "r1", Team: "dev", TaskID: "1", Attempt: 2, Status: "done", Skills: []string{"go"}},
			},
			teamName: "dev", runCount: 1,
		},
		{
			name: "skills_overwrite_not_union_across_events",
			events: []team.ExecutionEvent{
				{Timestamp: "2026-07-12T10:00:00Z", RunID: "r1", Team: "dev", TaskID: "1", Attempt: 1, Status: "in_progress", Skills: []string{"go"}},
				{Timestamp: "2026-07-12T10:00:01Z", RunID: "r1", Team: "dev", TaskID: "1", Attempt: 1, Status: "error", Skills: []string{"go"}},
				{Timestamp: "2026-07-12T10:00:02Z", RunID: "r1", Team: "dev", TaskID: "1", Attempt: 2, Status: "in_progress", Skills: []string{"review"}},
				{Timestamp: "2026-07-12T10:00:03Z", RunID: "r1", Team: "dev", TaskID: "1", Attempt: 2, Status: "done", Skills: []string{"review"}},
			},
			teamName: "dev", runCount: 1,
		},
		{
			name: "v1_minimal_fields",
			events: []team.ExecutionEvent{
				{Version: 1, Timestamp: "2026-07-12T10:00:00Z", RunID: "r1", Team: "dev", TaskID: "1", Agent: "developer", Attempt: 1, Status: "in_progress"},
				{Version: 1, Timestamp: "2026-07-12T10:00:01Z", RunID: "r1", Team: "dev", TaskID: "1", Agent: "developer", Attempt: 1, Status: "done", Usage: team.ExecutionUsage{TotalTokens: 3}},
			},
			teamName: "dev", runCount: 1,
		},
		{
			name: "same_end_timestamp_tie_break_by_run_id",
			events: []team.ExecutionEvent{
				{Timestamp: "2026-07-12T10:00:00Z", RunID: "run-b", Team: "dev", TaskID: "1", Status: "done"},
				{Timestamp: "2026-07-12T10:00:00Z", RunID: "run-a", Team: "dev", TaskID: "1", Status: "done"},
			},
			teamName: "dev", runCount: 5,
		},
		{
			name: "missing_agent_falls_back_to_unspecified",
			events: []team.ExecutionEvent{
				{Timestamp: "2026-07-12T10:00:00Z", RunID: "r1", Team: "dev", TaskID: "1", Attempt: 1, Status: "done", Usage: team.ExecutionUsage{TotalTokens: 7}},
			},
			teamName: "dev", runCount: 1,
		},
		{
			name: "unparseable_and_missing_timestamps_do_not_crash",
			events: []team.ExecutionEvent{
				{Timestamp: "2026-07-12T09:00:00Z", RunID: "r1", Team: "dev", TaskID: "", Status: "run_started"},
				{Timestamp: "2026-07-12T09:00:05Z", RunID: "r1", Team: "dev", TaskID: "1", Status: "done"},
				{Timestamp: "not-a-timestamp", RunID: "r1", Team: "dev", TaskID: "", Status: "run_finished"},
			},
			teamName: "dev", runCount: 1,
		},
		{
			name: "empty_team_name_defaults_to_latest_runs_team",
			events: []team.ExecutionEvent{
				{Timestamp: "2026-07-12T09:00:00Z", RunID: "r1", Team: "alpha", TaskID: "1", Status: "done"},
				{Timestamp: "2026-07-12T10:00:00Z", RunID: "r2", Team: "beta", TaskID: "1", Status: "done"},
			},
			teamName: "", runCount: 5,
		},
		{
			name: "events_without_run_id_or_team_are_dropped_before_selection",
			events: []team.ExecutionEvent{
				{Timestamp: "2026-07-12T09:00:00Z", RunID: "", Team: "dev", TaskID: "1", Status: "done"},
				{Timestamp: "2026-07-12T09:00:00Z", RunID: "r1", Team: "", TaskID: "1", Status: "done"},
				{Timestamp: "2026-07-12T09:00:00Z", RunID: "r2", Team: "dev", TaskID: "1", Status: "done", Usage: team.ExecutionUsage{TotalTokens: 4}},
			},
			teamName: "dev", runCount: 5,
		},
	}
}

func TestSQLExecutionMetricsParity(t *testing.T) {
	for _, tc := range executionMetricsParityCases() {
		t.Run(tc.name, func(t *testing.T) {
			wantTeam, wantRuns := selectRecentRuns(tc.events, tc.teamName, tc.runCount)
			wantRunIDs := runIDsOf(wantRuns)
			wantMetrics := collectExecutionMetrics(flattenRuns(wantRuns))

			session := newTestSession(t)
			loadFixtureEvents(t, session, tc.events)
			ctx := context.Background()
			gotTeam, gotRunIDs, err := session.sqlSelectRecentRuns(ctx, tc.teamName, tc.runCount)
			if err != nil {
				t.Fatalf("sqlSelectRecentRuns: %v", err)
			}
			if gotTeam != wantTeam {
				t.Fatalf("resolved team = %q, want %q", gotTeam, wantTeam)
			}
			if !reflect.DeepEqual(gotRunIDs, wantRunIDs) {
				t.Fatalf("run ids = %v, want %v", gotRunIDs, wantRunIDs)
			}

			gotMetrics, err := session.sqlCollectExecutionMetrics(ctx, gotRunIDs)
			if err != nil {
				t.Fatalf("sqlCollectExecutionMetrics: %v", err)
			}
			if !reflect.DeepEqual(gotMetrics, wantMetrics) {
				t.Fatalf("metrics mismatch:\n  got  = %+v\n  want = %+v", gotMetrics, wantMetrics)
			}
		})
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
}
