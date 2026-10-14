package team

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
)

func appendExportFixture(t *testing.T, workspace string) {
	t.Helper()
	store, err := NewEventStore(workspace, "run-export", "session-export")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	for _, event := range []RunEvent{
		{Type: "run_started", Actor: "user", IdempotencyKey: "run:started"},
		{Type: "task_progress", Actor: "worker", TaskID: "t1", Attempt: 1, BranchID: "feature", Payload: []byte(`{"progress": "spaced  payload"}`)},
		{Type: "run_finished", Actor: "coordinator", Payload: []byte(`{"outcome":"success"}`)},
	} {
		if err := store.Append(event); err != nil {
			t.Fatal(err)
		}
	}
}

// TestExportImportEventLogRoundTripsTheChain pins the portable format: an
// imported export is the same journal, so exporting it again yields the same
// bytes and the chain still validates.
func TestExportImportEventLogRoundTripsTheChain(t *testing.T) {
	source := t.TempDir()
	appendExportFixture(t, source)
	var exported bytes.Buffer
	if err := ExportEventLog(context.Background(), source, &exported); err != nil {
		t.Fatal(err)
	}
	if lines := strings.Count(exported.String(), "\n"); lines != 3 {
		t.Fatalf("exported %d lines, want 3", lines)
	}

	target := t.TempDir()
	imported, err := ImportEventLog(context.Background(), target, bytes.NewReader(exported.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if imported != 3 {
		t.Fatalf("imported %d events, want 3", imported)
	}
	var again bytes.Buffer
	if err := ExportEventLog(context.Background(), target, &again); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(again.Bytes(), exported.Bytes()) {
		t.Fatalf("re-export differs:\n%s\nwant\n%s", again.String(), exported.String())
	}
	store, err := OpenEventStore(target)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	if err := store.VerifyHashChain(); err != nil {
		t.Fatal(err)
	}
	next, err := store.AppendPersisted(RunEvent{Type: "task_progress", Actor: "worker", Payload: []byte(`{"progress":"after import"}`)})
	if err != nil {
		t.Fatal(err)
	}
	if next.PreviousHash == "" || next.SessionID != "session-export" {
		t.Fatalf("append after import = %+v, want it chained onto the imported head", next)
	}
}

func TestImportEventLogRejectsWhatItCannotKeepIntact(t *testing.T) {
	source := t.TempDir()
	appendExportFixture(t, source)
	var exported bytes.Buffer
	if err := ExportEventLog(context.Background(), source, &exported); err != nil {
		t.Fatal(err)
	}
	valid := exported.String()
	tests := []struct {
		name        string
		log         string
		populated   bool
		wantInvalid bool
	}{
		{name: "tampered event", log: strings.Replace(valid, `"run_started"`, `"run_hacked"`, 1), wantInvalid: true},
		{name: "dropped event", log: valid[strings.Index(valid, "\n")+1:], wantInvalid: true},
		{name: "malformed line", log: "not json\n"},
		{name: "journal already has events", log: valid, populated: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			target := t.TempDir()
			if tt.populated {
				appendExportFixture(t, target)
			}
			before := len(readJournal(t, target))
			_, err := ImportEventLog(context.Background(), target, strings.NewReader(tt.log))
			if err == nil {
				t.Fatal("import accepted the log")
			}
			if errors.Is(err, ErrEventLogInvalid) != tt.wantInvalid {
				t.Fatalf("import error = %v, want ErrEventLogInvalid %v", err, tt.wantInvalid)
			}
			if after := len(readJournal(t, target)); after != before {
				t.Fatalf("failed import changed the journal: %d -> %d rows", before, after)
			}
		})
	}
}

func TestReadUnvalidatedRunEventsFindsRunsInADamagedJournal(t *testing.T) {
	workspace := t.TempDir()
	appendExportFixture(t, workspace)
	insertJournalEvents(t, workspace, RunEvent{ID: "unscoped", Type: "legacy", Hash: "h"})
	execJournal(t, workspace, `UPDATE events SET hash = 'forged' WHERE type = 'task_progress'`)

	events, unscoped, err := ReadUnvalidatedRunEvents(context.Background(), workspace, "run-export")
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 3 || unscoped != 1 {
		t.Fatalf("run events = %d, unscoped = %d; want 3 and 1", len(events), unscoped)
	}
	if _, _, err := ReadUnvalidatedRunEvents(context.Background(), t.TempDir(), "run-export"); err == nil {
		t.Fatal("a workspace without a journal returned no error")
	}
}
