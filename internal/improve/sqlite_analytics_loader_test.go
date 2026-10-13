package improve

import (
	"context"
	"database/sql"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/kjelly/hufu/internal/team"
)

func newTestSession(t *testing.T) *sqliteAnalyticsSession {
	t.Helper()
	session, err := openSQLiteAnalyticsSession(context.Background())
	if err != nil {
		t.Fatalf("open analytics session: %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })
	return session
}

// insertFixtureRows inserts execution rows directly, for tests of the SQL
// that reads execution_events rather than of the event-store projection.
func insertFixtureRows(t *testing.T, session *sqliteAnalyticsSession, rows []team.ExecutionEvent) loadStats {
	t.Helper()
	tx, err := session.conn.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var stats loadStats
	if err := session.insertExecutionRows(context.Background(), tx, rows, &stats); err != nil {
		_ = tx.Rollback()
		t.Fatalf("insert execution rows: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	return stats
}

// loadFixtureLines inserts JSONL execution rows, skipping lines that do not
// decode, for SQL tests written as raw records.
func loadFixtureLines(t *testing.T, session *sqliteAnalyticsSession, lines []string) {
	t.Helper()
	var rows []team.ExecutionEvent
	for _, line := range lines {
		var row team.ExecutionEvent
		if json.Unmarshal([]byte(line), &row) == nil {
			rows = append(rows, row)
		}
	}
	insertFixtureRows(t, session, rows)
}

func executionTaskIDs(t *testing.T, session *sqliteAnalyticsSession) []string {
	t.Helper()
	rows, err := session.conn.QueryContext(context.Background(), "SELECT task_id FROM execution_events ORDER BY event_seq")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var got []string
	for rows.Next() {
		var taskID string
		if err := rows.Scan(&taskID); err != nil {
			t.Fatal(err)
		}
		got = append(got, taskID)
	}
	return got
}

func TestLoadEventStore_MissingStoreLoadsNothing(t *testing.T) {
	session := newTestSession(t)
	stats, err := session.loadEventStore(context.Background(), t.TempDir(), true)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if stats != (eventStoreLoadStats{}) {
		t.Fatalf("stats = %+v, want zero value", stats)
	}
}

func TestLoadEventStore_LoadsExecutionAndMemoryRowsInOnePass(t *testing.T) {
	workspace := t.TempDir()
	writeExecutionEvents(t, workspace, []team.ExecutionEvent{
		{Timestamp: "2026-07-12T10:00:00Z", RunID: "r1", Team: "dev", TaskID: "1", Agent: "developer", Attempt: 1, Status: "in_progress"},
		{Timestamp: "2026-07-12T10:00:00Z", RunID: "r1", Team: "dev", TaskID: "2", Agent: "developer", Attempt: 1, Status: "in_progress"},
		{Timestamp: "2026-07-12T10:00:01Z", RunID: "r1", Team: "dev", TaskID: "1", Agent: "developer", Attempt: 1, Status: "done"},
	})
	appendEventChain(t, workspace, []team.RunEvent{
		{RunID: "r1", TaskID: "1", Actor: "runtime", Type: memoryRetrievedEvent, Timestamp: "2026-07-12T10:00:00Z", Payload: []byte(`{"retrieval_id":"r","context_item_id":"c"}`)},
		{RunID: "r1", TaskID: "1", Actor: "runtime", Type: memoryRetrievedEvent, Timestamp: "2026-07-12T10:00:00Z", Payload: []byte(`true`)},
	})

	session := newTestSession(t)
	session.batchConfig.Execution = 32
	stats, err := session.loadEventStore(context.Background(), workspace, true)
	if err != nil {
		t.Fatal(err)
	}
	// run_started opens the run, then rows follow projection order even
	// when timestamps tie.
	if got := executionTaskIDs(t, session); !reflect.DeepEqual(got, []string{"", "1", "2", "1"}) {
		t.Fatalf("task ids in event_seq order = %v, want [\"\" 1 2 1]", got)
	}
	if stats.EventsRead != 6 || stats.Execution.RowsLoaded != 4 || stats.Memory.RowsLoaded != 1 || stats.Memory.MalformedPayloadRows != 1 {
		t.Fatalf("stats = %+v, want 6 events, 4 execution rows, 1 memory row, 1 malformed memory payload", stats)
	}
}

func TestLoadEventStore_ExecutionOnlySkipsMemoryRows(t *testing.T) {
	workspace := t.TempDir()
	writeExecutionEvents(t, workspace, []team.ExecutionEvent{{RunID: "r1", Team: "dev", TaskID: "1", Attempt: 1, Status: "done"}})
	appendEventChain(t, workspace, []team.RunEvent{{RunID: "r1", Actor: "runtime", Type: memoryRetrievedEvent, Payload: []byte(`{"retrieval_id":"r"}`)}})
	session := newTestSession(t)
	if _, err := session.loadEventStore(context.Background(), workspace, false); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := session.conn.QueryRowContext(context.Background(), "SELECT COUNT(*) FROM memory_events").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("memory_events rows = %d, want 0 for an execution-only load", count)
	}
}

func TestInsertExecutionRows_SkipsRowsWithoutRunID(t *testing.T) {
	session := newTestSession(t)
	stats := insertFixtureRows(t, session, []team.ExecutionEvent{
		{RunID: "r1", Team: "dev", TaskID: "1", Status: "done"},
		{RunID: "", Team: "dev", TaskID: "2", Status: "done"},
	})
	if stats.RowsLoaded != 1 || stats.MissingRunIDRows != 1 {
		t.Fatalf("stats = %+v, want 1 loaded and 1 missing run id", stats)
	}
}

func TestInsertExecutionRows_UnparseableTimestampYieldsNullUnixNS(t *testing.T) {
	session := newTestSession(t)
	session.batchConfig.Skills = 32
	insertFixtureRows(t, session, []team.ExecutionEvent{{Timestamp: "not-a-timestamp", RunID: "r1", Team: "dev", TaskID: "1"}})
	var raw string
	var unixNS sql.NullInt64
	if err := session.conn.QueryRowContext(context.Background(), "SELECT timestamp_raw, timestamp_unix_ns FROM execution_events").Scan(&raw, &unixNS); err != nil {
		t.Fatal(err)
	}
	if raw != "not-a-timestamp" {
		t.Fatalf("timestamp_raw = %q, want preserved raw value", raw)
	}
	if unixNS.Valid {
		t.Fatalf("timestamp_unix_ns = %v, want NULL for an unparseable timestamp", unixNS)
	}
}

func TestInsertExecutionRows_SkillsAreNormalizedAndDeduped(t *testing.T) {
	session := newTestSession(t)
	insertFixtureRows(t, session, []team.ExecutionEvent{{Timestamp: "2026-07-12T10:00:00Z", RunID: "r1", Team: "dev", TaskID: "1", Skills: []string{" go ", "go", " review ", "\t"}}})
	rows, err := session.conn.QueryContext(context.Background(), "SELECT skill FROM execution_event_skills ORDER BY skill")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var skills []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		skills = append(skills, s)
	}
	if len(skills) != 2 || skills[0] != "go" || skills[1] != "review" {
		t.Fatalf("skills = %v, want [go review] deduped", skills)
	}
}

func TestInsertExecutionRows_PreservesAllFieldsForOneRow(t *testing.T) {
	event := team.ExecutionEvent{
		Version: 3, Timestamp: "2026-07-12T10:00:00.5Z", RunID: "r1", Team: "dev", TaskID: "1",
		Agent: "developer", Attempt: 2, Status: "done", Model: "large", TaskType: "agent",
		TeamRevision: "rev-a", DurationMS: 1234,
		Usage:            team.ExecutionUsage{InputTokens: 10, OutputTokens: 20, TotalTokens: 30, ProgressTokens: 5},
		Outcome:          team.RunOutcome("goal_satisfied"),
		StopReason:       team.StopReason("completed"),
		AcceptanceState:  team.AcceptanceState("passed"),
		RepairAttempts:   1,
		Phase:            team.Phase("execute"),
		FailureSignature: "",
	}
	session := newTestSession(t)
	insertFixtureRows(t, session, []team.ExecutionEvent{event})
	var got team.ExecutionEvent
	var outcome, stopReason, acceptanceState, phase string
	row := session.conn.QueryRowContext(context.Background(), `SELECT version, timestamp_raw, run_id, team, task_id, agent, attempt,
		status, model, task_type, team_revision, duration_ms, input_tokens, output_tokens, total_tokens,
		progress_tokens, outcome, stop_reason, acceptance_state, repair_attempts, phase, provider, failure_signature
		FROM execution_events`)
	if err := row.Scan(&got.Version, &got.Timestamp, &got.RunID, &got.Team, &got.TaskID, &got.Agent, &got.Attempt,
		&got.Status, &got.Model, &got.TaskType, &got.TeamRevision, &got.DurationMS, &got.Usage.InputTokens, &got.Usage.OutputTokens,
		&got.Usage.TotalTokens, &got.Usage.ProgressTokens, &outcome, &stopReason, &acceptanceState, &got.RepairAttempts,
		&phase, &got.Provider, &got.FailureSignature); err != nil {
		t.Fatal(err)
	}
	got.Outcome, got.StopReason, got.AcceptanceState, got.Phase = team.RunOutcome(outcome), team.StopReason(stopReason), team.AcceptanceState(acceptanceState), team.Phase(phase)
	if !reflect.DeepEqual(got, event) {
		t.Fatalf("round-tripped event = %+v, want %+v", got, event)
	}
}
