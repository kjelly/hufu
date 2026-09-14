package improve

import (
	"fmt"
	"strings"
	"testing"

	"github.com/kjelly/hufu/internal/team"
)

func explainQueryPlan(t *testing.T, executor analyticsQueryExecutor, query string, args ...any) []string {
	t.Helper()
	rows, err := executor.QueryContext(t.Context(), "EXPLAIN QUERY PLAN "+query, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var plan []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		plan = append(plan, fmt.Sprintf("%d/%d %s", id, parent, detail))
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return plan
}

func TestSQLiteAnalyticsPlannerUsesRunTaskIndexForSelectedProjection(t *testing.T) {
	session := newTestSession(t)
	loadFixtureEvents(t, session, []team.ExecutionEvent{
		{Timestamp: "2026-07-12T10:00:00Z", RunID: "r1", Team: "dev", TaskID: "task", Status: "in_progress"},
		{Timestamp: "2026-07-12T10:00:01Z", RunID: "r1", Team: "dev", TaskID: "task", Status: "done"},
	})
	if err := session.createIndexes(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, _, err := session.sqlSelectRecentRunSummaries(t.Context(), "dev", 1); err != nil {
		t.Fatal(err)
	}
	plan := explainQueryPlan(t, session.executor, `
SELECT e.run_id, e.task_id, e.event_seq
FROM selected_runs sr
JOIN execution_events e ON e.run_id = sr.run_id
WHERE e.task_id <> ''
ORDER BY e.run_id, e.task_id, e.event_seq`)
	if !strings.Contains(strings.Join(plan, "\n"), "idx_execution_run_task_seq") {
		t.Fatalf("run/task query plan does not use replacement index:\n%s", strings.Join(plan, "\n"))
	}
}
