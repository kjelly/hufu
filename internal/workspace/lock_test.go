package workspace

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWorkspaceLocksAreExclusiveEmptyAndReusable(t *testing.T) {
	stateRoot := filepath.Join(t.TempDir(), "state")
	id := "ws_" + strings.Repeat("11", 16)
	first, err := AcquireWorkspaceLocks(stateRoot, []string{id, id})
	if err != nil {
		t.Fatal(err)
	}
	second, err := AcquireWorkspaceLocks(stateRoot, []string{id})
	if !errors.Is(err, ErrBusy) {
		t.Fatalf("second lock error = %v, want ErrBusy", err)
	}
	if second != nil {
		t.Fatal("busy acquisition returned locks")
	}
	path := filepath.Join(stateRoot, "locks", id+".lock")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != 0 || info.Mode().Perm() != 0o600 {
		t.Fatalf("lock file size/mode = %d/%o", info.Size(), info.Mode().Perm())
	}
	if err = first.Close(); err != nil {
		t.Fatal(err)
	}
	reacquired, err := AcquireWorkspaceLocks(stateRoot, []string{id})
	if err != nil {
		t.Fatal(err)
	}
	if err = reacquired.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestWorkspaceLocksReleaseEarlierLocksOnBusyFailure(t *testing.T) {
	stateRoot := filepath.Join(t.TempDir(), "state")
	firstID := "ws_" + strings.Repeat("11", 16)
	secondID := "ws_" + strings.Repeat("22", 16)
	held, err := AcquireWorkspaceLocks(stateRoot, []string{secondID})
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	if locks, lockErr := AcquireWorkspaceLocks(stateRoot, []string{secondID, firstID}); !errors.Is(lockErr, ErrBusy) || locks != nil {
		t.Fatalf("multi-lock result = %v, %v", locks, lockErr)
	}
	first, err := AcquireWorkspaceLocks(stateRoot, []string{firstID})
	if err != nil {
		t.Fatalf("earlier lock was not released: %v", err)
	}
	if err = first.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestWorkspaceLocksRejectInvalidIDWithoutFilesystemMutation(t *testing.T) {
	stateRoot := filepath.Join(t.TempDir(), "state")
	if _, err := AcquireWorkspaceLocks(stateRoot, []string{"../../escape"}); err == nil {
		t.Fatal("invalid workspace ID unexpectedly succeeded")
	}
	if _, err := os.Stat(stateRoot); !os.IsNotExist(err) {
		t.Fatalf("invalid lock request created state root: %v", err)
	}
}
