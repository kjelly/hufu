package team

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// These are supplementary (not among PR-08/09/10's named tests): codex_process.go
// ties ProcessSupervisor (Phase 3) to CodexRPCClient (this phase) and would
// otherwise have zero test coverage of its own.

// TestStartCodexAppServerInitializesOverRealProcess proves the full startup
// sequence — executable validation, ProcessSupervisor-managed pipes, and the
// initialize gate — succeeds end to end against the fake server fixture.
func TestStartCodexAppServerInitializesOverRealProcess(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("ProcessSupervisor has no Windows implementation yet (§16.2)")
	}
	dir := t.TempDir()
	scriptPath := filepath.Join(dir, "script.json")
	data, err := json.Marshal(fakeCodexScript{
		Steps: []fakeCodexStep{{Result: rawJSON(t, map[string]any{"protocol_version": "v2", "server_version": "fake-1.0"})}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(scriptPath, data, 0o644); err != nil {
		t.Fatal(err)
	}

	sup := NewProcessSupervisor()
	proc, err := StartCodexAppServer(context.Background(), sup, CodexProcessConfig{
		Argv:           []string{os.Args[0]},
		Env:            append(os.Environ(), fakeCodexServerScriptEnvVar+"="+scriptPath),
		StartupTimeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatalf("StartCodexAppServer: %v", err)
	}
	t.Cleanup(func() {
		_ = sup.KillTree(context.Background(), proc.Handle())
		_ = proc.Client.Close()
	})
}

// TestStartCodexAppServerRejectsMissingExecutable proves a nonexistent
// executable fails preflight before any process, pipe, or model interaction
// (§13.2 step 1).
func TestStartCodexAppServerRejectsMissingExecutable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("ProcessSupervisor has no Windows implementation yet (§16.2)")
	}
	sup := NewProcessSupervisor()
	_, err := StartCodexAppServer(context.Background(), sup, CodexProcessConfig{
		Argv: []string{"hufu-codex-definitely-does-not-exist"},
	})
	if err == nil {
		t.Fatal("expected a missing executable to fail before starting any process")
	}
}

type initializeFailureProcessHandle struct {
	exited    chan struct{}
	waitCalls atomic.Int32
}

func (h *initializeFailureProcessHandle) Pid() int { return 4242 }

func (h *initializeFailureProcessHandle) Wait() error {
	h.waitCalls.Add(1)
	<-h.exited
	return nil
}

type initializeFailureSupervisor struct {
	handle      *initializeFailureProcessHandle
	killCalls   atomic.Int32
	stdinClosed chan struct{}
	exit        func()
}

func newInitializeFailureSupervisor() *initializeFailureSupervisor {
	supervisor := &initializeFailureSupervisor{
		handle:      &initializeFailureProcessHandle{exited: make(chan struct{})},
		stdinClosed: make(chan struct{}),
	}
	supervisor.exit = sync.OnceFunc(func() { close(supervisor.handle.exited) })
	return supervisor
}

func (s *initializeFailureSupervisor) Start(_ context.Context, spec ProcessSpec) (ProcessHandle, error) {
	go func() {
		reader := bufio.NewReader(spec.Stdin)
		line, err := reader.ReadBytes('\n')
		if err == nil {
			var request struct {
				ID int64 `json:"id"`
			}
			if json.Unmarshal(line, &request) == nil {
				_, _ = fmt.Fprintf(spec.Stdout, "{\"jsonrpc\":\"2.0\",\"id\":%d,\"error\":{\"code\":-32000,\"message\":\"injected initialize failure\"}}\n", request.ID)
			}
		}
		_, _ = io.Copy(io.Discard, reader)
		close(s.stdinClosed)
	}()
	return s.handle, nil
}

func (s *initializeFailureSupervisor) Interrupt(context.Context, ProcessHandle) error { return nil }

func (s *initializeFailureSupervisor) TerminateTree(context.Context, ProcessHandle) error { return nil }

func (s *initializeFailureSupervisor) KillTree(context.Context, ProcessHandle) error {
	s.killCalls.Add(1)
	s.exit()
	return nil
}

func TestStartCodexAppServerInitializeFailureClosesKillsAndReaps(t *testing.T) {
	sup := newInitializeFailureSupervisor()
	proc, err := StartCodexAppServer(t.Context(), sup, CodexProcessConfig{
		Argv:           []string{os.Args[0]},
		StartupTimeout: time.Second,
	})
	if err == nil || proc != nil {
		t.Fatalf("StartCodexAppServer = (%v, %v), want nil process and initialize error", proc, err)
	}
	if sup.killCalls.Load() != 1 {
		t.Fatalf("KillTree calls = %d, want 1", sup.killCalls.Load())
	}
	if sup.handle.waitCalls.Load() != 1 {
		t.Fatalf("Wait calls = %d, want 1 synchronous reap", sup.handle.waitCalls.Load())
	}
	select {
	case <-sup.stdinClosed:
	case <-time.After(time.Second):
		t.Fatal("Codex client stdin remained open after initialize failure")
	}
}
