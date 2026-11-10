package team

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

func eventBatchFixture() []RunEvent {
	return []RunEvent{
		{Type: string(EventUserMessageAdded), Actor: "user", IdempotencyKey: "batch-user", Payload: []byte(`{"role":"user","content":"new request"}`)},
		{Type: string(EventAssistantMessageAdded), Actor: "assistant", IdempotencyKey: "batch-assistant", Payload: []byte(`{"role":"assistant","content":"response"}`)},
	}
}

func TestEventStoreAtomicBatchReplay(t *testing.T) {
	store, err := NewEventStore(t.TempDir(), "run-batch", "session-batch")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	batch := eventBatchFixture()
	first, err := store.AppendPersistedBatchAtHead(t.Context(), batch, "")
	if err != nil || len(first) != 2 || first[1].PreviousID != first[0].ID || first[1].PreviousHash != first[0].Hash {
		t.Fatalf("atomic chain = %+v, %v", first, err)
	}
	again, err := store.AppendPersistedBatchAtHead(t.Context(), batch, first[1].ID)
	if err != nil || !reflect.DeepEqual(first, again) || len(mustReadEvents(t, store)) != 2 {
		t.Fatalf("batch replay changed durable identity: %v", err)
	}
	for _, change := range []string{"payload", "partial", "duplicate", "missing_key"} {
		t.Run(change, func(t *testing.T) {
			request := eventBatchFixture()
			switch change {
			case "payload":
				request[1].Payload = []byte(`{"role":"assistant","content":"changed"}`)
			case "partial":
				request[1].IdempotencyKey = "different-member"
			case "duplicate":
				request[1] = request[0]
			case "missing_key":
				request[1].IdempotencyKey = ""
			}
			if _, err := store.AppendPersistedBatchAtHead(t.Context(), request, first[1].ID); !errors.Is(err, ErrEventBatchConflict) {
				t.Fatalf("changed atomic batch was accepted: %v", err)
			}
		})
	}
}

func TestEventStoreAtomicBatchRollsBackEveryMember(t *testing.T) {
	for _, failure := range []string{"validation", "database", "cancelled"} {
		t.Run(failure, func(t *testing.T) {
			store, err := NewEventStore(t.TempDir(), "run-batch", "session-batch")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = store.Close() })
			batch := eventBatchFixture()
			ctx := t.Context()
			switch failure {
			case "validation":
				batch[1].Type, batch[1].Payload = string(EventTaskCompleted), nil
			case "database":
				_, err = store.store.DB().ExecContext(ctx, `CREATE TRIGGER refuse_second_batch_event BEFORE INSERT ON events WHEN NEW.idempotency_key = 'batch-assistant' BEGIN SELECT RAISE(ABORT, 'fixture rejection'); END`)
				if err != nil {
					t.Fatal(err)
				}
			case "cancelled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			if _, err := store.AppendPersistedBatchAtHead(ctx, batch, ""); err == nil {
				t.Fatal("failed batch returned success")
			}
			if err := store.VerifyHashChain(); err != nil {
				t.Fatal(err)
			}
			if len(mustReadEvents(t, store)) != 0 {
				t.Fatal("failed batch retained a partial transition")
			}
		})
	}
}

func TestEventStoreAtomicBatchReconcilesUncertainCommit(t *testing.T) {
	workspace := t.TempDir()
	store, err := NewEventStore(workspace, "run-batch", "session-batch")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	store.commitHook = func() error { return errors.New("fixture sync failure") }
	if _, err := store.AppendPersistedBatchAtHead(t.Context(), eventBatchFixture(), ""); err == nil {
		t.Fatal("uncertain durability was reported as success")
	}
	reopened, err := NewEventStore(workspace, "run-batch", "session-batch")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	events := mustReadEvents(t, reopened)
	if len(events) != 2 {
		t.Fatalf("uncertain commit retained %d members", len(events))
	}
	replayed, err := reopened.AppendPersistedBatchAtHead(t.Context(), eventBatchFixture(), events[1].ID)
	if err != nil || !reflect.DeepEqual(replayed, events) || len(mustReadEvents(t, reopened)) != 2 {
		t.Fatalf("uncertain batch was duplicated: %v", err)
	}
}
