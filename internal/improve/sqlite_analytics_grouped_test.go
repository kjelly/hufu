package improve

import (
	"context"
	"reflect"
	"testing"

	"github.com/kjelly/hufu/internal/team"
)

func TestSQLGroupedMetricsParity(t *testing.T) {
	cases := executionMetricsParityCases()
	cases = append(cases, executionMetricsParityCase{
		name: "run_with_no_task_bearing_events",
		events: []team.ExecutionEvent{
			{Timestamp: "2026-07-12T09:00:00Z", RunID: "r1", Team: "dev", TaskID: "", Status: "run_started"},
			{Timestamp: "2026-07-12T09:00:01Z", RunID: "r1", Team: "dev", TaskID: "", Status: "run_finished"},
		},
		teamName: "dev", runCount: 1,
	})

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, wantRuns := selectRecentRuns(tc.events, tc.teamName, tc.runCount)
			wantGrouped := collectGroupedMetrics(flattenRuns(wantRuns))

			session := newTestSession(t)
			loadFixtureEvents(t, session, tc.events)
			ctx := context.Background()
			_, gotRunIDs, err := session.sqlSelectRecentRuns(ctx, tc.teamName, tc.runCount)
			if err != nil {
				t.Fatalf("sqlSelectRecentRuns: %v", err)
			}
			gotGrouped, err := session.sqlCollectGroupedMetrics(ctx, gotRunIDs)
			if err != nil {
				t.Fatalf("sqlCollectGroupedMetrics: %v", err)
			}
			if !reflect.DeepEqual(gotGrouped, wantGrouped) {
				t.Fatalf("grouped metrics mismatch:\n  got  = %+v\n  want = %+v", gotGrouped, wantGrouped)
			}
		})
	}
}

func TestSQLGroupedMetrics_MultiSkillOverlapAndMissingDimensionFallback(t *testing.T) {
	events := []team.ExecutionEvent{
		{Timestamp: "2026-07-12T10:00:00Z", RunID: "r1", Team: "dev", TaskID: "1", Status: "done", Skills: []string{"go"}},
		{Timestamp: "2026-07-12T10:00:01Z", RunID: "r1", Team: "dev", TaskID: "2", Status: "done", Skills: []string{"go", "review"}},
		{Timestamp: "2026-07-12T10:00:02Z", RunID: "r1", Team: "dev", TaskID: "3", Status: "done"}, // no agent/model/task_type/skills at all
	}
	session := newTestSession(t)
	loadFixtureEvents(t, session, events)
	ctx := context.Background()
	_, runIDs, err := session.sqlSelectRecentRuns(ctx, "dev", 1)
	if err != nil {
		t.Fatal(err)
	}
	got, err := session.sqlCollectGroupedMetrics(ctx, runIDs)
	if err != nil {
		t.Fatal(err)
	}
	if g := groupByKey(got.BySkill, "go"); g == nil || g.TotalTasks != 2 {
		t.Fatalf("go skill group = %+v, want 2 tasks", g)
	}
	if g := groupByKey(got.BySkill, "review"); g == nil || g.TotalTasks != 1 {
		t.Fatalf("review skill group = %+v, want 1 task", g)
	}
	if g := groupByKey(got.BySkill, "none"); g == nil || g.TotalTasks != 1 {
		t.Fatalf("none skill group = %+v, want 1 task", g)
	}
	if g := groupByKey(got.ByAgent, "unspecified"); g == nil || g.TotalTasks != 3 {
		t.Fatalf("agent fallback group = %+v, want 3 tasks", g)
	}
	if g := groupByKey(got.ByTaskType, "legacy/unspecified"); g == nil || g.TotalTasks != 3 {
		t.Fatalf("task type fallback group = %+v, want 3 tasks", g)
	}

	want := collectGroupedMetrics(events)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("mismatch vs legacy:\n  got  = %+v\n  want = %+v", got, want)
	}
}

func TestSQLGroupedMetrics_WhitespaceFallbackAndSkillNormalizationParity(t *testing.T) {
	events := []team.ExecutionEvent{
		{Timestamp: "2026-07-12T10:00:00Z", RunID: "r1", Team: "dev", TaskID: "1", Agent: " ", Model: "\t", TaskType: "  ", Status: "in_progress", Skills: []string{" go ", "go", " review "}},
		{Timestamp: "2026-07-12T10:00:01Z", RunID: "r1", Team: "dev", TaskID: "1", Agent: "developer", Model: "large", TaskType: "agent", Status: "done", Skills: []string{"\t"}},
		{Timestamp: "2026-07-12T10:00:02Z", RunID: "r1", Team: "dev", TaskID: "2", Agent: "\t", Model: "\u00a0", TaskType: "\v", Status: "done", Skills: []string{" go ", "go"}},
	}

	session := newTestSession(t)
	loadFixtureEvents(t, session, events)
	ctx := context.Background()
	_, runIDs, err := session.sqlSelectRecentRuns(ctx, "dev", 1)
	if err != nil {
		t.Fatal(err)
	}
	got, err := session.sqlCollectGroupedMetrics(ctx, runIDs)
	if err != nil {
		t.Fatal(err)
	}
	want := collectGroupedMetrics(events)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("mismatch vs legacy:\n  got  = %+v\n  want = %+v", got, want)
	}
}

func TestSQLiteAnalyticsSession_RejectsExecutionIngestAfterTaskProjection(t *testing.T) {
	events := []team.ExecutionEvent{{RunID: "r1", Team: "dev", TaskID: "1", Status: "done"}}
	session := newTestSession(t)
	loadFixtureEvents(t, session, events)
	if _, err := session.sqlCollectGroupedMetrics(context.Background(), []string{"r1"}); err != nil {
		t.Fatal(err)
	}

	path := writeJSONLFile(t, []string{`{"run_id":"r2","team":"dev","task_id":"2","status":"done"}`})
	if _, err := session.loadExecutionEvents(context.Background(), path); err == nil {
		t.Fatal("expected execution ingestion after task projection to fail")
	}
	var count int
	if err := session.conn.QueryRowContext(context.Background(), "SELECT COUNT(*) FROM execution_events").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("execution event count = %d, want 1 after rejected ingest", count)
	}
}
