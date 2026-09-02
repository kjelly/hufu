package improve

import (
	"bytes"
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/kjelly/hufu/internal/team"
)

func writeMemoryEventStore(t *testing.T, workspace string, events []team.RunEvent) string {
	t.Helper()
	store, err := team.NewEventStore(workspace, "memory-loader-run", "memory-loader-session")
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if err := store.Append(event); err != nil {
			_ = store.Close()
			t.Fatal(err)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(workspace, "logs", "event_store.jsonl")
}

func TestLoadMemoryEventsExtractsMinimalPayloadFields(t *testing.T) {
	workspace := t.TempDir()
	writeMemoryEventStore(t, workspace, []team.RunEvent{
		{ID: "run-start", Type: "run_started", Actor: "runtime", Payload: []byte(`{"secret":"do-not-store"}`)},
		{ID: "memory-retrieved", Type: memoryRetrievedEvent, Actor: "runtime", Timestamp: "2026-07-12T10:00:00Z", Payload: []byte(`{"retrieval_id":"retrieval-1","reason_code":"stale_environment","token_count":12,"secret":"memory-content"}`)},
		{ID: "memory-used", Type: memoryUsageRecordedEvent, Actor: "runtime", TaskID: "task-1", Attempt: 2, Payload: []byte(`{"retrieval_id":"retrieval-1","disposition":"applied","secret":"memory-content"}`)},
		{ID: "memory-outcome", Type: memoryOutcomeRecordedEvent, Actor: "runtime", Payload: []byte(`{"retrieval_id":"retrieval-1","signal":"verification_passed","direction":"negative","secret":"memory-content"}`)},
		{ID: "memory-bad-payload", Type: memoryRetrievedEvent, Actor: "runtime", Payload: []byte(`"not-an-object"`)},
	})

	session := newTestSession(t)
	stats, err := session.loadMemoryEvents(t.Context(), workspace)
	if err != nil {
		t.Fatalf("loadMemoryEvents: %v", err)
	}
	if stats.RowsLoaded != 3 || stats.EventsSkipped != 1 || stats.MalformedPayloadRows != 1 {
		t.Fatalf("stats = %+v, want three loaded, one skipped, one malformed payload", stats)
	}

	var count int
	if err := session.conn.QueryRowContext(t.Context(), "SELECT COUNT(*) FROM memory_events").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 3 {
		t.Fatalf("memory_events rows = %d, want 3", count)
	}
	rows, err := session.conn.QueryContext(t.Context(), `
SELECT type, retrieval_id, reason_code, token_count, disposition, signal, direction
FROM memory_events ORDER BY event_seq`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var got [][]any
	for rows.Next() {
		var eventType, retrievalID, reasonCode, disposition, signal, direction string
		var tokenCount int64
		if err := rows.Scan(&eventType, &retrievalID, &reasonCode, &tokenCount, &disposition, &signal, &direction); err != nil {
			t.Fatal(err)
		}
		got = append(got, []any{eventType, retrievalID, reasonCode, tokenCount, disposition, signal, direction})
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	want := [][]any{
		{memoryRetrievedEvent, "retrieval-1", "stale_environment", int64(12), "", "", ""},
		{memoryUsageRecordedEvent, "retrieval-1", "", int64(0), "applied", "", ""},
		{memoryOutcomeRecordedEvent, "retrieval-1", "", int64(0), "", "verification_passed", "negative"},
	}
	if len(got) != len(want) {
		t.Fatalf("memory rows = %#v, want %#v", got, want)
	}
	for i := range want {
		for j := range want[i] {
			if got[i][j] != want[i][j] {
				t.Fatalf("memory row %d = %#v, want %#v", i, got[i], want[i])
			}
		}
	}

	columns, err := session.conn.QueryContext(t.Context(), "PRAGMA table_info(memory_events)")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = columns.Close() }()
	for columns.Next() {
		var cid int
		var name, columnType string
		var notNull, primaryKey int
		var defaultValue sql.NullString
		if err := columns.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			t.Fatal(err)
		}
		if name == "payload" || name == "content" {
			t.Fatalf("memory_events stores sensitive raw column %q", name)
		}
	}
	if err := columns.Err(); err != nil {
		t.Fatal(err)
	}
}

func TestLoadMemoryEventsMissingStoreIsNonfatalAndDoesNotCreateFile(t *testing.T) {
	workspace := t.TempDir()
	session := newTestSession(t)
	stats, err := session.loadMemoryEvents(t.Context(), workspace)
	if err != nil {
		t.Fatalf("load missing event store: %v", err)
	}
	if stats != (memoryLoadStats{}) {
		t.Fatalf("stats = %+v, want zero value", stats)
	}
	if _, err := os.Stat(filepath.Join(workspace, "logs", "event_store.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("loader created missing event store: stat err = %v", err)
	}
}

func TestLoadMemoryEventsLateHashFailureRollsBackPrefix(t *testing.T) {
	workspace := t.TempDir()
	path := writeMemoryEventStore(t, workspace, []team.RunEvent{
		{ID: "memory-1", Type: memoryRetrievedEvent, Actor: "runtime", Payload: []byte(`{"retrieval_id":"r1"}`)},
		{ID: "memory-2", Type: memoryUsageRecordedEvent, Actor: "runtime", Payload: []byte(`{"disposition":"applied"}`)},
	})
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	corrupt, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	corrupt = bytes.Replace(corrupt, []byte(memoryUsageRecordedEvent), []byte("memory_usage_corrupt"), 1)
	if err := os.WriteFile(path, corrupt, 0o644); err != nil {
		t.Fatal(err)
	}

	session := newTestSession(t)
	stats, err := session.loadMemoryEvents(t.Context(), workspace)
	if err != nil {
		t.Fatalf("corrupt event store must be nonfatal: %v", err)
	}
	if stats.RowsLoaded != 1 {
		t.Fatalf("stats = %+v, want one streamed prefix row before rollback", stats)
	}
	var count int
	if err := session.conn.QueryRowContext(t.Context(), "SELECT COUNT(*) FROM memory_events").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("memory_events rows after late validation failure = %d, want zero", count)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(after, before) {
		t.Fatal("test corruption was not applied")
	}
	if !bytes.Equal(after, corrupt) {
		t.Fatal("loader changed canonical event store")
	}
}
