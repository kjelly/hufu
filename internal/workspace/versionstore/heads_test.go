package versionstore

import (
	"context"
	"errors"
	"io/fs"
	"path/filepath"
	"testing"
)

func publish(t *testing.T, store *Store, snapshot Snapshot, expected int64) BranchHead {
	t.Helper()
	head, err := store.PublishSnapshot(context.Background(), snapshot.ID, "evt-"+string(snapshot.ID), expected)
	if err != nil {
		t.Fatalf("PublishSnapshot(%s, %d): %v", snapshot.ID, expected, err)
	}
	return head
}

func TestPublishSnapshotAdvancesHeadWithGenerations(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	root := t.TempDir()
	writeFiles(t, root, map[string]string{"a": "1"})
	s1 := mustCapture(t, store, baseRequest(root))
	if _, _, ok, _ := store.GetBranchHead(ctx, "ws_test", "main"); ok {
		t.Fatal("capture must not create a head")
	}
	head := publish(t, store, s1, 0)
	if head.Generation != 1 || head.SnapshotID != s1.ID {
		t.Fatalf("head = %#v", head)
	}
	if again := publish(t, store, s1, 0); again.Generation != 1 {
		t.Fatalf("idempotent republish moved the head: %#v", again)
	}
	got, _ := store.GetSnapshot(ctx, s1.ID)
	if got.State != StatePublished || got.CommitEventID != "evt-"+string(s1.ID) {
		t.Fatalf("published snapshot = %#v", got)
	}

	writeFiles(t, root, map[string]string{"a": "2"})
	req := baseRequest(root)
	req.Parent, req.Reason = s1.ID, SnapshotExternalDrift
	s2 := mustCapture(t, store, req)
	if _, err := store.PublishSnapshot(ctx, s2.ID, "evt-s2", 0); !errors.Is(err, ErrBranchHeadConflict) {
		t.Fatalf("expected 0 with an existing head: err = %v", err)
	}
	if _, err := store.PublishSnapshot(ctx, s2.ID, "evt-s2", 7); !errors.Is(err, ErrBranchHeadConflict) {
		t.Fatalf("stale generation: err = %v", err)
	}
	if got, _ := store.GetSnapshot(ctx, s2.ID); got.State != StatePending {
		t.Fatal("a failed publish must leave the snapshot pending")
	}
	if head = publish(t, store, s2, 1); head.Generation != 2 || head.SnapshotID != s2.ID {
		t.Fatalf("head after s2 = %#v", head)
	}

	// A node that does not descend from the head cannot become it.
	stray := mustCapture(t, store, baseRequest(root))
	if _, err := store.PublishSnapshot(ctx, stray.ID, "evt-stray", 2); !errors.Is(err, ErrBranchHeadConflict) {
		t.Fatalf("non-descendant publish: err = %v", err)
	}
	if _, err := store.PublishSnapshot(ctx, s2.ID, "", 2); err == nil {
		t.Fatal("publish without a commit event id succeeded")
	}
}

func TestDeriveSnapshotForksWithoutCopyingBytes(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	root := t.TempDir()
	writeFiles(t, root, map[string]string{"big.bin": "lots of bytes", "dir/x": "y"})
	s1 := mustCapture(t, store, baseRequest(root))
	publish(t, store, s1, 0)
	before := casBytes(t, store)
	child, err := store.AttachBranch(ctx, "ws_test", "exp", s1.ID, SnapshotFork)
	if err != nil {
		t.Fatal(err)
	}
	if child.ID == s1.ID || child.Parent != s1.ID || child.RootTreeHash != s1.RootTreeHash || child.BranchID != "exp" {
		t.Fatalf("child = %#v", child)
	}
	if after := casBytes(t, store); after != before {
		t.Fatalf("fork wrote %d CAS bytes", after-before)
	}
	head := publish(t, store, child, 0)
	if head.BranchID != "exp" || head.SnapshotID != child.ID {
		t.Fatalf("child head = %#v", head)
	}
	if _, err = store.AttachBranch(ctx, "ws_test", "exp2", "wsv_"+SnapshotID(hashB[:32]), SnapshotFork); !errors.Is(err, ErrSnapshotNotFound) {
		t.Fatalf("attach to missing base: err = %v", err)
	}
}

func casBytes(t *testing.T, store *Store) int64 {
	t.Helper()
	var total int64
	err := filepath.WalkDir(store.layout.objectsDir(), func(_ string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		info, infoErr := entry.Info()
		if infoErr != nil {
			return infoErr
		}
		total += info.Size()
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return total
}

func TestOrphanAndStateGuards(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	root := t.TempDir()
	s1 := mustCapture(t, store, baseRequest(root))
	if err := store.OrphanSnapshot(ctx, s1.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.OrphanSnapshot(ctx, s1.ID); !errors.Is(err, ErrWorkspaceSnapshotUnavailable) {
		t.Fatalf("second orphan: err = %v", err)
	}
	if _, err := store.PublishSnapshot(ctx, s1.ID, "evt", 0); !errors.Is(err, ErrWorkspaceSnapshotUnavailable) {
		t.Fatalf("publish orphaned: err = %v", err)
	}
	if _, err := store.Materialize(ctx, MaterializeRequest{Root: t.TempDir(), SnapshotID: s1.ID}); !errors.Is(err, ErrWorkspaceSnapshotUnavailable) {
		t.Fatalf("materialize orphaned: err = %v", err)
	}
}

func TestOperationsLifecycle(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	op, err := store.CreateOperation(ctx, Operation{WorkspaceID: "ws_test", BranchID: "main", Kind: OperationCheckout, TargetBranchID: "exp", FromSnapshotID: "wsv_a"})
	if err != nil {
		t.Fatal(err)
	}
	if op.State != OperationPrepared || op.ID == "" {
		t.Fatalf("op = %#v", op)
	}
	if err = store.UpdateOperation(ctx, op.ID, OperationUpdate{State: OperationMaterialized, ToSnapshotID: "wsv_b"}); err != nil {
		t.Fatal(err)
	}
	incomplete, err := store.IncompleteOperations(ctx)
	if err != nil || len(incomplete) != 1 || incomplete[0].State != OperationMaterialized || incomplete[0].ToSnapshotID != "wsv_b" || incomplete[0].TargetBranchID != "exp" {
		t.Fatalf("incomplete = %#v, %v", incomplete, err)
	}
	if err = store.UpdateOperation(ctx, op.ID, OperationUpdate{State: OperationCompleted}); err != nil {
		t.Fatal(err)
	}
	if err = store.UpdateOperation(ctx, op.ID, OperationUpdate{State: OperationFailed}); err == nil {
		t.Fatal("a terminal operation was updated")
	}
	if incomplete, _ = store.IncompleteOperations(ctx); len(incomplete) != 0 {
		t.Fatalf("completed op still incomplete: %#v", incomplete)
	}
	got, err := store.GetOperation(ctx, op.ID)
	if err != nil || got.State != OperationCompleted || got.ToSnapshotID != "wsv_b" {
		t.Fatalf("GetOperation = %#v, %v", got, err)
	}
}

func TestSubjectStateBindingAndModeFloor(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	root := mustCanonical(t, t.TempDir())
	state, err := store.EnsureSubjectState(ctx, root, ModeObserve)
	if err != nil || state.ModeFloor != ModeObserve || state.SubjectRoot != root {
		t.Fatalf("ensure observe = %#v, %v", state, err)
	}
	if state, err = store.EnsureSubjectState(ctx, root, ModeRequired); err != nil || state.ModeFloor != ModeRequired {
		t.Fatalf("raise to required = %#v, %v", state, err)
	}
	if _, err = store.EnsureSubjectState(ctx, root, ModeObserve); !errors.Is(err, ErrModeDowngradeRefused) {
		t.Fatalf("silent downgrade: err = %v", err)
	}
	if err = store.UpdateSubjectState(ctx, root, func(next *SubjectState) {
		next.RecoveryRequired, next.RecoveryCode, next.CheckpointDeferred = true, "unauthorized_mutation", true
		next.ModeFloor = ModeObserve
	}); err != nil {
		t.Fatal(err)
	}
	state, ok, err := store.GetSubjectState(ctx, root)
	if err != nil || !ok || !state.RecoveryRequired || state.RecoveryCode != "unauthorized_mutation" || !state.CheckpointDeferred || state.ModeFloor != ModeObserve {
		t.Fatalf("state = %#v ok=%v err=%v", state, ok, err)
	}
	if _, _, err = store.GetSubjectState(ctx, root+"-other"); err != nil {
		t.Fatalf("unbound root lookup: %v", err)
	}
	if err = store.UpdateSubjectState(ctx, root+"-other", func(*SubjectState) {}); !errors.Is(err, ErrVersionedWorkspaceUnresolved) {
		t.Fatalf("update unbound root: err = %v", err)
	}
}

// TestRecoverPendingSnapshotsAtEachStage covers the §16.2 crash points that
// versionstore owns: a pending snapshot without a durable event is orphaned
// (and no event is written), one with a durable event is published and its
// head repaired.
func TestRecoverPendingSnapshotsAtEachStage(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name       string
		eventFound bool
		wantState  SnapshotState
		wantHead   bool
	}{
		{name: "crash after snapshot insert, before event", eventFound: false, wantState: StateOrphaned},
		{name: "crash after event, before head update", eventFound: true, wantState: StatePublished, wantHead: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := openTestStore(t)
			root := t.TempDir()
			writeFiles(t, root, map[string]string{"a": "1"})
			pending := mustCapture(t, store, baseRequest(root))
			lookups := 0
			actions, err := store.RecoverPendingSnapshots(ctx, "ws_test", func(_ context.Context, id SnapshotID) (string, bool, error) {
				lookups++
				if id != pending.ID {
					t.Fatalf("lookup for unexpected snapshot %s", id)
				}
				return "evt-durable", tt.eventFound, nil
			})
			if err != nil || len(actions) != 1 || lookups != 1 {
				t.Fatalf("actions = %#v err = %v lookups = %d", actions, err, lookups)
			}
			got, _ := store.GetSnapshot(ctx, pending.ID)
			if got.State != tt.wantState {
				t.Fatalf("state = %s, want %s", got.State, tt.wantState)
			}
			_, head, hasHead, _ := store.GetBranchHead(ctx, "ws_test", "main")
			if hasHead != tt.wantHead || (tt.wantHead && (head.SnapshotID != pending.ID || got.CommitEventID != "evt-durable")) {
				t.Fatalf("head = %#v hasHead = %v", head, hasHead)
			}
			// Recovery is idempotent: nothing is pending any more.
			again, err := store.RecoverPendingSnapshots(ctx, "ws_test", func(context.Context, SnapshotID) (string, bool, error) {
				t.Fatal("second recovery looked up a settled snapshot")
				return "", false, nil
			})
			if err != nil || len(again) != 0 {
				t.Fatalf("second recovery = %#v, %v", again, err)
			}
		})
	}
}
