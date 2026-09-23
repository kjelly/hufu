package versionstore

import (
	"errors"
	"strings"
	"testing"
)

func TestTryLockProject(t *testing.T) {
	stateDir := t.TempDir()
	first, err := TryLockProject(stateDir, LockOwner{WorkspaceID: "ws_a", Operation: "run"})
	if err != nil {
		t.Fatal(err)
	}
	owner, ok, err := ReadLockOwner(stateDir)
	if err != nil || !ok || owner.WorkspaceID != "ws_a" || owner.Operation != "run" || owner.PID == 0 {
		t.Fatalf("owner = %#v ok=%v err=%v", owner, ok, err)
	}
	_, err = TryLockProject(stateDir, LockOwner{WorkspaceID: "ws_b", Operation: "checkout"})
	if !errors.Is(err, ErrVersionOperationInProgress) || !strings.Contains(err.Error(), "ws_a") {
		t.Fatalf("second lock err = %v", err)
	}
	if err = first.Close(); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ = ReadLockOwner(stateDir); ok {
		t.Fatal("owner record survived release")
	}
	again, err := TryLockProject(stateDir, LockOwner{WorkspaceID: "ws_b", Operation: "checkout"})
	if err != nil {
		t.Fatalf("relock: %v", err)
	}
	_ = again.Close()
}

func TestTryLockProjectUnsupportedPlatform(t *testing.T) {
	previous := goos
	goos = "darwin"
	t.Cleanup(func() { goos = previous })
	if _, err := TryLockProject(t.TempDir(), LockOwner{}); !errors.Is(err, ErrUnsupportedPlatform) {
		t.Fatalf("err = %v, want ErrUnsupportedPlatform", err)
	}
}
