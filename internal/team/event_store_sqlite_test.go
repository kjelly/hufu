package team

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kjelly/hufu/internal/runtimestore"
)

// TestEventStoreAppendScansOnlyWhatOthersAppended pins the append-time
// refresh: the prefix a store already validated is not loaded again, while
// events another writer committed still join the chain head strictly.
func TestEventStoreAppendScansOnlyWhatOthersAppended(t *testing.T) {
	tests := []struct {
		name         string
		afterOwn     func(t *testing.T, workspace string)
		wantErr      string
		wantPrevious string
		wantScans    int
	}{
		{name: "own appends are not reloaded", wantPrevious: "second", wantScans: 1},
		{name: "another writer's event becomes the chain head", afterOwn: func(t *testing.T, workspace string) {
			other, err := NewEventStore(workspace, "run-scan", "session-scan")
			if err != nil {
				t.Fatal(err)
			}
			if err := other.Append(RunEvent{ID: "foreign", Type: "task_progress", Actor: "worker", Payload: []byte(`{"progress":"elsewhere"}`)}); err != nil {
				t.Fatal(err)
			}
			if err := other.Close(); err != nil {
				t.Fatal(err)
			}
		}, wantPrevious: "foreign", wantScans: 1},
		{name: "a corrupt committed tail fails closed", afterOwn: func(t *testing.T, workspace string) {
			appendRawChainedEvent(t, workspace, RunEvent{ID: "corrupt", Type: "task_progress"})
			execJournal(t, workspace, `UPDATE events SET hash = 'forged' WHERE id = 'corrupt'`)
		}, wantErr: "load events committed by other writers"},
		{name: "a head payload rewritten under its id fails closed", afterOwn: func(t *testing.T, workspace string) {
			execJournal(t, workspace, `UPDATE events SET payload = '{"progress":"forged"}' WHERE id = 'second'`)
		}, wantErr: "load events committed by other writers"},
		{name: "a rewrite deep in the validated prefix fails closed", afterOwn: func(t *testing.T, workspace string) {
			execJournal(t, workspace, `UPDATE events SET payload = '{"progress":"forged"}' WHERE id = 'first'`)
		}, wantErr: "load events committed by other writers"},
		{name: "a head rewrite that bypasses the rewrite counter fails closed", afterOwn: func(t *testing.T, workspace string) {
			execJournal(t, workspace, `DROP TRIGGER events_rewritten_by_update`)
			execJournal(t, workspace, `UPDATE events SET payload = '{"progress":"forged"}' WHERE id = 'second'`)
		}, wantErr: "load events committed by other writers"},
		{name: "a journal truncated below the head is reloaded in full", afterOwn: func(t *testing.T, workspace string) {
			execJournal(t, workspace, `DELETE FROM events WHERE sequence > 1`)
		}, wantPrevious: "first", wantScans: 2},
		{name: "a rewritten head row is reloaded in full", afterOwn: func(t *testing.T, workspace string) {
			execJournal(t, workspace, `DELETE FROM events WHERE sequence > 1`)
			appendRawChainedEvent(t, workspace, RunEvent{ID: "rewritten", Type: "task_progress"})
		}, wantPrevious: "rewritten", wantScans: 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			workspace := t.TempDir()
			store, err := NewEventStore(workspace, "run-scan", "session-scan")
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = store.Close() }()
			for _, id := range []string{"first", "second"} {
				if err := store.Append(RunEvent{ID: id, Type: "task_progress", Actor: "worker", Payload: []byte(`{"progress":"` + id + `"}`)}); err != nil {
					t.Fatal(err)
				}
			}
			if tt.afterOwn != nil {
				tt.afterOwn(t, workspace)
			}
			before := len(readJournal(t, workspace))
			next, err := store.AppendPersisted(RunEvent{ID: "next", Type: "task_progress", Actor: "worker", Payload: []byte(`{"progress":"next"}`)})
			if tt.wantErr != "" {
				after := len(readJournal(t, workspace))
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) || after != before {
					t.Fatalf("append error = %v, journal %d -> %d rows; want %q and no write", err, before, after, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if next.PreviousID != tt.wantPrevious || store.scanCount != tt.wantScans {
				t.Fatalf("append previous_id = %q after %d full loads, want %q after %d", next.PreviousID, store.scanCount, tt.wantPrevious, tt.wantScans)
			}
			if err := store.VerifyHashChain(); err != nil {
				t.Fatalf("appended chain does not verify: %v", err)
			}
		})
	}
}

// TestEventStoreAppendSeesAnotherWritersIdempotencyKey keeps the reason the
// append-time refresh exists: an operation another writer already recorded
// is answered from its durable event instead of being written twice.
func TestEventStoreAppendSeesAnotherWritersIdempotencyKey(t *testing.T) {
	workspace := t.TempDir()
	store, err := NewEventStore(workspace, "run-scan", "session-scan")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	if err := store.Append(RunEvent{ID: "first", Type: "task_progress", Actor: "worker", Payload: []byte(`{"progress":"first"}`)}); err != nil {
		t.Fatal(err)
	}
	other, err := NewEventStore(workspace, "run-scan", "session-scan")
	if err != nil {
		t.Fatal(err)
	}
	if err := other.Append(RunEvent{ID: "foreign", Type: "task_progress", Actor: "worker", IdempotencyKey: "shared", Payload: []byte(`{"progress":"elsewhere"}`)}); err != nil {
		t.Fatal(err)
	}
	if err := other.Close(); err != nil {
		t.Fatal(err)
	}
	durable, err := store.AppendPersisted(RunEvent{ID: "retry", Type: "task_progress", Actor: "worker", IdempotencyKey: "shared", Payload: []byte(`{"progress":"elsewhere"}`)})
	if err != nil {
		t.Fatal(err)
	}
	events, err := store.ReadEvents()
	if err != nil {
		t.Fatal(err)
	}
	if durable.ID != "foreign" || len(events) != 2 || events[1].ID != "foreign" {
		t.Fatalf("append returned %q with cache %d events; want the other writer's durable event and no second write", durable.ID, len(events))
	}
}

// TestEventStoreAppendMergesAnotherWritersTail pins that another writer's
// committed event for a key answers a retry instead of a second write.
func TestEventStoreAppendMergesAnotherWritersTail(t *testing.T) {
	workspace := t.TempDir()
	store, err := NewEventStore(workspace, "run-scan", "session-scan")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	if err := store.Append(RunEvent{ID: "own", Type: "task_progress", Actor: "worker", IdempotencyKey: "own-key", Payload: []byte(`{"progress":"own"}`)}); err != nil {
		t.Fatal(err)
	}
	appendRawChainedEvent(t, workspace, RunEvent{ID: "tail", Type: "task_progress", IdempotencyKey: "tail-key"})
	before := len(readJournal(t, workspace))
	durable, err := store.AppendPersisted(RunEvent{ID: "next", Type: "task_progress", Actor: "worker", IdempotencyKey: "tail-key", Payload: []byte(`{"progress":"next"}`)})
	if err != nil {
		t.Fatal(err)
	}
	if after := len(readJournal(t, workspace)); durable.ID != "tail" || after != before {
		t.Fatalf("retry returned %q, journal %d -> %d rows; want %q and no write", durable.ID, before, after, "tail")
	}
}

// TestEventJournalKeepsOneEventPerIdempotencyIdentity pins the schema half of
// idempotency: no writer can store a second event for a branch and key, with
// the empty branch meaning main, while other branches stay independent.
func TestEventJournalKeepsOneEventPerIdempotencyIdentity(t *testing.T) {
	tests := []struct {
		name    string
		branch  string
		wantErr bool
	}{
		{name: "same branch", branch: "main", wantErr: true},
		{name: "empty branch is main", branch: "", wantErr: true},
		{name: "another branch", branch: "feature"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			workspace := t.TempDir()
			store, err := NewEventStore(workspace, "run-scan", "session-scan")
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = store.Close() }()
			if err := store.Append(RunEvent{ID: "own", Type: "task_progress", Actor: "worker", BranchID: "main", IdempotencyKey: "key", Payload: []byte(`{"progress":"own"}`)}); err != nil {
				t.Fatal(err)
			}
			var insertErr error
			withJournal(t, workspace, func(q runtimestore.Queryer) {
				insertErr = insertEvent(context.Background(), q, 2, RunEvent{ID: "dup", Type: "task_progress", BranchID: tt.branch, IdempotencyKey: "key", Hash: "h"})
			})
			if (insertErr != nil) != tt.wantErr {
				t.Fatalf("insert error = %v, want error %v", insertErr, tt.wantErr)
			}
		})
	}
}

// TestEventStoreAppendRefusesReplacedDatabase keeps appends off a database
// file the path no longer names: an open connection would keep writing to it
// and no later reader would see the events. The refused append invalidates
// the store, so the next one reopens the path.
func TestEventStoreAppendRefusesReplacedDatabase(t *testing.T) {
	removeDatabase := func(t *testing.T, workspace string) {
		for _, name := range runtimestore.Files {
			if err := os.Remove(filepath.Join(workspace, name)); err != nil && !os.IsNotExist(err) {
				t.Fatal(err)
			}
		}
	}
	tests := []struct {
		name         string
		replace      func(t *testing.T, workspace string)
		wantPrevious string // empty: the database is gone and nothing is retried
	}{
		{name: "replaced by another journal", replace: func(t *testing.T, workspace string) {
			other := t.TempDir()
			replacement, err := NewEventStore(other, "run-scan", "session-scan")
			if err != nil {
				t.Fatal(err)
			}
			if err := replacement.Append(RunEvent{ID: "replacement", Type: "task_progress", Actor: "worker", Payload: []byte(`{"progress":"replacement"}`)}); err != nil {
				t.Fatal(err)
			}
			if err := replacement.Close(); err != nil {
				t.Fatal(err)
			}
			removeDatabase(t, workspace)
			if err := os.Rename(runtimestore.Path(other), runtimestore.Path(workspace)); err != nil {
				t.Fatal(err)
			}
		}, wantPrevious: "replacement"},
		{name: "removed", replace: removeDatabase},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			workspace := t.TempDir()
			store, err := NewEventStore(workspace, "run-scan", "session-scan")
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = store.Close() }()
			if err := store.Append(RunEvent{ID: "first", Type: "task_progress", Actor: "worker", Payload: []byte(`{"progress":"first"}`)}); err != nil {
				t.Fatal(err)
			}
			tt.replace(t, workspace)

			_, err = store.AppendPersisted(RunEvent{ID: "refused", Type: "task_progress", Actor: "worker", Payload: []byte(`{"progress":"refused"}`)})
			if !errors.Is(err, runtimestore.ErrReplaced) {
				t.Fatalf("append error = %v, want %v", err, runtimestore.ErrReplaced)
			}
			if tt.wantPrevious == "" {
				if runtimestore.Exists(workspace) {
					t.Fatal("a refused append recreated the removed database")
				}
				return
			}

			retried, err := store.AppendPersisted(RunEvent{ID: "retried", Type: "task_progress", Actor: "worker", Payload: []byte(`{"progress":"retried"}`)})
			if err != nil {
				t.Fatal(err)
			}
			if retried.PreviousID != tt.wantPrevious {
				t.Fatalf("retried append previous_id = %q, want %q", retried.PreviousID, tt.wantPrevious)
			}
			events, err := readValidatedEventJournal(context.Background(), workspace)
			if err != nil {
				t.Fatal(err)
			}
			if len(events) != 2 || events[1].ID != "retried" {
				t.Fatalf("journal at the path = %d events, want the replacement then the retried event", len(events))
			}
		})
	}
}

// TestEventStoreConcurrentProcessesKeepOneChain runs independent processes
// appending to one workspace at once. Every event lands exactly once on one
// contiguous chain, and a key both processes record is written once.
func TestEventStoreConcurrentProcessesKeepOneChain(t *testing.T) {
	const perProcess, shared = 40, 10
	if name := os.Getenv("HUFU_EVENT_STORE_WRITER"); name != "" {
		store, err := NewEventStore(os.Getenv("HUFU_EVENT_STORE_WORKSPACE"), "run-concurrent", "session-concurrent")
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = store.Close() }()
		for i := range perProcess {
			key := fmt.Sprintf("%s-%d", name, i)
			if i < shared {
				key = fmt.Sprintf("shared-%d", i)
			}
			if err := store.Append(RunEvent{Type: "task_progress", Actor: name, IdempotencyKey: key, Payload: []byte(`{"progress":"concurrent"}`)}); err != nil {
				t.Fatal(err)
			}
		}
		return
	}
	workspace := t.TempDir()
	writers := []string{"writer-a", "writer-b"}
	commands := make([]*exec.Cmd, len(writers))
	outputs := make([]bytes.Buffer, len(writers))
	for i, name := range writers {
		cmd := exec.Command(os.Args[0], "-test.run=^TestEventStoreConcurrentProcessesKeepOneChain$")
		cmd.Env = append(os.Environ(), "HUFU_EVENT_STORE_WRITER="+name, "HUFU_EVENT_STORE_WORKSPACE="+workspace)
		cmd.Stdout, cmd.Stderr = &outputs[i], &outputs[i]
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		commands[i] = cmd
	}
	for i, cmd := range commands {
		if err := cmd.Wait(); err != nil {
			t.Fatalf("%s: %v\n%s", writers[i], err, outputs[i].String())
		}
	}
	events, err := readValidatedEventJournal(context.Background(), workspace)
	if err != nil {
		t.Fatal(err)
	}
	if want := len(writers)*perProcess - shared; len(events) != want {
		t.Fatalf("journal has %d events, want %d", len(events), want)
	}
	keys := make(map[string]bool, len(events))
	for _, event := range events {
		if keys[event.IdempotencyKey] {
			t.Fatalf("key %q was written twice", event.IdempotencyKey)
		}
		keys[event.IdempotencyKey] = true
	}
}
