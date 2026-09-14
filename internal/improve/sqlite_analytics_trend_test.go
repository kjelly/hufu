package improve

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/kjelly/hufu/internal/team"
)

type countingAnalyticsExecutor struct {
	base       analyticsQueryExecutor
	statements int
}

func (c *countingAnalyticsExecutor) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	c.statements++
	return c.base.ExecContext(ctx, query, args...)
}

func (c *countingAnalyticsExecutor) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	c.statements++
	return c.base.QueryContext(ctx, query, args...)
}

func (c *countingAnalyticsExecutor) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	c.statements++
	return c.base.QueryRowContext(ctx, query, args...)
}

func TestTrendAggregationQueryCountIsIndependentOfSelectedRunCount(t *testing.T) {
	events := make([]team.ExecutionEvent, 0, 200)
	base := time.Date(2026, time.July, 12, 10, 0, 0, 0, time.UTC)
	for run := range 100 {
		runID := fmt.Sprintf("run-%03d", run)
		events = append(events,
			team.ExecutionEvent{Timestamp: base.Add(time.Duration(run) * time.Minute).Format(time.RFC3339), RunID: runID, Team: "dev", TaskID: "task", Agent: "worker", Attempt: 1, Status: "in_progress"},
			team.ExecutionEvent{Timestamp: base.Add(time.Duration(run)*time.Minute + time.Second).Format(time.RFC3339), RunID: runID, Team: "dev", TaskID: "task", Agent: "worker", Attempt: 1, Status: "done"},
		)
	}
	counts := make(map[int]int)
	for _, runCount := range []int{1, 100} {
		session := newTestSession(t)
		loadFixtureEvents(t, session, events)
		if _, _, err := session.sqlSelectRecentRunSummaries(t.Context(), "dev", runCount); err != nil {
			t.Fatal(err)
		}
		if err := session.materializeTaskViews(t.Context()); err != nil {
			t.Fatal(err)
		}
		counter := &countingAnalyticsExecutor{base: session.executor}
		session.executor = counter
		metrics, err := session.sqlCollectExecutionMetrics(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		start, _ := time.Parse(time.RFC3339, metrics.StartedAt)
		end, _ := time.Parse(time.RFC3339, metrics.EndedAt)
		if err := session.sqlCollectAuditMetrics(t.Context(), "dev", start, end, &metrics); err != nil {
			t.Fatal(err)
		}
		_, revisions, err := session.sqlSelectedRevisions(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		memory, err := session.sqlCollectMemoryAnalytics(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if err := memory.apply(&metrics, allSelectedRunOrdinals); err != nil {
			t.Fatal(err)
		}
		if _, _, err := session.sqlMemoryEvidence(t.Context()); err != nil {
			t.Fatal(err)
		}
		if _, err := session.sqlCollectTrend(t.Context(), memory, revisions); err != nil {
			t.Fatal(err)
		}
		if _, err := session.sqlCollectGroupedMetrics(t.Context()); err != nil {
			t.Fatal(err)
		}
		counts[runCount] = counter.statements
	}
	if counts[1] != counts[100] {
		t.Fatalf("aggregation statements grow with selected runs: runCount=1: %d, runCount=100: %d", counts[1], counts[100])
	}
	t.Logf("aggregation statements: runCount=1: %d, runCount=100: %d", counts[1], counts[100])
}

func TestSQLCollectTrendPreservesNullWindowsRevisionsAndEmptyMaps(t *testing.T) {
	session := newTestSession(t)
	loadFixtureEvents(t, session, []team.ExecutionEvent{
		{Timestamp: "invalid", RunID: "null-window", Team: "dev", Status: "run_started", TeamRevision: "revision-old"},
		{Timestamp: "also-invalid", RunID: "null-window", Team: "dev", Status: "run_finished", TeamRevision: "revision-new"},
		{Timestamp: "2026-07-12T10:00:00Z", RunID: "zero-token", Team: "dev", TaskID: "task", Agent: "worker", Status: "done"},
	})
	if _, _, err := session.sqlSelectRecentRunSummaries(t.Context(), "dev", 2); err != nil {
		t.Fatal(err)
	}
	if err := session.materializeTaskViews(t.Context()); err != nil {
		t.Fatal(err)
	}
	memory, err := session.sqlCollectMemoryAnalytics(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	overallRevisions, revisions, err := session.sqlSelectedRevisions(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	trend, err := session.sqlCollectTrend(t.Context(), memory, revisions)
	if err != nil {
		t.Fatal(err)
	}
	if len(overallRevisions) != 2 || overallRevisions[0] != "revision-new" || overallRevisions[1] != "revision-old" {
		t.Fatalf("overall revisions = %v", overallRevisions)
	}
	if len(trend) != 2 || trend[0].RunID != "null-window" || trend[0].TeamRevision != "revision-new" {
		t.Fatalf("trend revisions/order = %+v", trend)
	}
	if trend[0].StartedAt != "0001-01-01T00:00:00Z" || trend[0].EndedAt != "0001-01-01T00:00:00Z" {
		t.Fatalf("null window = %s..%s", trend[0].StartedAt, trend[0].EndedAt)
	}
	for _, point := range trend {
		if point.Metrics.TokensByAgent == nil || point.Metrics.ToolCallsByAgent == nil || point.Metrics.ToolErrorsByAgent == nil {
			t.Fatalf("trend maps must be initialized: %+v", point)
		}
	}
}
