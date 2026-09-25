package team

import (
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
