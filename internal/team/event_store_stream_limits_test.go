package team

import (
	"context"
	"errors"
	"testing"
)

func TestStreamValidatedRunEventsLimitsBeforeVisit(t *testing.T) {
	workspace := t.TempDir()
	store, err := NewEventStore(workspace, "run-bounded", "session-bounded")
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte(`{"value":"界"}`)
	for _, id := range []string{"first", "second"} {
		if err := store.Append(RunEvent{ID: id, Type: "memory_retrieved", Actor: "runtime", Payload: payload}); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name      string
		limits    EventReadLimits
		wantLimit bool
	}{
		{"event_overflow", EventReadLimits{MaxEvents: 1}, true},
		{"byte_overflow", EventReadLimits{MaxPayloadBytes: int64(len(payload)*2 - 1)}, true},
		{"exact", EventReadLimits{MaxEvents: 2, MaxPayloadBytes: int64(len(payload) * 2)}, false},
		{"unlimited", EventReadLimits{}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			visited := 0
			err := StreamValidatedRunEventsWithLimits(t.Context(), workspace, tc.limits, func(RunEvent) error {
				visited++
				return nil
			})
			if tc.wantLimit {
				if !errors.Is(err, ErrReadLimit) || visited != 0 {
					t.Fatalf("limit admitted payloads: visited=%d err=%v", visited, err)
				}
			} else if err != nil || visited != 2 {
				t.Fatalf("valid journal rejected: visited=%d err=%v", visited, err)
			}
		})
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	err = StreamValidatedRunEventsWithLimits(ctx, workspace, EventReadLimits{MaxEvents: 2}, func(RunEvent) error {
		t.Fatal("cancelled request visited an event")
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation lost: %v", err)
	}
}

func TestStreamValidatedRunEventsLimitedSnapshot(t *testing.T) {
	workspace := t.TempDir()
	store, err := NewEventStore(workspace, "run-snapshot", "session-snapshot")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	if err := store.Append(RunEvent{ID: "before", Type: "memory_retrieved", Actor: "runtime"}); err != nil {
		t.Fatal(err)
	}
	var ids []string
	err = StreamValidatedRunEventsWithLimits(t.Context(), workspace, EventReadLimits{MaxEvents: 1}, func(event RunEvent) error {
		ids = append(ids, event.ID)
		return store.Append(RunEvent{ID: "after", Type: "memory_retrieved", Actor: "runtime"})
	})
	if err != nil || len(ids) != 1 || ids[0] != "before" {
		t.Fatalf("read escaped admitted snapshot: %v %v", ids, err)
	}
	visited := false
	err = StreamValidatedRunEventsWithLimits(t.Context(), workspace, EventReadLimits{MaxEvents: 1}, func(RunEvent) error {
		visited = true
		return nil
	})
	if !errors.Is(err, ErrReadLimit) || visited {
		t.Fatalf("next read missed concurrent append: visited=%v err=%v", visited, err)
	}
}
