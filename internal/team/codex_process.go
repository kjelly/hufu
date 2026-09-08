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
}

// Handle exposes the underlying ProcessHandle for cancellation
// (Interrupt/TerminateTree/KillTree via the same ProcessSupervisor).
func (p *CodexAppServerProcess) Handle() ProcessHandle { return p.handle }

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

	return &CodexAppServerProcess{Client: client, sup: sup, handle: handle, stderr: stderrBuf}, nil
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
