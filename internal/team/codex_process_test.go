package team

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
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
	skipUnsupportedCodexIntegration(t)
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
	skipUnsupportedCodexIntegration(t)
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
	stderr      string
	reply       jsonRPCError
	killError   error
}

const initializeFixtureCredential = "opaque-initialize-fixture-65201"

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
				_, _ = fmt.Fprintln(spec.Stderr, s.stderr)
				_ = json.NewEncoder(spec.Stdout).Encode(map[string]any{"jsonrpc": "2.0", "id": request.ID, "error": s.reply})
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
	if s.killError != nil {
		return s.killError
	}
	s.exit()
	return nil
}

func TestStartCodexAppServerInitializeFailureClosesKillsAndReaps(t *testing.T) {
	skipUnsupportedCodexIntegration(t)
	for _, source := range []string{"stderr", "RPC data", "inherited environment"} {
		t.Run(source, func(t *testing.T) {
			sup := newInitializeFailureSupervisor()
			credential := initializeFixtureCredential
			var env []string
			if source == "inherited environment" {
				credential = "a plain inherited startup passphrase"
				env = []string{"TEST_CREDENTIAL=" + credential}
			}
			sup.reply = jsonRPCError{Code: -32000, Message: "injected initialize failure " + credential}
			sup.stderr = "startup detail API_TOKEN=" + credential
			switch source {
			case "RPC data":
				sup.stderr = "startup detail " + initializeFixtureCredential
				sup.reply.Data = rawJSON(t, map[string]string{"api_token": initializeFixtureCredential})
			case "inherited environment":
				sup.stderr = "startup detail " + credential
			}
			proc, err := StartCodexAppServer(t.Context(), sup, CodexProcessConfig{
				Argv:           []string{os.Args[0]},
				Env:            env,
				StartupTimeout: time.Second,
			})
			rpc, ok := errors.AsType[*jsonRPCError](err)
			initialization, typed := errors.AsType[*codexInitializationFailure](err)
			if !ok || rpc.Code != sup.reply.Code || len(rpc.Data) != 0 || proc != nil || !typed || !initialization.settled {
				t.Fatalf("startup = (%v, %v), want nil process and typed initialize error", proc, err)
			}
			if body := err.Error(); !strings.Contains(body, "injected initialize failure [REDACTED]") ||
				!strings.Contains(body, "startup detail") || strings.Count(body, "[REDACTED]") < 2 || strings.Contains(body, credential) {
				t.Fatal("initialize failure did not retain its redacted message and stderr")
			}
			if sup.killCalls.Load() != 1 || sup.handle.waitCalls.Load() != 1 {
				t.Fatal("failed initialization was not terminated and synchronously reaped")
			}
			select {
			case <-sup.stdinClosed:
			case <-time.After(time.Second):
				t.Fatal("Codex client stdin remained open after initialize failure")
			}
		})
	}
}

type codexStartupSnapshotProbe struct {
	ExecutionWorld
	calls int
}

func (w *codexStartupSnapshotProbe) Snapshot(context.Context, *PreparedExecutionWorld) (WorkspaceSnapshot, error) {
	w.calls++
	return WorkspaceSnapshot{}, errors.New("injected snapshot probe")
}

func TestCodexUnsettledInitializationBlocksWorkspaceEvidence(t *testing.T) {
	skipUnsupportedCodexIntegration(t)
	f := newVersionFixture(t)
	c := runtimeCoordinator(t, f)
	if err := c.admitWorkspaceVersion(t.Context()); err != nil {
		t.Fatal(err)
	}
	_, provider, item, _ := newCodexHarness(t, f.subject, nil)
	provider.coordinator = c
	sup := newInitializeFailureSupervisor()
	sup.reply = jsonRPCError{Code: -32000, Message: "injected initialize failure"}
	sup.killError = errors.New("injected process group cleanup failure")
	t.Cleanup(sup.exit)
	world := &codexStartupSnapshotProbe{}
	request := codexAttemptRequest(item, "correct result")
	local := NewLocalExecutionWorld()
	prepared, err := local.Prepare(t.Context(), ExecutionWorldSpec{
		Root: f.subject, CWD: f.subject, SideEffect: SideEffectWorkspaceWrite,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = local.Release(context.Background(), prepared) })
	canonical, claims, _, _, err := provider.attemptResultRepair(t.Context(), sup, request, world, "original-thread",
		prepared, WorkspaceDelta{}, errors.New("invalid proposal"), &codexTranscript{}, nil, nil,
		time.Second, time.Second, time.Second, nil)
	initialization, typed := errors.AsType[*codexInitializationFailure](err)
	providerFailure, failed := errors.AsType[*CodexProviderError](err)
	if !typed || initialization.settled || !failed || providerFailure.Class != CodexFailureWorkspaceViolation || sup.killCalls.Load() != 1 {
		t.Fatalf("unsettled initialization did not reach settlement boundary: %v", err)
	}
	if canonical != nil || claims != nil || world.calls != 0 {
		t.Fatalf("unsettled initialization produced result or attempted %d workspace snapshots", world.calls)
	}
	f.reopen()
	if state := subjectState(t, f); !state.RecoveryRequired || state.RecoveryCode != "provider_process_unsettled" {
		t.Fatalf("reopened workspace recovery state = %+v", state)
	}
}

func TestStartCodexAppServerSettlesFramesWrittenBeforeExit(t *testing.T) {
	skipUnsupportedCodexIntegration(t)
	if runtime.GOOS == "windows" {
		t.Skip("requires a Unix process supervisor")
	}
	for _, terminalBeforeReply := range []bool{false, true} {
		t.Run(fmt.Sprintf("terminal before reply %t", terminalBeforeReply), func(t *testing.T) {
			const threadID, turnID = "thread", "exit-turn"
			step := fakeCodexTurnCompletedStep(t, turnID, validProposalJSON(""))
			if !terminalBeforeReply {
				step.AfterResponse, step.Notifications = step.Notifications, nil
			}
			step.CloseAfterResponse = true
			initialize := codexInitializeStep(t)
			initialize.Notifications = []fakeCodexNotification{{
				Method: codexNotificationTurnCompleted,
				Params: rawJSON(t, map[string]any{"turn": map[string]string{"status": "failed"}}),
			}}
			script := filepath.Join(t.TempDir(), "script.json")
			if err := os.WriteFile(script, rawJSON(t, fakeCodexScript{
				Steps: []fakeCodexStep{initialize, step},
			}), 0o600); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			proc, err := StartCodexAppServer(ctx, NewProcessSupervisor(), CodexProcessConfig{
				Argv: []string{os.Args[0]},
				Env:  append(os.Environ(), fakeCodexServerScriptEnvVar+"="+script),
			})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				_ = proc.Client.Close()
				_ = proc.forceStop(defaultCodexProcessReapTimeout)
			})
			var ackID string
			result, err := codexRunTurn(ctx, proc.Client, threadID, "work", codexTurnOptions{
				OnTurnStarted: func(id string) error { ackID = id; return nil },
			})
			if err != nil || ackID != turnID || result.TurnID != turnID || result.Proposal == nil {
				t.Fatalf("settlement after process exit: ack %q, terminal %q, error %v", ackID, result.TurnID, err)
			}
			if !proc.waitForExit(time.Second) {
				t.Fatal("app-server did not exit")
			}
		})
	}
}
