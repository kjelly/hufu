//go:build !windows

package team

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Phase 3 tests (spec.md §36 PR-07): reliable process-tree cleanup. Every
// test drives real helper child processes (plain shell scripts), never
// Codex, matching the spec's own instruction for this PR. This file is
// Unix-only (build-tagged !windows): §16.2 has no Windows implementation
// yet, and syscall.Kill/SIGINT/etc. do not exist on that platform's
// syscall package at all.

func requireUnixProcessSupervisor(t *testing.T) ProcessSupervisor {
	t.Helper()
	return NewProcessSupervisor()
}

func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || !errors.Is(err, syscall.ESRCH)
}

func waitUntil(t *testing.T, timeout time.Duration, condition func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return condition()
}

func writeExecutableScript(t *testing.T, dir, name, body string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestProcessSupervisorKillsDescendants proves KillTree reaches a
// background grandchild the direct child spawned, not only the direct child
// itself — the entire point of a process-group-based supervisor over a bare
// exec.CommandContext, which only ever kills the one process it started.
func TestProcessSupervisorKillsDescendants(t *testing.T) {
	sup := requireUnixProcessSupervisor(t)
	dir := t.TempDir()
	childPIDFile := filepath.Join(dir, "child.pid")
	heartbeatFile := filepath.Join(dir, "heartbeat")
	script := writeExecutableScript(t, dir, "spawn.sh", `#!/bin/sh
(while true; do date +%s%N > "$1"; sleep 0.05; done) &
echo $! > "$2"
wait
`)

	handle, err := sup.Start(context.Background(), ProcessSpec{Argv: []string{"sh", script, heartbeatFile, childPIDFile}})
	if err != nil {
		t.Fatal(err)
	}
	waitDone := make(chan error, 1)
	go func() { waitDone <- handle.Wait() }()

	if !waitUntil(t, 2*time.Second, func() bool {
		data, err := os.ReadFile(childPIDFile)
		return err == nil && strings.TrimSpace(string(data)) != ""
	}) {
		t.Fatal("grandchild never reported its PID")
	}
	childPIDData, err := os.ReadFile(childPIDFile)
	if err != nil {
		t.Fatal(err)
	}
	grandchildPID, err := strconv.Atoi(strings.TrimSpace(string(childPIDData)))
	if err != nil {
		t.Fatalf("parse grandchild pid: %v", err)
	}
	if !processAlive(grandchildPID) {
		t.Fatal("grandchild process is not alive before KillTree")
	}

	if err := sup.KillTree(context.Background(), handle); err != nil {
		t.Fatalf("KillTree: %v", err)
	}
	select {
	case <-waitDone:
	case <-time.After(2 * time.Second):
		t.Fatal("direct child did not exit after KillTree")
	}
	if !waitUntil(t, time.Second, func() bool { return !processAlive(grandchildPID) }) {
		t.Fatalf("grandchild pid %d is still alive after KillTree", grandchildPID)
	}
}

// TestProcessSupervisorCancellation proves Interrupt delivers a graceful,
// catchable signal that a normal (non-trapping) process exits on — the
// first step of §16.1's required cancellation flow.
func TestProcessSupervisorCancellation(t *testing.T) {
	sup := requireUnixProcessSupervisor(t)
	handle, err := sup.Start(context.Background(), ProcessSpec{Argv: []string{"sleep", "30"}})
	if err != nil {
		t.Fatal(err)
	}
	waitDone := make(chan error, 1)
	go func() { waitDone <- handle.Wait() }()

	if !waitUntil(t, time.Second, func() bool { return processAlive(handle.Pid()) }) {
		t.Fatal("child process never started")
	}
	if err := sup.Interrupt(context.Background(), handle); err != nil {
		t.Fatalf("Interrupt: %v", err)
	}
	select {
	case <-waitDone:
	case <-time.After(2 * time.Second):
		t.Fatal("process did not exit after Interrupt (SIGINT)")
	}
}

// TestProcessSupervisorTimeout simulates the escalation §16.1 requires when
// graceful signals do not stop a process in time: a process that traps and
// ignores both SIGINT and SIGTERM must still be reliably stopped by
// KillTree (SIGKILL, which cannot be caught).
func TestProcessSupervisorTimeout(t *testing.T) {
	sup := requireUnixProcessSupervisor(t)
	dir := t.TempDir()
	readyFile := filepath.Join(dir, "ready")
	script := writeExecutableScript(t, dir, "stubborn.sh", `#!/bin/sh
trap '' TERM INT
touch "$1"
while true; do sleep 0.05; done
`)
	handle, err := sup.Start(context.Background(), ProcessSpec{Argv: []string{"sh", script, readyFile}})
	if err != nil {
		t.Fatal(err)
	}
	waitDone := make(chan error, 1)
	go func() { waitDone <- handle.Wait() }()

	// The trap must actually be installed before any signal is sent — the
	// process existing is not enough, since sh has not necessarily reached
	// the trap statement yet.
	if !waitUntil(t, time.Second, func() bool {
		_, err := os.Stat(readyFile)
		return err == nil
	}) {
		t.Fatal("child process never signaled that its trap was installed")
	}
	if err := sup.Interrupt(context.Background(), handle); err != nil {
		t.Fatalf("Interrupt: %v", err)
	}
	if err := sup.TerminateTree(context.Background(), handle); err != nil {
		t.Fatalf("TerminateTree: %v", err)
	}
	select {
	case <-waitDone:
		t.Fatal("stubborn process exited on a graceful signal it was supposed to trap; test setup is broken")
	case <-time.After(300 * time.Millisecond):
	}
	if !processAlive(handle.Pid()) {
		t.Fatal("stubborn process unexpectedly exited before KillTree")
	}

	if err := sup.KillTree(context.Background(), handle); err != nil {
		t.Fatalf("KillTree: %v", err)
	}
	select {
	case <-waitDone:
	case <-time.After(2 * time.Second):
		t.Fatal("process ignoring graceful signals did not exit after KillTree (SIGKILL)")
	}
}
