package versionstore

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// linearHistory publishes n snapshots on main, each changing unique.txt and
// sharing shared.txt, and returns them oldest first.
func linearHistory(t *testing.T, store *Store, root string, n int) []Snapshot {
	t.Helper()
	var out []Snapshot
	var parent SnapshotID
	for index := 0; index < n; index++ {
		writeFiles(t, root, map[string]string{"shared.txt": "same in every snapshot", "unique.txt": strings.Repeat("x", index+1)})
		req := baseRequest(root)
		req.Parent = parent
		if parent != "" {
			req.Reason = SnapshotExternalDrift
		}
		snapshot := mustCapture(t, store, req)
		expected := int64(index)
		publish(t, store, snapshot, expected)
		parent = snapshot.ID
		out = append(out, snapshot)
	}
	return out
}

func TestGCPrunesOutsideRetentionAndSweepsUnreachableObjects(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	root := t.TempDir()
	history := linearHistory(t, store, root, 5)
	orphan := mustCapture(t, store, func() CaptureRequest {
		writeFiles(t, root, map[string]string{"orphan-only.txt": "orphan"})
		return baseRequest(root)
	}())
	if err := store.OrphanSnapshot(ctx, orphan.ID); err != nil {
		t.Fatal(err)
	}

	dry, err := store.GC(ctx, GCRequest{KeepRecent: 2})
	if err != nil {
		t.Fatal(err)
	}
	// The two most recent (the head is one of them) are kept; the three
	// oldest are prunable.
	if len(dry.SnapshotsPrunable) != 3 || dry.ObjectsPrunable == 0 || dry.Applied {
		t.Fatalf("dry run = %#v", dry)
	}
	if got, _ := store.GetSnapshot(ctx, history[0].ID); got.State != StatePublished {
		t.Fatal("a dry run changed state")
	}
	applied, err := store.GC(ctx, GCRequest{KeepRecent: 2, Apply: true})
	if err != nil {
		t.Fatal(err)
	}
	if applied.ObjectsPrunable != dry.ObjectsPrunable || !applied.Applied {
		t.Fatalf("applied = %#v, dry = %#v", applied, dry)
	}
	for index, snapshot := range history {
		got, _ := store.GetSnapshot(ctx, snapshot.ID)
		wantPruned := index < 3
		if (got.State == StatePruned) != wantPruned {
			t.Fatalf("snapshot %d state = %s, want pruned=%v", index, got.State, wantPruned)
		}
	}
	// Retained snapshots stay fully intact, including the shared blob.
	for _, snapshot := range history[3:] {
		if err = store.Verify(ctx, snapshot.ID); err != nil {
			t.Fatalf("retained snapshot %s: %v", snapshot.ID, err)
		}
	}
	// A pruned snapshot is unavailable, never silently substituted.
	if _, err = store.Materialize(ctx, MaterializeRequest{Root: t.TempDir(), SnapshotID: history[0].ID}); !errors.Is(err, ErrWorkspaceSnapshotUnavailable) {
		t.Fatalf("materialize pruned: err = %v", err)
	}
	if _, err = store.Diff(ctx, history[0].ID, history[4].ID); !errors.Is(err, ErrWorkspaceSnapshotUnavailable) {
		t.Fatalf("diff pruned: err = %v", err)
	}
	// Lineage rows survive: the pruned snapshot's parent link is intact.
	if got, _ := store.GetSnapshot(ctx, history[1].ID); got.Parent != history[0].ID || got.State != StatePruned {
		t.Fatal("pruning broke the DAG lineage")
	}
	again, err := store.GC(ctx, GCRequest{KeepRecent: 2, Apply: true})
	if err != nil || len(again.SnapshotsPrunable) != 0 || again.ObjectsPrunable != 0 {
		t.Fatalf("second GC = %#v, %v", again, err)
	}
}

func TestGCGraceProtectsRecentObjectsAndPending(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	root := t.TempDir()
	writeFiles(t, root, map[string]string{"in-flight.txt": "being captured"})
	pending := mustCapture(t, store, baseRequest(root))
	result, err := store.GC(ctx, GCRequest{Grace: time.Hour, Apply: true})
	if err != nil {
		t.Fatal(err)
	}
	if result.ObjectsPrunable != 0 {
		t.Fatalf("grace period did not protect fresh objects: %#v", result)
	}
	if err = store.Verify(ctx, pending.ID); err != nil {
		t.Fatalf("in-flight pending snapshot lost objects: %v", err)
	}
	stale := filepath.Join(store.layout.tmpDir(), "obj-stale")
	if err = os.WriteFile(stale, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * time.Hour)
	if err = os.Chtimes(stale, old, old); err != nil {
		t.Fatal(err)
	}
	result, err = store.GC(ctx, GCRequest{Grace: time.Hour, Apply: true})
	if err != nil || result.TempFilesPrunable != 1 {
		t.Fatalf("temp sweep = %#v, %v", result, err)
	}
	if _, err = os.Stat(stale); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("stale temp file survived")
	}
}

func TestGCPinnedAndMaterializedAreRoots(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	root := t.TempDir()
	history := linearHistory(t, store, root, 3)
	canonical := mustCanonical(t, root)
	if _, err := store.EnsureSubjectState(ctx, canonical, ModeRequired); err != nil {
		t.Fatal(err)
	}
	if err := store.UpdateSubjectState(ctx, canonical, func(state *SubjectState) { state.MaterializedSnapshotID = history[1].ID }); err != nil {
		t.Fatal(err)
	}
	result, err := store.GC(ctx, GCRequest{KeepRecent: 0, Pinned: []SnapshotID{history[0].ID}, Apply: true})
	if err != nil || len(result.SnapshotsPrunable) != 0 {
		t.Fatalf("GC = %#v, %v; pinned and materialized snapshots must be roots", result, err)
	}
}

func TestLiveRootTreeWritesNothing(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	root := t.TempDir()
	writeFiles(t, root, map[string]string{"a": "1", "dir/b": "2"})
	snapshot := mustCapture(t, store, baseRequest(root))
	before := casBytes(t, store)
	live, err := store.LiveRootTree(ctx, baseRequest(root))
	if err != nil || live != snapshot.RootTreeHash {
		t.Fatalf("live = %s, %v; want %s", live, err, snapshot.RootTreeHash)
	}
	writeFiles(t, root, map[string]string{"a": "changed"})
	live, err = store.LiveRootTree(ctx, baseRequest(root))
	if err != nil || live == snapshot.RootTreeHash {
		t.Fatalf("drifted live = %s, %v", live, err)
	}
	if casBytes(t, store) != before {
		t.Fatal("LiveRootTree wrote CAS objects")
	}
	readOnly, err := Open(ctx, Options{StateDir: filepath.Dir(store.Root()), ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = readOnly.Close() }()
	if _, err = readOnly.LiveRootTree(ctx, baseRequest(root)); err != nil {
		t.Fatalf("LiveRootTree on a read-only store: %v", err)
	}
}

func TestDoctorFindsCorruptionAndRepairsHeads(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	root := t.TempDir()
	history := linearHistory(t, store, root, 2)
	if issues, err := store.Doctor(ctx); err != nil || len(issues) != 0 {
		t.Fatalf("clean doctor = %#v, %v", issues, err)
	}
	path, _ := store.layout.objectPath(ObjectBlob, sha("same in every snapshot"))
	if err := os.WriteFile(path, []byte("SAME IN EVERY SNAPSHOT"), 0o600); err != nil {
		t.Fatal(err)
	}
	issues, err := store.Doctor(ctx)
	if err != nil {
		t.Fatal(err)
	}
	corrupt := 0
	for _, issue := range issues {
		if issue.Code == "cas_integrity" && issue.Severity == "error" {
			corrupt++
		}
	}
	if corrupt != 2 {
		t.Fatalf("issues = %#v; want both snapshots flagged", issues)
	}
	if err = store.RepairBranchHead(ctx, "ws_test", "main", history[0].ID); err != nil {
		t.Fatal(err)
	}
	if head, _, _, _ := store.GetBranchHead(ctx, "ws_test", "main"); head.ID != history[0].ID {
		t.Fatalf("repaired head = %s", head.ID)
	}
	if err = store.RepairBranchHead(ctx, "ws_test", "exp", history[0].ID); !errors.Is(err, ErrWorkspaceSnapshotUnavailable) {
		t.Fatalf("repair onto another branch's snapshot: err = %v", err)
	}
}

func TestDowngradeLowersTheFloorExplicitly(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	root := mustCanonical(t, t.TempDir())
	if _, err := store.EnsureSubjectState(ctx, root, ModeRequired); err != nil {
		t.Fatal(err)
	}
	if err := store.Downgrade(ctx, root, "ws_test", ModeRequired); err == nil {
		t.Fatal("downgrade to the same mode succeeded")
	}
	if err := store.Downgrade(ctx, root, "ws_test", ModeOff); err != nil {
		t.Fatal(err)
	}
	if _, err := store.EnsureSubjectState(ctx, root, ModeObserve); err != nil {
		t.Fatalf("observe after downgrade: %v", err)
	}
	if ops, _ := store.IncompleteOperations(ctx); len(ops) != 0 {
		t.Fatalf("downgrade left an incomplete operation: %#v", ops)
	}
}
