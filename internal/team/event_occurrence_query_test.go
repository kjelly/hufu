package team

import (
	"context"
	"errors"
	"strings"
	"testing"

	"charm.land/fantasy"
)

func TestEventOccurrenceQueryRebuildsAndExtendsValidatedIndex(t *testing.T) {
	workspace := t.TempDir()
	store, err := NewEventStore(workspace, "indexed-run", "indexed-session")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	appendEvent := func(writer *EventStore, branch, task, kind string, attempt int) {
		t.Helper()
		if err := writer.Append(RunEvent{BranchID: branch, TaskID: task, Attempt: attempt, Type: kind, Payload: []byte(`{"value":"source"}`)}); err != nil {
			t.Fatal(err)
		}
	}
	appendEvent(store, "main", "1", "observation-test", 1)
	appendEvent(store, "sibling", "1", "observation-test", 1)
	appendEvent(store, "main", "2", "observation-test", 1)
	appendEvent(store, "main", "1", "observation-test", 2)
	query := EventQuery{RunID: "indexed-run", TaskID: "1", Attempt: 1, BranchID: "main", Types: []string{"observation-test", "observation-test"}}
	assertCount := func(want int) {
		t.Helper()
		got, err := store.QueryEvents(query)
		if err != nil || len(got) != want {
			t.Fatalf("query count = %d, want %d; %v", len(got), want, err)
		}
		for _, event := range got {
			if event.TaskID != "1" || event.Attempt != 1 || effectiveBranchID(event.BranchID) != "main" {
				t.Fatalf("scope widened: %+v", event)
			}
		}
		if want > 0 {
			got[0].Payload[0] = 'X'
		}
	}
	assertCount(1)
	assertCount(1) // Query results do not mutate the canonical cache.
	scans := store.scanCount
	appendEvent(store, "main", "1", "observation-test", 1)
	assertCount(2)
	if store.scanCount != scans {
		t.Fatal("append/query rescanned the journal")
	}
	// A different writer's validated tail is picked up on normal refresh.
	other, err := NewEventStore(workspace, "indexed-run", "indexed-session")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = other.Close() })
	appendEvent(other, "main", "1", "observation-test", 1)
	appendEvent(store, "main", "unrelated", "noise", 1)
	assertCount(3)
	store.commitHook = func() error { return errors.New("uncertain commit") }
	if err := store.Append(RunEvent{TaskID: "1", Attempt: 1, Type: "observation-test", Payload: []byte(`{}`)}); err == nil {
		t.Fatal("uncertain append succeeded")
	}
	if got, err := store.QueryEvents(query); err == nil || len(got) != 0 {
		t.Fatal("invalidated index remained readable")
	}
	appendEvent(store, "main", "unrelated", "noise", 1)
	assertCount(4) // The committed but unacknowledged row is rebuilt exactly once.
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = NewEventStore(workspace, "indexed-run", "indexed-session")
	if err != nil {
		t.Fatal(err)
	}
	assertCount(4)
}

type observationQueryJournal struct {
	eventStoreJournal
	queries int
}

func (j *observationQueryJournal) ReadEvents(context.Context) ([]RunEvent, error) {
	return nil, errors.New("capture must not read the complete journal")
}

func (j *observationQueryJournal) QueryEvents(ctx context.Context, query EventQuery) ([]RunEvent, error) {
	j.queries++
	return j.eventStoreJournal.QueryEvents(ctx, query)
}

func TestObservationCaptureUsesOccurrenceQueryAcrossRestart(t *testing.T) {
	c, ctx, item := observationFixture(t)
	journal := &observationQueryJournal{eventStoreJournal: eventStoreJournal{store: c.eventStore}}
	c.SetEventJournal(journal)
	for range 2 {
		c.captureToolObservation(ctx, fantasy.ToolCall{ID: "once"}, "view", `{}`, "immutable source")
	}
	query := EventQuery{RunID: c.executionRunID, TaskID: item.ID, Attempt: 1, Types: []string{string(EventToolObservationRecorded)}}
	got, err := c.eventStore.QueryEvents(query)
	if err != nil || len(got) != 1 || journal.queries != 2 {
		t.Fatalf("capture/query = %d/%d, %v", len(got), journal.queries, err)
	}
	if err := c.eventStore.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := NewEventStore(c.session.Workspace, c.executionRunID, "conversation")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	c.eventStore = reopened
	c.SetEventJournal(&observationQueryJournal{eventStoreJournal: eventStoreJournal{store: reopened}})
	c.captureToolObservation(ctx, fantasy.ToolCall{ID: "once"}, "view", `{}`, "immutable source")
	got, err = reopened.QueryEvents(query)
	if err != nil || len(got) != 1 {
		t.Fatalf("restart duplicated capture: %d, %v", len(got), err)
	}
}

func BenchmarkEventOccurrenceQueryLargeHistory(b *testing.B) {
	noise := []byte(strings.Repeat("x", 1024))
	events := make([]RunEvent, 10000)
	for i := range events {
		events[i] = RunEvent{RunID: "old", Type: "noise", Payload: noise}
	}
	events = append(events, RunEvent{RunID: "current", TaskID: "1", Attempt: 1, Type: "observation", Payload: []byte(`{}`)})
	store := &EventStore{mu: make(chan struct{}, 1), path: "benchmark", stateValid: true, cachedEvents: events}
	store.mu <- struct{}{}
	query := EventQuery{RunID: "current", TaskID: "1", Attempt: 1, Types: []string{"observation"}}
	if _, err := store.QueryEvents(query); err != nil {
		b.Fatal(err)
	}
	b.Run("complete_journal", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			if _, err := store.ReadEvents(); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("indexed_occurrence", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			got, err := store.QueryEvents(query)
			if err != nil || len(got) != 1 {
				b.Fatalf("query: %v, %d", err, len(got))
			}
		}
	})
}
