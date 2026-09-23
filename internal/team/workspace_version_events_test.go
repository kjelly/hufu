package team

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/kjelly/hufu/internal/workspace/versionstore"
)

func openVersionTestStore(t *testing.T) *versionstore.Store {
	t.Helper()
	store, err := versionstore.Open(context.Background(), versionstore.Options{StateDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func captureForTest(t *testing.T, store *versionstore.Store, root, branch string, parent versionstore.SnapshotID) (versionstore.Snapshot, versionstore.CaptureStats) {
	t.Helper()
	snapshot, stats, err := store.Capture(context.Background(), versionstore.CaptureRequest{
		WorkspaceID: "ws_test", BranchID: branch, Root: root, Parent: parent, Reason: versionstore.SnapshotBaseline,
	})
	if err != nil {
		t.Fatal(err)
	}
	return snapshot, stats
}

// TestWorkspaceSnapshotCommitEventRoundTrip covers the event-first adapter:
// the payload survives redaction, retries are idempotent, and the commit
// lookup finds the durable event that recovery publishes from.
func TestWorkspaceSnapshotCommitEventRoundTrip(t *testing.T) {
	ctx := context.Background()
	workspace := t.TempDir()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("a"), 0o644); err != nil {
		t.Fatal(err)
	}
	store := openVersionTestStore(t)
	es, err := OpenEventStore(workspace)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = es.Close() }()

	snapshot, stats := captureForTest(t, store, root, "main", "")
	first, err := appendWorkspaceSnapshotCommitted(ctx, es, snapshot, stats)
	if err != nil {
		t.Fatal(err)
	}
	retry, err := appendWorkspaceSnapshotCommitted(ctx, es, snapshot, stats)
	if err != nil || retry.ID != first.ID {
		t.Fatalf("retried append = %s, %v; want deduplicated %s", retry.ID, err, first.ID)
	}
	events, err := es.ReadEvents()
	if err != nil {
		t.Fatal(err)
	}
	commits := 0
	for _, event := range events {
		if payload, ok := decodeWorkspaceSnapshotEvent(event); ok {
			commits++
			if payload.SnapshotID != string(snapshot.ID) || payload.RootTreeHash != snapshot.RootTreeHash || payload.ManifestDigest != snapshot.ManifestDigest || event.BranchID != "main" {
				t.Fatalf("stored payload = %#v branch %q", payload, event.BranchID)
			}
		}
	}
	if commits != 1 {
		t.Fatalf("commit events = %d, want 1", commits)
	}
	if !IsKnownEventType(string(EventWorkspaceSnapshotCommitted)) {
		t.Fatal("workspace_snapshot_committed is not in the event catalog")
	}

	// Crash after the event, before the head update: recovery publishes.
	actions, err := store.RecoverPendingSnapshots(ctx, "ws_test", workspaceCommitLookup(events))
	if err != nil || len(actions) != 1 || actions[0].Kind != versionstore.RecoveryPublished || actions[0].EventID != first.ID {
		t.Fatalf("recovery = %#v, %v", actions, err)
	}
	head, _, ok, err := store.GetBranchHead(ctx, "ws_test", "main")
	if err != nil || !ok || head.ID != snapshot.ID {
		t.Fatalf("head = %#v ok=%v err=%v", head, ok, err)
	}
}

// TestWorkspaceSnapshotRecoveryNeverAppends covers the crash before the event
// append: the pending snapshot is orphaned and the event log is unchanged.
func TestWorkspaceSnapshotRecoveryNeverAppends(t *testing.T) {
	ctx := context.Background()
	workspace := t.TempDir()
	store := openVersionTestStore(t)
	es, err := OpenEventStore(workspace)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = es.Close() }()
	pending, _ := captureForTest(t, store, t.TempDir(), "main", "")
	before, _ := es.ReadEvents()
	actions, err := store.RecoverPendingSnapshots(ctx, "ws_test", workspaceCommitLookup(before))
	if err != nil || len(actions) != 1 || actions[0].Kind != versionstore.RecoveryOrphaned || actions[0].SnapshotID != pending.ID {
		t.Fatalf("recovery = %#v, %v", actions, err)
	}
	after, _ := es.ReadEvents()
	if len(after) != len(before) {
		t.Fatalf("recovery appended %d events", len(after)-len(before))
	}
}

// TestWorkspaceSnapshotAtEvent covers §23 (IT3): a fork at event E uses the
// last snapshot committed at or before E in E's lineage, never a later one.
func TestWorkspaceSnapshotAtEvent(t *testing.T) {
	events := []RunEvent{
		{ID: "e1", BranchID: "main", Type: string(EventWorkspaceSnapshotCommitted), Payload: []byte(`{"snapshot_id":"wsv_s1"}`)},
		{ID: "e2", BranchID: "main", Type: "task_started"},
		{ID: "e3", BranchID: "main", Type: string(EventWorkspaceSnapshotCommitted), Payload: []byte(`{"snapshot_id":"wsv_s2"}`)},
		{ID: "e4", BranchID: "main", Type: "task_completed"},
		{ID: "x1", BranchID: "exp", Type: string(EventWorkspaceSnapshotCommitted), Payload: []byte(`{"snapshot_id":"wsv_exp"}`)},
	}
	tree := NewSessionTree()
	tree.Branches["exp"] = &SessionBranch{ID: "exp", Name: "exp", ParentID: "main", ForkEventID: "e2"}
	tests := []struct {
		branch, event string
		want          versionstore.SnapshotID
		found         bool
	}{
		{branch: "main", event: "e2", want: "wsv_s1", found: true},
		{branch: "main", event: "e4", want: "wsv_s2", found: true},
		{branch: "main", event: "e1", want: "wsv_s1", found: true},
		{branch: "main", event: "", want: "wsv_s2", found: true},
		{branch: "exp", event: "", want: "wsv_exp", found: true},
		{branch: "exp", event: "e2", want: "wsv_s1", found: true},
	}
	for _, tt := range tests {
		got, found, err := workspaceSnapshotAtEvent(events, tree, tt.branch, tt.event)
		if err != nil || found != tt.found || got != tt.want {
			t.Errorf("at(%s,%q) = %s,%v,%v; want %s,%v", tt.branch, tt.event, got, found, err, tt.want, tt.found)
		}
	}
	legacy := []RunEvent{{ID: "old", BranchID: "main", Type: "task_started"}}
	if _, found, err := workspaceSnapshotAtEvent(legacy, tree, "main", "old"); err != nil || found {
		t.Fatalf("legacy lineage: found=%v err=%v; want unavailable", found, err)
	}
	if _, _, err := workspaceSnapshotAtEvent(events, tree, "exp", "e4"); err == nil {
		t.Fatal("an event outside the branch lineage resolved")
	}
}
