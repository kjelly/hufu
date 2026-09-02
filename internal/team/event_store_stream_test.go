package team

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestStreamValidatedRunEventsMissingFileDoesNotCreateStore(t *testing.T) {
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
	if _, err := os.Stat(filepath.Join(workspace, logsDir, eventStoreFile)); !os.IsNotExist(err) {
		t.Fatalf("stream created event store: stat err = %v", err)
	}
}

func TestStreamValidatedRunEventsPreservesCanonicalFile(t *testing.T) {
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

	path := filepath.Join(workspace, logsDir, eventStoreFile)
	before, err := os.ReadFile(path)
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
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, before) {
		t.Fatal("stream changed canonical event store")
	}
	if len(got) != 2 || got[0].ID != "event-1" || got[1].PreviousHash != got[0].Hash {
		t.Fatalf("streamed events = %+v, want validated chain in file order", got)
	}
}

func TestStreamValidatedRunEventsReportsLateHashFailureAfterPrefix(t *testing.T) {
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

	path := filepath.Join(workspace, logsDir, eventStoreFile)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	data = bytes.Replace(data, []byte("memory_usage_recorded"), []byte("memory_usage_corrupt"), 1)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}

	var streamed int
	err = StreamValidatedRunEvents(t.Context(), workspace, func(RunEvent) error {
		streamed++
		return nil
	})
	if err == nil {
		t.Fatal("stream accepted a corrupted hash chain")
	}
	if streamed != 1 {
		t.Fatalf("streamed prefix events = %d, want one before late validation failure", streamed)
	}
}
