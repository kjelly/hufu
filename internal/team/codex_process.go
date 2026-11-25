package team

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"time"

	"github.com/kjelly/hufu/internal/utils"
)

// This file starts one codex app-server process and wires it to a
// CodexRPCClient (docs/architecture/execution-runtime.md). It builds on
// ProcessSupervisor
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

	sup             ProcessSupervisor
	handle          ProcessHandle
	stderr          *codexStderrCapture
	exited          chan struct{}
	closeChildPipes func()

	waitOnce sync.Once
	waitErr  error
}

// Handle exposes the underlying ProcessHandle for cancellation
// (Interrupt/TerminateTree/KillTree via the same ProcessSupervisor).
func (p *CodexAppServerProcess) Handle() ProcessHandle { return p.handle }

// codexProcessGroup is implemented by the native supervisor. Its child
// inherits its own copies of the files passed in ProcessSpec.
type codexProcessGroup interface {
	groupExists() (bool, error)
	waitAfterGroupKill(context.Context) error
}

// Wait reaps the direct process. The separate stdout reader owns wire EOF
// and may still have complete frames to dispatch after Wait returns.
func (p *CodexAppServerProcess) Wait() error {
	p.waitOnce.Do(func() {
		p.waitErr = p.handle.Wait()
		p.closeChildPipes()
	})
	return p.waitErr
}

// Stderr returns redacted process stderr within the configured capture bound.
func (p *CodexAppServerProcess) Stderr() string {
	diagnostics := p.Client.newDiagnostics(nil)
	text, truncated := p.stderr.snapshot()
	if truncated {
		diagnostics.discard()
	}
	diagnostics.learn(text)
	return diagnostics.secrets.RedactText(text)
}

type codexInitializationFailure struct {
	cause   error
	settled bool
	secrets *utils.SecretRedactionScope
}

func (e *codexInitializationFailure) Error() string { return e.cause.Error() }
func (e *codexInitializationFailure) Unwrap() error { return e.cause }

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
	stdinRead, stdinWrite, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("codex process: stdin pipe: %w", err)
	}
	stdoutRead, stdoutWrite, err := os.Pipe()
	if err != nil {
		_ = stdinRead.Close()
		_ = stdinWrite.Close()
		return nil, fmt.Errorf("codex process: stdout pipe: %w", err)
	}
	closeWire := func() {
		_ = stdinRead.Close()
		_ = stdinWrite.Close()
		_ = stdoutRead.Close()
		_ = stdoutWrite.Close()
	}
	stderr, err := startCodexStderrCapture(maxStderr)
	if err != nil {
		closeWire()
		return nil, fmt.Errorf("codex process: stderr capture: %w", err)
	}
	env := append([]string{}, cfg.Env...)
	handle, err := sup.Start(startCtx, ProcessSpec{
		Argv: cfg.Argv, Dir: cfg.Dir, Env: env,
		Stdin: stdinRead, Stdout: stdoutWrite, Stderr: stderr.writer,
	})
	if err != nil {
		closeWire()
		stderr.finish()
		return nil, fmt.Errorf("codex process: start: %w", err)
	}
	closeChildPipes := sync.OnceFunc(func() {
		_ = stdinRead.Close()
		_ = stdoutWrite.Close()
		_ = stderr.writer.Close()
	})
	if _, external := handle.(codexProcessGroup); external {
		closeChildPipes()
	}

	client := NewCodexRPCClient(stdinWrite, stdoutRead, cfg.MaxFrameBytes)
	client.diagnosticEnv = env
	client.knownSecrets = new(utils.SecretRedactionScope)
	client.knownSecrets.DiscoverEnvironment(env)
	client.stderr = stderr
	proc := &CodexAppServerProcess{
		Client: client, sup: sup, handle: handle, stderr: stderr,
		exited: make(chan struct{}), closeChildPipes: closeChildPipes,
	}
	go func() {
		_ = proc.Wait()
		close(proc.exited)
		// An orphan that kept stdout open must not hide leader exit
		// indefinitely. Close the read descriptor at the deadline so the
		// wire reader can publish its buffered frames before disconnecting.
		select {
		case <-client.Done():
		case <-time.After(time.Second):
		}
		_ = stdoutRead.Close()
		<-client.Done()
		_ = client.Close()
		proc.stderr.finish()
	}()
	var rejected *jsonRPCError
	if _, err := codexInitialize(startCtx, client, func(reply jsonRPCResponse) error {
		rejected = reply.Error
		return fmt.Errorf("initialize RPC rejected")
	}); err != nil {
		_ = client.Close()
		killErr := proc.forceStop(defaultCodexProcessReapTimeout)
		proc.stderr.finish()
		captured, truncated := proc.stderr.snapshot()
		diagnostics := client.newDiagnostics(nil)
		if killErr != nil || truncated {
			diagnostics.discard()
		}
		diagnostics.learn(captured)
		if rejected != nil {
			err = rejected.redacted(diagnostics)
		} else {
			diagnostics.learn(err.Error())
			err = redactCodexError(err, diagnostics.secrets)
		}
		captured = utils.TruncateRunes(diagnostics.secrets.RedactText(captured), codexFailureEvidenceMaxRunes)
		if killErr != nil {
			return nil, &codexInitializationFailure{
				cause:   fmt.Errorf("codex process: initialize: %w (stderr: %s; cleanup: %v)", err, captured, redactCodexError(killErr, diagnostics.secrets)),
				secrets: diagnostics.secrets,
			}
		}
		return nil, &codexInitializationFailure{
			cause:   fmt.Errorf("codex process: initialize: %w (stderr: %s)", err, captured),
			settled: true, secrets: diagnostics.secrets,
		}
	}

	return proc, nil
}

const defaultCodexProcessReapTimeout = 3 * time.Second

func (p *CodexAppServerProcess) waitForExit(grace time.Duration) bool {
	timer := time.NewTimer(grace)
	defer timer.Stop()
	select {
	case <-p.exited:
		return true
	case <-timer.C:
		return false
	}
}

func (p *CodexAppServerProcess) waitForTreeExit(grace time.Duration) bool {
	timer := time.NewTimer(grace)
	defer timer.Stop()
	select {
	case <-p.exited:
	case <-timer.C:
		return false
	}
	group, ok := p.handle.(codexProcessGroup)
	if !ok {
		return true
	}
	poll := time.NewTicker(10 * time.Millisecond)
	defer poll.Stop()
	for {
		exists, err := group.groupExists()
		if err != nil || !exists {
			return err == nil
		}
		select {
		case <-timer.C:
			return false
		case <-poll.C:
		}
	}
}

func (p *CodexAppServerProcess) forceStop(grace time.Duration) error {
	if err := p.sup.KillTree(context.Background(), p.handle); err != nil {
		return err
	}
	if !p.waitForExit(grace) {
		return fmt.Errorf("codex process did not exit after force-kill")
	}
	if group, ok := p.handle.(codexProcessGroup); ok {
		ctx, cancel := context.WithTimeout(context.Background(), grace)
		defer cancel()
		return group.waitAfterGroupKill(ctx)
	}
	return nil
}

// Own the stderr pipe explicitly so a direct process wait cannot bypass
// its draining. A descendent that retains the descriptor cannot prevent
// settlement, and a truncated or interrupted stream suppresses details.
type codexStderrCapture struct {
	*boundedBuffer
	reader, writer *os.File
	done           chan error
	finishOnce     sync.Once
}

func startCodexStderrCapture(limit int) (*codexStderrCapture, error) {
	reader, writer, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	capture := &codexStderrCapture{boundedBuffer: newBoundedBuffer(limit), reader: reader, writer: writer, done: make(chan error, 1)}
	go func() {
		_, err := io.Copy(capture.boundedBuffer, reader)
		capture.done <- err
	}()
	return capture, nil
}

func (s *codexStderrCapture) finish() {
	s.finishOnce.Do(func() {
		_ = s.writer.Close()
		timer := time.AfterFunc(time.Second, func() { _ = s.reader.Close() })
		err := <-s.done
		timer.Stop()
		_ = s.reader.Close()
		if err != nil {
			s.mu.Lock()
			s.truncated = true
			s.mu.Unlock()
		}
	})
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
	text, _ := b.snapshot()
	return text
}

func (b *boundedBuffer) snapshot() (string, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.truncated {
		return b.buf.String() + "\n...[truncated]", true
	}
	return b.buf.String(), false
}

func (b *boundedBuffer) Truncated() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.truncated
}
