//go:build linux || darwin

package tools

import (
	"os/exec"
	"syscall"
	"testing"
)

// The terminal manager sets Setpgid before requesting a network namespace so
// its close/timeout path can terminate the entire child process group. Keep
// this invariant explicit: replacing SysProcAttr here would orphan descendants
// whenever --no-net is enabled.
func TestSetNetNamespacePreservesExistingSysProcAttr(t *testing.T) {
	cmd := exec.Command("true")
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setpgid:   true,
		Pdeathsig: syscall.SIGTERM,
	}

	if err := SetNetNamespace(cmd); err != nil {
		t.Fatalf("SetNetNamespace: %v", err)
	}
	if cmd.SysProcAttr == nil {
		t.Fatal("SetNetNamespace cleared SysProcAttr")
	}
	if !cmd.SysProcAttr.Setpgid {
		t.Fatal("SetNetNamespace must preserve Setpgid for process-group cleanup")
	}
	if cmd.SysProcAttr.Pdeathsig != syscall.SIGTERM {
		t.Fatalf("Pdeathsig = %v, want %v", cmd.SysProcAttr.Pdeathsig, syscall.SIGTERM)
	}
	wantFlags := uintptr(syscall.CLONE_NEWNET | syscall.CLONE_NEWUSER)
	if cmd.SysProcAttr.Cloneflags&wantFlags != wantFlags {
		t.Fatalf("Cloneflags = %#x, want network and user namespace flags %#x", cmd.SysProcAttr.Cloneflags, wantFlags)
	}
}
