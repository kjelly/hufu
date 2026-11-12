package workspace

import (
	"context"
	"errors"
	"testing"
)

func TestWorkspacePurgePreservationRunsUnderLockBeforeDestruction(t *testing.T) {
	fixture := newLifecycleFixture(t, nil)
	deleted, err := fixture.manager.Delete(t.Context(), DeleteRequest{StartDir: fixture.subjectRoot, TeamName: "default"})
	if err != nil {
		t.Fatal(err)
	}
	item := deleted.Items[0]
	stop := errors.New("knowledge storage failed")
	_, err = fixture.manager.Purge(t.Context(), PurgeRequest{TrashID: item.TrashID, Preserve: func(ctx context.Context, trash TrashWorkspace) error {
		if trash.TrashID != item.TrashID || !pathExists(trash.TrashPath) {
			t.Fatal("preservation ran without source")
		}
		locks, err := AcquireWorkspaceLocks(fixture.stateRoot, []string{trash.WorkspaceID})
		if err == nil {
			_ = locks.Close()
			t.Fatal("preservation was not locked")
		}
		if !errors.Is(err, ErrBusy) {
			t.Fatalf("lock error=%v", err)
		}
		return stop
	}})
	if !errors.Is(err, stop) || !pathExists(item.TrashPath) {
		t.Fatalf("purge=%v source retained=%v", err, pathExists(item.TrashPath))
	}
	if err = fixture.manager.VisitTrash(t.Context(), item.TrashID, func(trash TrashWorkspace) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err = fixture.manager.Purge(t.Context(), PurgeRequest{TrashID: item.TrashID, Preserve: func(context.Context, TrashWorkspace) error { return nil }}); err != nil {
		t.Fatal(err)
	}
	if pathExists(item.TrashPath) {
		t.Fatal("successful preservation did not allow purge")
	}
}
