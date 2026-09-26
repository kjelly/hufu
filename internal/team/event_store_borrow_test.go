package team

import (
	"errors"
	"testing"
)

// TestBorrowEventStore lends only a live, valid store for the same workspace;
// anything else is replaced by a freshly opened store that release closes.
func TestBorrowEventStore(t *testing.T) {
	tests := []struct {
		name     string
		lender   func(t *testing.T, workspace string) *EventStore
		wantLent bool
	}{
		{name: "the workspace's live store is lent", lender: func(t *testing.T, workspace string) *EventStore {
			return newBorrowTestStore(t, workspace)
		}, wantLent: true},
		{name: "no store opens one", lender: func(*testing.T, string) *EventStore { return nil }},
		{name: "another workspace's store is not lent", lender: func(t *testing.T, _ string) *EventStore {
			return newBorrowTestStore(t, t.TempDir())
		}},
		{name: "a closed store is not lent", lender: func(t *testing.T, workspace string) *EventStore {
			store := newBorrowTestStore(t, workspace)
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			return store
		}},
		{name: "a degraded store is not lent", lender: func(t *testing.T, workspace string) *EventStore {
			store := newBorrowTestStore(t, workspace)
			store.invalidateState(errors.New("simulated failed append"))
			return store
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			workspace := t.TempDir()
			seed := newBorrowTestStore(t, workspace)
			if err := seed.Append(RunEvent{ID: "seed", Type: "task_progress", Actor: "worker", Payload: []byte(`{"progress":"seed"}`)}); err != nil {
				t.Fatal(err)
			}
			lender := tt.lender(t, workspace)

			store, release, err := borrowEventStore(workspace, lender)
			if err != nil {
				t.Fatal(err)
			}
			if lent := store == lender; lent != tt.wantLent {
				t.Fatalf("store lent = %v, want %v", lent, tt.wantLent)
			}
			events, err := store.ReadEvents()
			if err != nil || len(events) != 1 || events[0].ID != "seed" {
				t.Fatalf("borrowed store events = %v, %v; want the seeded event", events, err)
			}
			release()
			if store.closed == tt.wantLent {
				t.Fatalf("after release closed = %v; a lent store stays open, an opened one is closed", store.closed)
			}
		})
	}
}

func newBorrowTestStore(t *testing.T, workspace string) *EventStore {
	t.Helper()
	store, err := NewEventStore(workspace, "run-borrow", "session-borrow")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// TestMaterializeCompactionBranchReadsLentEventStore keeps branch forking on
// the caller's store: both the fork lineage and checkpoint pruning read it
// instead of rescanning the log.
func TestMaterializeCompactionBranchReadsLentEventStore(t *testing.T) {
	workspace := t.TempDir()
	state := testCompactionState(t, workspace)
	generation := state.Generations["g-1"]
	checkpoint := state.Branches["main"]
	es := newBorrowTestStore(t, workspace)
	if _, err := es.AppendPersisted(RunEvent{
		ID: compactionGenerationEventID(generation.ID), BranchID: "main", Type: compactionGenerationEventType,
		Actor: "coordinator", Payload: compactionJSON(CompactionReference{GenerationID: generation.ID, BranchID: generation.BranchID, Checksum: generation.Checksum}),
	}); err != nil {
		t.Fatal(err)
	}
	checkpointEvent, err := compactionCheckpointAttestationEvent(checkpoint, generation)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := es.AppendPersisted(checkpointEvent); err != nil {
		t.Fatal(err)
	}
	tree := NewSessionTree()
	branch, err := tree.CreateBranch("feature", checkpoint.EventID, es)
	if err != nil {
		t.Fatal(err)
	}
	if err := SaveSessionTree(workspace, tree); err != nil {
		t.Fatal(err)
	}

	reads := es.cacheHitCount
	if err := MaterializeCompactionBranchWithEvents(workspace, es, "main", branch.ID, branch.ForkEventID); err != nil {
		t.Fatal(err)
	}
	if got := es.cacheHitCount - reads; got < 2 {
		t.Fatalf("lent store answered %d reads, want the fork lineage and pruning reads", got)
	}
	materialized, exists, err := LoadConversationCompactionState(workspace)
	if err != nil || !exists {
		t.Fatalf("load materialized state: exists=%v err=%v", exists, err)
	}
	if _, ok := materialized.Branches[branch.ID]; !ok {
		t.Fatalf("child branch %q has no materialized checkpoint", branch.ID)
	}
}
