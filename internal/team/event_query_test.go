package team

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestEventStoreQueryEventsMatchesRunAndTypesInDurableOrder(t *testing.T) {
	store, err := NewEventStore(t.TempDir(), "run-default", "session-query")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()

	fixtures := []RunEvent{
		{RunID: "run-a", Type: "noise", Actor: "test", Payload: []byte(`{"n":1}`)},
		{RunID: "run-b", Type: "wanted", Actor: "test", Payload: []byte(`{"n":2}`)},
		{RunID: "run-a", Type: "wanted", Actor: "test", Payload: []byte(`{"n":3}`)},
		{RunID: "run-a", Type: "other", Actor: "test", Payload: []byte(`{"n":4}`)},
	}
	for _, fixture := range fixtures {
		if err := store.Append(fixture); err != nil {
			t.Fatalf("append %q/%q: %v", fixture.RunID, fixture.Type, err)
		}
	}

	got, err := store.QueryEvents(EventQuery{RunID: "run-a", Types: []string{"other", "wanted", "wanted"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("matching event count = %d, want 2", len(got))
	}
	if got[0].RunID != "run-a" || got[0].Type != "wanted" || string(got[0].Payload) != `{"n":3}` {
		t.Fatalf("first matching event = %#v, want run-a/wanted/{n:3}", got[0])
	}
	if got[1].RunID != "run-a" || got[1].Type != "other" || string(got[1].Payload) != `{"n":4}` {
		t.Fatalf("second matching event = %#v, want run-a/other/{n:4}", got[1])
	}

	all, err := store.QueryEvents(EventQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != len(fixtures) {
		t.Fatalf("wildcard query count = %d, want %d", len(all), len(fixtures))
	}
	for index, event := range all {
		if event.Type != fixtures[index].Type || event.RunID != fixtures[index].RunID {
			t.Fatalf("wildcard event %d = %#v, want fixture %#v", index, event, fixtures[index])
		}
	}
}

func TestEventStoreQueryEventsReturnsDefensiveCopiesWithoutRescan(t *testing.T) {
	store, err := NewEventStore(t.TempDir(), "run-query-copy", "session-query-copy")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	if err := store.Append(RunEvent{Type: "query_copy", Actor: "test", Payload: []byte(`{"status":"original"}`)}); err != nil {
		t.Fatal(err)
	}

	scanCount := store.scanCount
	cacheHits := store.cacheHitCount
	got, err := store.QueryEvents(EventQuery{Types: []string{"query_copy"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("query result count = %d, want 1", len(got))
	}
	got[0].Payload[0] = 'X'
	if store.scanCount != scanCount {
		t.Fatalf("query triggered a rescan: %d -> %d", scanCount, store.scanCount)
	}
	if store.cacheHitCount != cacheHits+1 {
		t.Fatalf("cache hits = %d, want %d", store.cacheHitCount, cacheHits+1)
	}

	unchanged, err := store.QueryEvents(EventQuery{Types: []string{"query_copy"}})
	if err != nil {
		t.Fatal(err)
	}
	if gotPayload := string(unchanged[0].Payload); gotPayload != `{"status":"original"}` {
		t.Fatalf("cached payload = %s, want original payload", gotPayload)
	}
}

func TestEventStoreQueryEventsEmptyAndUnknownResults(t *testing.T) {
	store, err := NewEventStore(t.TempDir(), "run-query-empty", "session-query-empty")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	if err := store.Append(RunEvent{Type: "known", Actor: "test"}); err != nil {
		t.Fatal(err)
	}

	got, err := store.QueryEvents(EventQuery{Types: []string{"unknown"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("unknown type result = %#v, want empty", got)
	}

	got, err = store.QueryEvents(EventQuery{RunID: "other-run"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("unknown run result = %#v, want empty", got)
	}

	zeroPath := &EventStore{}
	got, err = zeroPath.QueryEvents(EventQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Fatalf("zero-path result = %#v, want nil", got)
	}
}

func TestEventStoreQueryEventsFailsClosedWithInvalidState(t *testing.T) {
	store, err := NewEventStore(t.TempDir(), "run-query-invalid", "session-query-invalid")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	if err := store.Append(RunEvent{Type: "stale", Actor: "test"}); err != nil {
		t.Fatal(err)
	}

	stateErr := errors.New("injected query state failure")
	store.stateValid = false
	store.stateErr = stateErr
	got, err := store.QueryEvents(EventQuery{Types: []string{"stale"}})
	if got != nil {
		t.Fatalf("invalid-state result = %#v, want nil", got)
	}
	if err == nil || !strings.Contains(err.Error(), "event store state invalid") || !errors.Is(err, stateErr) {
		t.Fatalf("invalid-state error = %v, want wrapped state error", err)
	}

	store.stateErr = nil
	_, err = store.QueryEvents(EventQuery{})
	if err == nil || err.Error() != "event store state invalid" {
		t.Fatalf("invalid-state error without cause = %v, want exact state error", err)
	}
}

func TestEventStoreQueryEventsWildcardMatchesReadEvents(t *testing.T) {
	store, err := NewEventStore(t.TempDir(), "run-query-equal", "session-query-equal")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	for _, event := range []RunEvent{
		{Type: "first", Actor: "test", Payload: []byte(`{"n":1}`)},
		{Type: "second", Actor: "test", Payload: []byte(`{"n":2}`)},
	} {
		if err := store.Append(event); err != nil {
			t.Fatal(err)
		}
	}

	read, err := store.ReadEvents()
	if err != nil {
		t.Fatal(err)
	}
	queried, err := store.QueryEvents(EventQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(queried, read) {
		t.Fatalf("wildcard query = %#v, ReadEvents = %#v", queried, read)
	}
}
