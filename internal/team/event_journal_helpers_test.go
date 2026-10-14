package team

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/kjelly/hufu/internal/runtimestore"
)

// withJournal runs fn on workspace's runtime.sqlite connection, the way
// another writer or a tamperer would reach the events table.
func withJournal(t *testing.T, workspace string, fn func(q runtimestore.Queryer)) {
	t.Helper()
	store, err := runtimestore.Open(context.Background(), workspace)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	fn(store.DB())
}

// execJournal runs one statement against workspace's events table.
func execJournal(t *testing.T, workspace, query string, args ...any) {
	t.Helper()
	withJournal(t, workspace, func(q runtimestore.Queryer) {
		if _, err := q.ExecContext(context.Background(), query, args...); err != nil {
			t.Fatal(err)
		}
	})
}

// readJournal returns every row of workspace's events table in journal order
// without validating them.
func readJournal(t *testing.T, workspace string) []RunEvent {
	t.Helper()
	var events []RunEvent
	withJournal(t, workspace, func(q runtimestore.Queryer) {
		rows, err := q.QueryContext(context.Background(), `SELECT `+eventColumns+` FROM events ORDER BY sequence`)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			_, event, err := scanEventRow(rows)
			if err != nil {
				t.Fatal(err)
			}
			events = append(events, event)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
	})
	return events
}

// appendRawChainedEvent appends event to workspace's journal the way another
// writer would, chained to the last row, bypassing any open store.
func appendRawChainedEvent(t *testing.T, workspace string, event RunEvent) {
	t.Helper()
	if err := insertRawChainedEvent(t, workspace, event); err != nil {
		t.Fatal(err)
	}
}

// insertRawChainedEvent is appendRawChainedEvent returning the insert error,
// for tests of what the journal's schema refuses.
func insertRawChainedEvent(t *testing.T, workspace string, event RunEvent) error {
	t.Helper()
	var insertErr error
	withJournal(t, workspace, func(q runtimestore.Queryer) {
		var sequence int
		var last RunEvent
		err := q.QueryRowContext(context.Background(), `SELECT sequence, id, hash FROM events ORDER BY sequence DESC LIMIT 1`).Scan(&sequence, &last.ID, &last.Hash)
		if err != nil {
			t.Fatal(err)
		}
		event.SchemaVersion = eventStoreSchemaVersion
		event.RunID, event.SessionID, event.Actor = "run-scan", "session-scan", "worker"
		event.Timestamp = "2026-09-26T00:00:00Z"
		if event.Payload == nil {
			event.Payload = []byte(`{"progress":"raw"}`)
		}
		event.PreviousID, event.PreviousHash = last.ID, last.Hash
		event.Hash = ComputeEventHash(event.PreviousHash, event.ID, event.Type, event.Timestamp, event.Payload)
		insertErr = insertEvent(context.Background(), q, sequence+1, event)
	})
	return insertErr
}

// insertJournalEvents writes events verbatim after the last row, bypassing
// validation, the way a tamperer or a broken writer would.
func insertJournalEvents(t *testing.T, workspace string, events ...RunEvent) {
	t.Helper()
	withJournal(t, workspace, func(q runtimestore.Queryer) {
		var sequence int
		if err := q.QueryRowContext(context.Background(), `SELECT COALESCE(MAX(sequence), 0) FROM events`).Scan(&sequence); err != nil {
			t.Fatal(err)
		}
		for _, event := range events {
			sequence++
			if err := insertEvent(context.Background(), q, sequence, event); err != nil {
				t.Fatal(err)
			}
		}
	})
}

// replaceRuntimeDatabase moves replacement's runtime.sqlite over workspace's,
// as a restore from elsewhere would.
func replaceRuntimeDatabase(t *testing.T, workspace, replacement string) {
	t.Helper()
	for _, name := range runtimestore.Files {
		if err := os.Remove(filepath.Join(workspace, name)); err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
	}
	if err := os.Rename(runtimestore.Path(replacement), runtimestore.Path(workspace)); err != nil {
		t.Fatal(err)
	}
}

// exportedJournal returns workspace's validated journal in the JSONL export
// format, the bytes the retired event_store.jsonl held.
func exportedJournal(t *testing.T, workspace string) []byte {
	t.Helper()
	var exported bytes.Buffer
	if err := ExportEventLog(context.Background(), workspace, &exported); err != nil {
		t.Fatal(err)
	}
	return exported.Bytes()
}

// tamperJournalPayload replaces the first occurrence of old in the payload of
// the first row containing it, leaving the stored hash stale, and reports
// whether a row changed.
func tamperJournalPayload(t *testing.T, workspace, old, replacement string) bool {
	t.Helper()
	for i, event := range readJournal(t, workspace) {
		if !bytes.Contains(event.Payload, []byte(old)) {
			continue
		}
		tampered := bytes.Replace(event.Payload, []byte(old), []byte(replacement), 1)
		execJournal(t, workspace, `UPDATE events SET payload = ? WHERE sequence = ?`, tampered, i+1)
		return true
	}
	return false
}
