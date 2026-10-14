package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/kjelly/hufu/internal/runtimestore"
	"github.com/kjelly/hufu/internal/team"
)

// writeEventJournal records events in workspace's event journal through the
// event store, the way a run would. Events without an actor are recorded as
// the coordinator's.
func writeEventJournal(t *testing.T, workspace string, events ...team.RunEvent) {
	t.Helper()
	store, err := team.NewEventStore(workspace, "", "session-fixture")
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event.Actor == "" {
			event.Actor = "coordinator"
		}
		if err := store.Append(event); err != nil {
			_ = store.Close()
			t.Fatal(err)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
}

// exportedEventLog returns workspace's validated event journal as JSONL, the
// form the retired logs/event_store.jsonl had.
func exportedEventLog(t *testing.T, workspace string) string {
	t.Helper()
	var log bytes.Buffer
	if err := team.ExportEventLog(context.Background(), workspace, &log); err != nil {
		t.Fatal(err)
	}
	return log.String()
}

// tamperEventJournal runs one statement against workspace's events table, the
// way a tamperer would edit the journal behind the event store.
func tamperEventJournal(t *testing.T, workspace, query string, args ...any) {
	t.Helper()
	ctx := context.Background()
	store, err := runtimestore.Open(ctx, workspace)
	if err != nil {
		t.Fatal(err)
	}
	result, err := store.DB().ExecContext(ctx, query, args...)
	if closeErr := store.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		t.Fatal(err)
	}
	if changed, err := result.RowsAffected(); err != nil || changed == 0 {
		t.Fatalf("tampering changed %d events (err %v), want at least one", changed, err)
	}
}

// runtimeStoreFiles returns the bytes of every runtime.sqlite file present in
// workspace, keyed by name, so a test can prove a reader changed, created, or
// removed none of them.
func runtimeStoreFiles(t *testing.T, workspace string) map[string][]byte {
	t.Helper()
	files := make(map[string][]byte)
	for _, name := range runtimestore.Files {
		data, err := os.ReadFile(filepath.Join(workspace, name))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		files[name] = data
	}
	return files
}
