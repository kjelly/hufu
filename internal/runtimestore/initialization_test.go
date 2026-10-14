package runtimestore

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/kjelly/hufu/internal/fsutil"
)

func TestOpenWaitsForInitializationAndReleasesLock(t *testing.T) {
	workspace := t.TempDir()
	lockPath := Path(workspace) + ".init.lock"
	lock, err := fsutil.TryLockExclusive(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lock.Close() })
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	type opened struct {
		store *Store
		err   error
	}
	result := make(chan opened, 1)
	go func() {
		store, err := Open(ctx, workspace)
		result <- opened{store, err}
	}()
	select {
	case got := <-result:
		if got.store != nil {
			_ = got.store.Close()
		}
		t.Fatalf("Open finished while another initializer held the lock: %v", got.err)
	case <-time.After(30 * time.Millisecond):
	}
	if Exists(workspace) {
		t.Fatal("Open created the database before acquiring the initialization lock")
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
	got := <-result
	if got.err != nil {
		t.Fatal(got.err)
	}
	t.Cleanup(func() { _ = got.store.Close() })
	// Initialization must release the lock while the store remains open.
	lock, err = fsutil.TryLockExclusive(lockPath)
	if err != nil {
		t.Fatalf("Open retained the initialization lock: %v", err)
	}
	if err := got.store.PutDocument(ctx, DocumentSession, []byte(`{}`)); err != nil {
		t.Fatalf("ordinary writes require the initialization lock: %v", err)
	}
}

func TestOpenInitializationWaitHonorsContext(t *testing.T) {
	workspace := t.TempDir()
	lock, err := fsutil.TryLockExclusive(Path(workspace) + ".init.lock")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lock.Close() }()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel()
	store, err := Open(ctx, workspace)
	if store != nil {
		_ = store.Close()
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Open error = %v, want context.DeadlineExceeded", err)
	}
	if Exists(workspace) {
		t.Fatal("a canceled initialization wait created the database")
	}
}

func TestFailedInitializationReleasesLock(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(Path(workspace), []byte("invalid database"), 0o600); err != nil {
		t.Fatal(err)
	}
	if store, err := Open(t.Context(), workspace); err == nil {
		_ = store.Close()
		t.Fatal("Open accepted an invalid database")
	}
	lock, err := fsutil.TryLockExclusive(Path(workspace) + ".init.lock")
	if err != nil {
		t.Fatalf("failed Open retained the initialization lock: %v", err)
	}
	defer func() { _ = lock.Close() }()
}
