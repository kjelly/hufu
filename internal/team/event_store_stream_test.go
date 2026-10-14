package team

import (
	"bytes"
	"os"
	"slices"
	"testing"

	"github.com/kjelly/hufu/internal/runtimestore"
)

func workspaceEntries(t *testing.T, workspace string) []string {
	t.Helper()
	entries, err := os.ReadDir(workspace)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}

func TestStreamValidatedRunEventsMissingStoreDoesNotCreateIt(t *testing.T) {
	workspace := t.TempDir()
	var count int
	if err := StreamValidatedRunEvents(t.Context(), workspace, func(RunEvent) error {
		count++
		return nil
	}); err != nil {
		t.Fatalf("stream missing event store: %v", err)
	}
	if count != 0 {
		t.Fatalf("events = %d, want zero", count)
	}
	if names := workspaceEntries(t, workspace); len(names) != 0 {
		t.Fatalf("stream created %v", names)
	}
}

func TestStreamValidatedRunEventsPreservesCanonicalStore(t *testing.T) {
	workspace := t.TempDir()
	store, err := NewEventStore(workspace, "run-stream", "session-stream")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Append(RunEvent{ID: "event-1", Type: "memory_retrieved", Actor: "runtime", Payload: []byte(`{"retrieval_id":"r1"}`)}); err != nil {
		t.Fatal(err)
	}
	if err := store.Append(RunEvent{ID: "event-2", Type: "memory_usage_recorded", Actor: "runtime", Payload: []byte(`{"disposition":"applied"}`)}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	beforeNames := workspaceEntries(t, workspace)
	before, err := os.ReadFile(runtimestore.Path(workspace))
	if err != nil {
		t.Fatal(err)
	}
	var got []RunEvent
	if err := StreamValidatedRunEvents(t.Context(), workspace, func(event RunEvent) error {
		got = append(got, event)
		return nil
	}); err != nil {
		t.Fatalf("stream valid event store: %v", err)
	}
	after, err := os.ReadFile(runtimestore.Path(workspace))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, before) || !slices.Equal(workspaceEntries(t, workspace), beforeNames) {
		t.Fatal("stream changed the canonical event store")
	}
	if len(got) != 2 || got[0].ID != "event-1" || got[1].PreviousHash != got[0].Hash {
		t.Fatalf("streamed events = %+v, want validated chain in journal order", got)
	}
}

func TestStreamValidatedRunEventsReportsLateFailureAfterPrefix(t *testing.T) {
	tests := []struct {
		name   string
		tamper string
	}{
		{name: "hash", tamper: `UPDATE events SET type = 'memory_usage_corrupt' WHERE id = 'event-2'`},
		{name: "gap", tamper: `UPDATE events SET sequence = 5 WHERE id = 'event-2'`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			workspace := t.TempDir()
			store, err := NewEventStore(workspace, "run-stream-corrupt", "session-stream-corrupt")
			if err != nil {
				t.Fatal(err)
			}
			if err := store.Append(RunEvent{ID: "event-1", Type: "memory_retrieved", Actor: "runtime", Payload: []byte(`{"retrieval_id":"r1"}`)}); err != nil {
				t.Fatal(err)
			}
			if err := store.Append(RunEvent{ID: "event-2", Type: "memory_usage_recorded", Actor: "runtime", Payload: []byte(`{"disposition":"applied"}`)}); err != nil {
				t.Fatal(err)
			}
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			execJournal(t, workspace, tt.tamper)

			var streamed int
			err = StreamValidatedRunEvents(t.Context(), workspace, func(RunEvent) error {
				streamed++
				return nil
			})
			if err == nil {
				t.Fatal("stream accepted a broken journal")
			}
			if streamed != 1 {
				t.Fatalf("streamed prefix events = %d, want one before late validation failure", streamed)
			}
		})
	}
}
