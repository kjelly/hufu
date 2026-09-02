package improve

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
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

func writeJSONLFile(t *testing.T, lines []string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "execution-events.jsonl")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadExecutionEvents_MissingFileIsNotAnError(t *testing.T) {
	session := newTestSession(t)
	stats, err := session.loadExecutionEvents(context.Background(), filepath.Join(t.TempDir(), "does-not-exist.jsonl"))
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if stats != (loadStats{}) {
		t.Fatalf("stats = %+v, want zero value", stats)
	}
}

func TestLoadExecutionEvents_SkipsMalformedAndMissingRunID(t *testing.T) {
	valid1, _ := json.Marshal(team.ExecutionEvent{Version: 1, Timestamp: "2026-07-12T10:00:00Z", RunID: "r1", Team: "dev", TaskID: "1", Status: "done"})
	valid2, _ := json.Marshal(team.ExecutionEvent{Version: 1, Timestamp: "2026-07-12T10:00:01Z", RunID: "r1", Team: "dev", TaskID: "2", Status: "done"})
	missingRunID, _ := json.Marshal(team.ExecutionEvent{Version: 1, Timestamp: "2026-07-12T10:00:02Z", RunID: "", Team: "dev", TaskID: "3", Status: "done"})
	path := writeJSONLFile(t, []string{
		string(valid1),
		"{not valid json",
		string(missingRunID),
		string(valid2),
	})

	session := newTestSession(t)
	stats, err := session.loadExecutionEvents(context.Background(), path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if stats.RowsLoaded != 2 || stats.MalformedRows != 1 || stats.MissingRunIDRows != 1 || stats.LinesRead != 4 {
		t.Fatalf("stats = %+v, want RowsLoaded=2 MalformedRows=1 MissingRunIDRows=1 LinesRead=4", stats)
	}

	var count int
	if err := session.conn.QueryRowContext(context.Background(), "SELECT COUNT(*) FROM execution_events").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("execution_events rows = %d, want 2", count)
	}
}

func TestLoadExecutionEvents_AssignsMonotonicEventSeqInFileOrder(t *testing.T) {
	e1, _ := json.Marshal(team.ExecutionEvent{Timestamp: "2026-07-12T10:00:00Z", RunID: "r1", Team: "dev", TaskID: "1"})
	e2, _ := json.Marshal(team.ExecutionEvent{Timestamp: "2026-07-12T10:00:00Z", RunID: "r1", Team: "dev", TaskID: "2"}) // same timestamp as e1
	path := writeJSONLFile(t, []string{string(e1), string(e2)})

	session := newTestSession(t)
	if _, err := session.loadExecutionEvents(context.Background(), path); err != nil {
		t.Fatal(err)
	}
	rows, err := session.conn.QueryContext(context.Background(), "SELECT event_seq, task_id FROM execution_events ORDER BY event_seq")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var got []string
	for rows.Next() {
		var seq int64
		var taskID string
		if err := rows.Scan(&seq, &taskID); err != nil {
			t.Fatal(err)
		}
		got = append(got, taskID)
	}
	if len(got) != 2 || got[0] != "1" || got[1] != "2" {
		t.Fatalf("task ids in event_seq order = %v, want [1 2] (file order preserved even with identical timestamps)", got)
	}
}

func TestLoadExecutionEvents_UnparseableTimestampYieldsNullUnixNS(t *testing.T) {
	e, _ := json.Marshal(team.ExecutionEvent{Timestamp: "not-a-timestamp", RunID: "r1", Team: "dev", TaskID: "1"})
	path := writeJSONLFile(t, []string{string(e)})

	session := newTestSession(t)
	if _, err := session.loadExecutionEvents(context.Background(), path); err != nil {
		t.Fatal(err)
	}
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

func TestLoadExecutionEvents_SkillsAreNormalizedAndDeduped(t *testing.T) {
	e, _ := json.Marshal(team.ExecutionEvent{Timestamp: "2026-07-12T10:00:00Z", RunID: "r1", Team: "dev", TaskID: "1", Skills: []string{" go ", "go", " review ", "\t"}})
	path := writeJSONLFile(t, []string{string(e)})

	session := newTestSession(t)
	if _, err := session.loadExecutionEvents(context.Background(), path); err != nil {
		t.Fatal(err)
	}
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

func TestLoadExecutionEvents_PreservesAllFieldsForOneRow(t *testing.T) {
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
		Provider:         "anthropic",
		FailureSignature: "",
	}
	data, _ := json.Marshal(event)
	path := writeJSONLFile(t, []string{string(data)})

	session := newTestSession(t)
	if _, err := session.loadExecutionEvents(context.Background(), path); err != nil {
		t.Fatal(err)
	}
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
