package team

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestEventStoreAppendScansOnlyWhatOthersAppended pins the append-time
// refresh: the prefix a store already validated is not scanned again, while
// events another writer appended still join the chain head strictly.
func TestEventStoreAppendScansOnlyWhatOthersAppended(t *testing.T) {
	tests := []struct {
		name         string
		afterOwn     func(t *testing.T, workspace, path string)
		wantErr      string
		wantPrevious string
		wantScans    int
	}{
		{name: "own appends are not rescanned", wantPrevious: "second", wantScans: 1},
		{name: "another writer's event becomes the chain head", afterOwn: func(t *testing.T, workspace, _ string) {
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
		{name: "a corrupt appended tail fails closed", afterOwn: func(t *testing.T, _, path string) {
			f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.WriteString("not json\n"); err != nil {
				t.Fatal(err)
			}
			if err := f.Close(); err != nil {
				t.Fatal(err)
			}
		}, wantErr: "rescan event store before append"},
		{name: "a log rewritten shorter is scanned in full", afterOwn: func(t *testing.T, _, path string) {
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			firstLine, _, _ := strings.Cut(string(data), "\n")
			if err := os.WriteFile(path, []byte(firstLine+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}, wantPrevious: "first", wantScans: 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			workspace := t.TempDir()
			path := filepath.Join(workspace, logsDir, eventStoreFile)
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
				tt.afterOwn(t, workspace, path)
			}
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			next, err := store.AppendPersisted(RunEvent{ID: "next", Type: "task_progress", Actor: "worker", Payload: []byte(`{"progress":"next"}`)})
			if tt.wantErr != "" {
				after, readErr := os.ReadFile(path)
				if readErr != nil {
					t.Fatal(readErr)
				}
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) || string(after) != string(before) {
					t.Fatalf("append error = %v, log grew %d -> %d bytes; want %q and no write", err, len(before), len(after), tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if next.PreviousID != tt.wantPrevious || store.scanCount != tt.wantScans {
				t.Fatalf("append previous_id = %q after %d full scans, want %q after %d", next.PreviousID, store.scanCount, tt.wantPrevious, tt.wantScans)
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

// appendRawChainedEvent appends event to the log at path the way another
// writer would, chained to the log's last line, bypassing any open store.
func appendRawChainedEvent(t *testing.T, path string, event RunEvent) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	var last RunEvent
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &last); err != nil {
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
	line, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if _, err := f.Write(append(line, '\n')); err != nil {
		t.Fatal(err)
	}
}

// TestEventStoreAppendMergesAnotherWritersTail pins the idempotency semantics
// of merging a tail scanned apart from the published index: conflicts are
// found across the prefix and inside the tail, and the latest event for a key
// answers a retry.
func TestEventStoreAppendMergesAnotherWritersTail(t *testing.T) {
	tests := []struct {
		name          string
		tail          []RunEvent
		retryKey      string
		wantErr       error
		wantDurableID string
	}{
		{
			name:    "a tail decision event conflicting with a published key fails closed",
			tail:    []RunEvent{{ID: "tail", Type: string(EventDecisionRunOpened), IdempotencyKey: "own-key"}},
			wantErr: ErrDecisionIdempotencyConflict,
		},
		{
			name: "a decision event conflicting inside the tail fails closed",
			tail: []RunEvent{
				{ID: "tail-1", Type: "task_progress", IdempotencyKey: "tail-key"},
				{ID: "tail-2", Type: string(EventDecisionRunOpened), IdempotencyKey: "tail-key"},
			},
			wantErr: ErrDecisionIdempotencyConflict,
		},
		{
			name:          "the tail's later event answers a key it repeats",
			tail:          []RunEvent{{ID: "tail", Type: "task_progress", IdempotencyKey: "own-key"}},
			retryKey:      "own-key",
			wantDurableID: "tail",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			workspace := t.TempDir()
			path := filepath.Join(workspace, logsDir, eventStoreFile)
			store, err := NewEventStore(workspace, "run-scan", "session-scan")
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = store.Close() }()
			if err := store.Append(RunEvent{ID: "own", Type: "task_progress", Actor: "worker", IdempotencyKey: "own-key", Payload: []byte(`{"progress":"own"}`)}); err != nil {
				t.Fatal(err)
			}
			for _, event := range tt.tail {
				appendRawChainedEvent(t, path, event)
			}
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			durable, err := store.AppendPersisted(RunEvent{ID: "next", Type: "task_progress", Actor: "worker", IdempotencyKey: tt.retryKey, Payload: []byte(`{"progress":"next"}`)})
			after, readErr := os.ReadFile(path)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) || string(after) != string(before) {
					t.Fatalf("append error = %v, log grew %d -> %d bytes; want %v and no write", err, len(before), len(after), tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if durable.ID != tt.wantDurableID || string(after) != string(before) {
				t.Fatalf("retry returned %q, log grew %d -> %d bytes; want %q and no write", durable.ID, len(before), len(after), tt.wantDurableID)
			}
		})
	}
}

// TestEventStoreAppendRefusesReplacedLog keeps appends off a file the log path
// no longer names: such events would never reach anyone who opens the path.
// The refused append invalidates the store, so the next one reopens the path.
func TestEventStoreAppendRefusesReplacedLog(t *testing.T) {
	replaceWith := func(t *testing.T, path string, rewrite func(tmp string)) {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		tmp := path + ".rewrite"
		if err := os.WriteFile(tmp, data, 0o644); err != nil {
			t.Fatal(err)
		}
		if rewrite != nil {
			rewrite(tmp)
		}
		if err := os.Rename(tmp, path); err != nil {
			t.Fatal(err)
		}
	}
	tests := []struct {
		name         string
		replace      func(t *testing.T, path string)
		wantPrevious string // empty: the log is gone and nothing is retried
	}{
		{name: "replaced with identical content", replace: func(t *testing.T, path string) {
			replaceWith(t, path, nil)
		}, wantPrevious: "first"},
		{name: "replaced by a rewrite that added an event", replace: func(t *testing.T, path string) {
			replaceWith(t, path, func(tmp string) {
				appendRawChainedEvent(t, tmp, RunEvent{ID: "rewritten", Type: "task_progress"})
			})
		}, wantPrevious: "rewritten"},
		{name: "removed", replace: func(t *testing.T, path string) {
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			workspace := t.TempDir()
			path := filepath.Join(workspace, logsDir, eventStoreFile)
			store, err := NewEventStore(workspace, "run-scan", "session-scan")
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = store.Close() }()
			if err := store.Append(RunEvent{ID: "first", Type: "task_progress", Actor: "worker", Payload: []byte(`{"progress":"first"}`)}); err != nil {
				t.Fatal(err)
			}
			superseded, err := os.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = superseded.Close() }()
			supersededInfo, err := superseded.Stat()
			if err != nil {
				t.Fatal(err)
			}
			tt.replace(t, path)
			before, _ := os.ReadFile(path)

			_, err = store.AppendPersisted(RunEvent{ID: "refused", Type: "task_progress", Actor: "worker", Payload: []byte(`{"progress":"refused"}`)})
			after, _ := os.ReadFile(path)
			nowInfo, statErr := superseded.Stat()
			if statErr != nil {
				t.Fatal(statErr)
			}
			if !errors.Is(err, errEventStoreReplaced) || string(after) != string(before) || nowInfo.Size() != supersededInfo.Size() {
				t.Fatalf("append error = %v, path log %d -> %d bytes, superseded file %d -> %d bytes; want %v and no write", err, len(before), len(after), supersededInfo.Size(), nowInfo.Size(), errEventStoreReplaced)
			}
			if tt.wantPrevious == "" {
				return
			}

			retried, err := store.AppendPersisted(RunEvent{ID: "retried", Type: "task_progress", Actor: "worker", Payload: []byte(`{"progress":"retried"}`)})
			if err != nil {
				t.Fatal(err)
			}
			if retried.PreviousID != tt.wantPrevious {
				t.Fatalf("retried append previous_id = %q, want %q", retried.PreviousID, tt.wantPrevious)
			}
			reopened, err := OpenEventStore(workspace)
			if err != nil {
				t.Fatalf("reopen replaced log: %v", err)
			}
			defer func() { _ = reopened.Close() }()
			events, err := reopened.ReadEvents()
			if err != nil {
				t.Fatal(err)
			}
			if last := events[len(events)-1]; last.ID != "retried" {
				t.Fatalf("path log ends with %q, want the retried event", last.ID)
			}
		})
	}
}
