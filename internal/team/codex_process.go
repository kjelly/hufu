package team

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os/exec"
	"sync"
	"time"
)

// This file starts one codex app-server process and wires it to a
// CodexRPCClient (docs/hufu-external-coding-agent-runtime-spec.md §13.2, one
// process per Hufu attempt per §13.1). It builds on ProcessSupervisor
// (Phase 3) rather than calling os/exec directly, so cancellation/cleanup
// share the same process-tree guarantees every other external-provider
// process gets.

const (
	defaultCodexStartupTimeout = 15 * time.Second
	defaultCodexMaxStderrBytes = 64 * 1024
)

// CodexProcessConfig configures one app-server launch.
type CodexProcessConfig struct {
	Argv           []string
	Dir            string
	Env            []string
	MaxFrameBytes  int
	MaxStderrBytes int
	StartupTimeout time.Duration
}

// CodexAppServerProcess is one started, initialized app-server process and
// its JSON-RPC client.
type CodexAppServerProcess struct {
	Client *CodexRPCClient

	sup    ProcessSupervisor
	handle ProcessHandle
	stderr *boundedBuffer
	stdout io.Closer

	waitOnce sync.Once
	waitErr  error
}

// Handle exposes the underlying ProcessHandle for cancellation
// (Interrupt/TerminateTree/KillTree via the same ProcessSupervisor).
func (p *CodexAppServerProcess) Handle() ProcessHandle { return p.handle }

// processRawWaiter is implemented by a platform's concrete ProcessHandle
// (unixProcessHandle) when it can reap process exit directly, without going
// through the generic ProcessHandle.Wait (== *exec.Cmd.Wait). This matters
// specifically for the Codex process: its stdin/stdout are bridged to the
// JSON-RPC client through an io.Pipe (see stdinRead/stdoutWrite above), and
// os/exec's own Cmd.Wait cannot return until the goroutine it spawns to
// bridge that pipe sees EOF — which only happens once CodexRPCClient.Close
// closes the pipe's write end. Using Cmd.Wait to *detect* a crash is
// therefore circular: detecting the crash is exactly what would let us
// decide to call Close. A raw process wait breaks the cycle by reaping the
// child directly (a plain waitpid, independent of any Go-level io.Pipe
// plumbing), so the crash monitor below can close the client — and thereby
// unblock the bridge goroutine — as soon as the process is actually gone,
// rather than needing the bridge already unblocked first.
type processRawWaiter interface {
	waitRaw() error
}

// Wait blocks until the process has exited, closing the stdout pipe
// afterward so the JSON-RPC client's read loop can observe EOF. It is safe
// to call concurrently (memoized via sync.Once) since both the background
// crash monitor below and stopCodexProcess's own cleanup need to observe
// process exit.
func (p *CodexAppServerProcess) Wait() error {
	p.waitOnce.Do(func() {
		if raw, ok := p.handle.(processRawWaiter); ok {
			p.waitErr = raw.waitRaw()
		} else {
			p.waitErr = p.handle.Wait()
		}
		if p.stdout != nil {
			_ = p.stdout.Close()
		}
	})
	return p.waitErr
}

// Stderr returns the process's captured stderr so far, bounded by
// MaxStderrBytes (§13.2 step 5, §17.2).
func (p *CodexAppServerProcess) Stderr() string { return p.stderr.String() }

// StartCodexAppServer performs §13.2's startup sequence: validate the
// executable exists, start it under the process supervisor with a sanitized
// environment and bounded stderr capture, then require a successful
// initialize handshake — all within StartupTimeout — before returning a
// client ready for thread operations.
func StartCodexAppServer(ctx context.Context, sup ProcessSupervisor, cfg CodexProcessConfig) (*CodexAppServerProcess, error) {
	if len(cfg.Argv) == 0 {
		return nil, fmt.Errorf("codex process: argv is required")
	}
	if sup == nil {
		return nil, fmt.Errorf("codex process: process supervisor is required")
	}
	if _, err := exec.LookPath(cfg.Argv[0]); err != nil {
		return nil, fmt.Errorf("codex process: executable %q not found: %w", cfg.Argv[0], err)
	}

	startupTimeout := cfg.StartupTimeout
	if startupTimeout <= 0 {
		startupTimeout = defaultCodexStartupTimeout
	}
	startCtx, cancel := context.WithTimeout(ctx, startupTimeout)
	defer cancel()

	maxStderr := cfg.MaxStderrBytes
	if maxStderr <= 0 {
		maxStderr = defaultCodexMaxStderrBytes
	}
	stderrBuf := newBoundedBuffer(maxStderr)

	stdinRead, stdinWrite := io.Pipe()
	stdoutRead, stdoutWrite := io.Pipe()

	handle, err := sup.Start(startCtx, ProcessSpec{
		Argv: cfg.Argv, Dir: cfg.Dir, Env: cfg.Env,
		Stdin: stdinRead, Stdout: stdoutWrite, Stderr: stderrBuf,
	})
	if err != nil {
		return nil, fmt.Errorf("codex process: start: %w", err)
	}

	client := NewCodexRPCClient(stdinWrite, stdoutRead, cfg.MaxFrameBytes)
	if _, err := codexInitialize(startCtx, client); err != nil {
		_ = sup.KillTree(context.Background(), handle)
		return nil, fmt.Errorf("codex process: initialize: %w (stderr: %s)", err, stderrBuf.String())
	}

	proc := &CodexAppServerProcess{Client: client, sup: sup, handle: handle, stderr: stderrBuf, stdout: stdoutWrite}
	// The process's real stdout is bridged into stdoutRead through an
	// io.Pipe (see the type doc above): os/exec's internal copy goroutine
	// never closes a non-*os.File Stdout writer once the child exits, so an
	// unexpected crash would otherwise leave the client's read loop blocked
	// forever waiting for EOF that never comes, and any in-flight Call
	// blocked forever waiting for a response that will never arrive. This
	// background monitor is what actually notices the crash: once the
	// process exits for any reason, it closes the client, which unblocks
	// every pending/future Call via its closeCh case (§16.3).
	go func() {
		_ = proc.Wait()
		_ = proc.Client.Close()
	}()

	return proc, nil
}

// boundedBuffer caps how much of a stream it retains, recording (but not
// growing without limit for) truncation — used for stderr capture
// (§13.2 step 5, §17.2's size-limit family of rules).
type boundedBuffer struct {
	mu        sync.Mutex
	buf       bytes.Buffer
	max       int
	truncated bool
}

func newBoundedBuffer(max int) *boundedBuffer {
	if max <= 0 {
		max = defaultCodexMaxStderrBytes
	}
	return &boundedBuffer{max: max}
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	remaining := b.max - b.buf.Len()
	if remaining <= 0 {
		b.truncated = true
		return len(p), nil
	}
	if len(p) > remaining {
		b.buf.Write(p[:remaining])
		b.truncated = true
		return len(p), nil
	}
	b.buf.Write(p)
	return len(p), nil
}

func (b *boundedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.truncated {
		return b.buf.String() + "\n...[truncated]"
	}
	return b.buf.String()
}
