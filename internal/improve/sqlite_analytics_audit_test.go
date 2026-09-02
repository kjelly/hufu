package improve

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func writeAuditJSONL(t *testing.T, dir, name string, lines []string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadAuditEvents_SkipsMalformedRowsAndKeepsOnlyMinimalFields(t *testing.T) {
	dir := t.TempDir()
	writeAuditJSONL(t, dir, "audit-2026-07-12.jsonl", []string{
		`{"timestamp":"2026-07-12T11:00:00Z","team":"dev","agent":"developer","event":"tool_call","input":"top-secret","command":"secret-command"}`,
		"{not valid json",
		`{"timestamp":"not-a-timestamp","team":"dev","agent":"developer","event":"tool_error","result":"secret-result"}`,
	})

	session := newTestSession(t)
	stats, err := session.loadAuditEvents(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if stats.FilesSeen != 1 || stats.LinesRead != 3 || stats.RowsLoaded != 2 || stats.MalformedRows != 1 || stats.InvalidTimestampRows != 1 {
		t.Fatalf("stats = %+v", stats)
	}

	rows, err := session.conn.QueryContext(context.Background(), "PRAGMA table_info(audit_events)")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var columns []string
	for rows.Next() {
		var cid int
		var name, columnType string
		var notNull, pk int
		var defaultValue sql.NullString
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &pk); err != nil {
			t.Fatal(err)
		}
		columns = append(columns, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	wantColumns := []string{"event_seq", "timestamp_raw", "timestamp_unix_ns", "team", "agent", "event"}
	if !reflect.DeepEqual(columns, wantColumns) {
		t.Fatalf("audit_events columns = %v, want %v", columns, wantColumns)
	}

	var raw string
	if err := session.conn.QueryRowContext(context.Background(), "SELECT timestamp_raw FROM audit_events WHERE event_seq = 1").Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(raw, "top-secret") {
		t.Fatal("audit payload leaked into timestamp_raw")
	}
}

func TestSQLAuditMetricsParity_InclusiveTeamAndTimeFiltering(t *testing.T) {
	dir := t.TempDir()
	writeAuditJSONL(t, dir, "audit-2026-07-12.jsonl", []string{
		`{"timestamp":"2026-07-12T10:59:59Z","team":"dev","agent":"developer","event":"tool_call"}`,
		`{"timestamp":"2026-07-12T11:00:00Z","team":"dev","agent":"developer","event":"tool_call"}`,
		`{"timestamp":"2026-07-12T11:00:05Z","team":"other","agent":"developer","event":"tool_call"}`,
		`{"timestamp":"2026-07-12T11:00:10Z","team":"dev","agent":"developer","event":"tool_error"}`,
		`{"timestamp":"2026-07-12T11:00:10Z","team":"dev","agent":"","event":"tool_call"}`,
		`{"timestamp":"2026-07-12T11:00:11Z","team":"dev","agent":"developer","event":"tool_call"}`,
		`{"timestamp":"not-a-timestamp","team":"dev","agent":"developer","event":"tool_call"}`,
		`{"timestamp":"2026-07-12T11:00:05Z","team":"dev","agent":"developer","event":"tool_result"}`,
	})

	start, _ := time.Parse(time.RFC3339, "2026-07-12T11:00:00Z")
	end, _ := time.Parse(time.RFC3339, "2026-07-12T11:00:10Z")
	want := &Metrics{
		ToolCalls: 5, ToolErrors: 5,
		ToolCallsByAgent:  map[string]int{"existing": 2},
		ToolErrorsByAgent: map[string]int{"existing": 1},
	}
	want.ToolCallsByAgent["developer"] = 1
	want.ToolCallsByAgent[""] = 1
	want.ToolErrorsByAgent["developer"] = 1

	session := newTestSession(t)
	if _, err := session.loadAuditEvents(context.Background(), dir); err != nil {
		t.Fatal(err)
	}
	got := &Metrics{
		ToolCalls: 3, ToolErrors: 4,
		ToolCallsByAgent:  map[string]int{"existing": 2},
		ToolErrorsByAgent: map[string]int{"existing": 1},
	}
	if err := session.sqlCollectAuditMetrics(context.Background(), "dev", start, end, got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(*got, *want) {
		t.Fatalf("audit metrics mismatch:\n  got  = %+v\n  want = %+v", *got, *want)
	}
}

func TestLoadAuditEvents_MissingDirectoryIsNonFatal(t *testing.T) {
	session := newTestSession(t)
	stats, err := session.loadAuditEvents(context.Background(), filepath.Join(t.TempDir(), "missing"))
	if err != nil {
		t.Fatal(err)
	}
	if stats != (auditLoadStats{}) {
		t.Fatalf("stats = %+v, want zero value", stats)
	}
}

func TestLoadAuditEvents_RollsBackOnScannerError(t *testing.T) {
	dir := t.TempDir()
	writeAuditJSONL(t, dir, "audit-2026-07-12.jsonl", []string{
		`{"timestamp":"2026-07-12T11:00:00Z","team":"dev","agent":"developer","event":"tool_call"}` + strings.Repeat("x", 4<<20),
	})

	session := newTestSession(t)
	if _, err := session.loadAuditEvents(context.Background(), dir); err == nil {
		t.Fatal("expected scanner error for an oversized audit line")
	}
	var count int
	if err := session.conn.QueryRowContext(context.Background(), "SELECT COUNT(*) FROM audit_events").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("audit_events rows = %d, want 0 after rollback", count)
	}
}

func TestSQLAuditMetrics_NilMetricsReturnsError(t *testing.T) {
	session := newTestSession(t)
	if err := session.sqlCollectAuditMetrics(context.Background(), "dev", time.Time{}, time.Time{}, nil); err == nil {
		t.Fatal("expected nil metrics error")
	}
}
