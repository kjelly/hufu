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

	got, err := session.sqlCollectExecutionMetrics(ctx, gotRunIDs)
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
}
