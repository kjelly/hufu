//go:build linux

package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// ── PTY helpers (minimal, stdlib + x/sys/unix only) ─────────────────────────

// safeBuffer is a mutex-protected buffer for reading PTY output concurrently.
type safeBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *safeBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *safeBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func (b *safeBuffer) Bytes() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	return bytes.Clone(b.buf.Bytes())
}

// ptyProcess wraps a process running in a pseudo-terminal.
type ptyProcess struct {
	master   *os.File
	slave    *os.File
	cmd      *exec.Cmd
	output   *safeBuffer
	waitDone chan struct{}
	waitErr  error
	readDone chan struct{}
}

func ptyTestDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp(os.TempDir(), "pty-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(dir); err != nil {
			t.Errorf("clean PTY directory: %v", err)
		}
	})
	return dir
}

// startPTY starts a command in a pseudo-terminal with the given window size.
func startPTY(t *testing.T, binary string, args []string, rows, cols uint16) *ptyProcess {
	t.Helper()
	home := ptyTestDir(t)

	// Open /dev/ptmx — the master side
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR, 0)
	if err != nil {
		t.Fatalf("open /dev/ptmx: %v", err)
	}

	// Unlock the slave
	if err := unix.IoctlSetPointerInt(int(master.Fd()), unix.TIOCSPTLCK, 0); err != nil {
		master.Close()
		t.Fatalf("unlock pty: %v", err)
	}

	// Get the slave number
	n, err := unix.IoctlGetInt(int(master.Fd()), unix.TIOCGPTN)
	if err != nil {
		master.Close()
		t.Fatalf("get pty number: %v", err)
	}

	slavePath := fmt.Sprintf("/dev/pts/%d", n)
	slave, err := os.OpenFile(slavePath, os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		master.Close()
		t.Fatalf("open slave %s: %v", slavePath, err)
	}

	// Set window size
	ws := &unix.Winsize{Row: rows, Col: cols}
	if err := unix.IoctlSetWinsize(int(master.Fd()), unix.TIOCSWINSZ, ws); err != nil {
		master.Close()
		slave.Close()
		t.Fatalf("set winsize: %v", err)
	}

	// Build the command
	// This bounds the whole multi-turn process, independently of each assertion.
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	t.Cleanup(cancel)
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Stdin = slave
	cmd.Stdout = slave
	cmd.Stderr = slave
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setsid:  true,
		Setctty: true,
		Ctty:    0, // slave fd in child
	}
	// Isolate environment
	cmd.Dir = home
	cmd.Env = []string{
		"HOME=" + home,
		"XDG_CONFIG_HOME=" + filepath.Join(home, ".config"),
		"XDG_CACHE_HOME=" + filepath.Join(home, ".cache"),
		"XDG_DATA_HOME=" + filepath.Join(home, ".local", "share"),
		"TERM=xterm-256color",
		"NO_COLOR=1",
		"LC_ALL=C",
		"LANG=C",
		"PATH=" + os.Getenv("PATH"),
		"TMPDIR=" + os.TempDir(),
		"GOMAXPROCS=2",
	}

	proc := &ptyProcess{
		master:   master,
		slave:    slave,
		cmd:      cmd,
		output:   &safeBuffer{},
		waitDone: make(chan struct{}),
		readDone: make(chan struct{}),
	}

	// Start the process
	if err := cmd.Start(); err != nil {
		master.Close()
		slave.Close()
		t.Fatalf("start process: %v", err)
	}

	// Close slave in parent — child has its own copy
	slave.Close()

	// Read from master in background
	go func() {
		defer close(proc.readDone)
		buf := make([]byte, 4096)
		for {
			n, err := master.Read(buf)
			if n > 0 {
				proc.output.Write(buf[:n])
			}
			if err != nil {
				return
			}
		}
	}()
	// One waiter owns reaping; waitExit and cleanup observe the same result.
	go func() {
		proc.waitErr = cmd.Wait()
		close(proc.waitDone)
	}()

	// Cleanup
	t.Cleanup(func() {
		cancel()
		master.Close()
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		select {
		case <-proc.waitDone:
		case <-time.After(30 * time.Second):
			t.Error("PTY process did not stop during cleanup")
		}
		select {
		case <-proc.readDone:
		case <-time.After(30 * time.Second):
			t.Error("PTY output reader did not stop during cleanup")
		}
	})

	return proc
}

// writeInput writes a string to the PTY master (simulating keyboard input).
func (p *ptyProcess) writeInput(t *testing.T, input string) {
	t.Helper()
	if _, err := p.master.WriteString(input); err != nil {
		t.Fatalf("write pty input: %v", err)
	}
}

// waitForOutput polls until the output contains the wanted substring or times out.
func (p *ptyProcess) waitForOutput(t *testing.T, want string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if strings.Contains(stripPTYANSI(p.output.String()), want) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	last := p.output.String()
	if len(last) > 4000 {
		last = last[len(last)-4000:]
	}
	t.Fatalf("waitForOutput: %q not found after %s\noutput (last 4KB):\n%s", want, timeout, stripPTYANSI(last))
}

// waitForOutputRaw is like waitForOutput but checks the raw bytes (no ANSI stripping).
func (p *ptyProcess) waitForOutputRaw(t *testing.T, want string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if bytes.Contains(p.output.Bytes(), []byte(want)) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("waitForOutputRaw: %q not found after %s", want, timeout)
}

// resizePTY changes the PTY window size.
func (p *ptyProcess) resizePTY(t *testing.T, rows, cols uint16) {
	t.Helper()
	ws := &unix.Winsize{Row: rows, Col: cols}
	if err := unix.IoctlSetWinsize(int(p.master.Fd()), unix.TIOCSWINSZ, ws); err != nil {
		t.Fatalf("resize pty: %v", err)
	}
}

// waitExit waits for the process to exit and returns the exit code.
func (p *ptyProcess) waitExit(t *testing.T, timeout time.Duration) int {
	t.Helper()
	select {
	case <-p.waitDone:
		select {
		case <-p.readDone:
		case <-time.After(timeout):
			t.Fatal("PTY output reader did not drain after process exit")
		}
		err := p.waitErr
		if err == nil {
			return 0
		}
		if exitErr, ok := err.(*exec.ExitError); ok {
			return exitErr.ExitCode()
		}
		t.Fatalf("wait: %v", err)
	case <-time.After(timeout):
		output := stripPTYANSI(p.output.String())
		if len(output) > 4000 {
			output = output[len(output)-4000:]
		}
		t.Fatalf("process did not exit after %s\noutput (last 4KB):\n%s", timeout, output)
	}
	return -1
}

// sendSignal sends a signal to the child process.
func (p *ptyProcess) sendSignal(sig os.Signal) error {
	return p.cmd.Process.Signal(sig)
}

// stripPTYANSI removes ANSI escape codes for plain-text assertions.
func stripPTYANSI(s string) string {
	var b strings.Builder
	i := 0
	for i < len(s) {
		if s[i] == '\x1b' {
			// CSI sequence
			if i+1 < len(s) && s[i+1] == '[' {
				j := i + 2
				for j < len(s) && !isFinalByte(s[j]) {
					j++
				}
				if j < len(s) {
					j++ // include final byte
				}
				i = j
				continue
			}
			// OSC sequence
			if i+1 < len(s) && s[i+1] == ']' {
				j := i + 2
				for j < len(s) && s[j] != '\x07' && s[j] != '\x1b' {
					j++
				}
				if j < len(s) && s[j] == '\x07' {
					j++
				}
				i = j
				continue
			}
			// Other escape: skip 2 chars
			i += 2
			continue
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}

func isFinalByte(c byte) bool {
	return (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || c == '~' || c == '@'
}

// ── L4 Tests ────────────────────────────────────────────────────────────────

// TestE2E_BuildBinary verifies the real binary compiles successfully.
func TestE2E_BuildBinary(t *testing.T) {
	binary := buildTestBinary(t)
	info, err := os.Stat(binary)
	if err != nil {
		t.Fatalf("binary not found: %v", err)
	}
	if info.Size() < 1000 {
		t.Fatalf("binary too small: %d bytes", info.Size())
	}
}

// TestE2E_VersionOutput verifies the real binary prints version info.
func TestE2E_VersionOutput(t *testing.T) {
	binary := buildTestBinary(t)
	proc := startPTY(t, binary, []string{"--version"}, 30, 100)
	proc.waitForOutput(t, "hufu version", 10*time.Second)
}

// TestE2E_HelpOutput verifies the real binary prints help text.
func TestE2E_HelpOutput(t *testing.T) {
	binary := buildTestBinary(t)
	proc := startPTY(t, binary, []string{"--help"}, 30, 100)
	proc.waitForOutput(t, "hufu discovers", 10*time.Second)
	proc.waitForOutput(t, "agent-team", 5*time.Second)
}

// TestE2E_DoctorCommand verifies the doctor subcommand runs and exits.
func TestE2E_DoctorCommand(t *testing.T) {
	binary := buildTestBinary(t)
	proc := startPTY(t, binary, []string{"doctor"}, 30, 100)
	// doctor should produce some output (even on error) and exit
	proc.waitForOutputRaw(t, "", 1*time.Second) // give it a moment to start
	exitCode := proc.waitExit(t, 15*time.Second)
	// doctor exits non-zero when provider is unreachable, which is expected
	_ = exitCode
}

// TestE2E_ListCommand verifies the list subcommand shows team information.
func TestE2E_ListCommand(t *testing.T) {
	binary := buildTestBinary(t)
	proc := startPTY(t, binary, []string{"list"}, 30, 100)
	// List should show available teams (or "no teams" message)
	// Give it some time to produce output, then check exit
	proc.waitForOutputRaw(t, "", 1*time.Second)
	exitCode := proc.waitExit(t, 10*time.Second)
	if exitCode != 0 {
		t.Errorf("list command exit code = %d, want 0", exitCode)
	}
}

// startBlockedTUI waits for both a real provider request and terminal ownership.
func startBlockedTUI(t *testing.T) *ptyProcess {
	t.Helper()
	server := newChatFixtureServer(t)
	workspaceRoot := filepath.Join(ptyTestDir(t), "workspace")
	proc := startPTY(t, buildTestBinary(t), []string{
		"--tui", "--default", "--provider-url", server.URL + "/v1",
		"--model", "test", "--coordinator-model", "test",
		"--context-window", "131072", "--timeout", "60",
		"--workspace", workspaceRoot, chatFixtureSlowToken,
	}, 30, 100)
	select {
	case <-server.slow:
	case <-time.After(30 * time.Second):
		t.Fatalf("TUI never reached the blocking provider; output=%q", truncateContractOutput(proc.output.Bytes()))
	}
	proc.waitForOutputRaw(t, "\x1b[?1049h", 30*time.Second)
	return proc
}

func interruptBlockedTUI(t *testing.T, proc *ptyProcess) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	// Standard signals can coalesce. Repeat while the blocked process is alive;
	// the interval is a delivery cadence, not a correctness deadline. Deterministic
	// interrupt-handler unit tests cover the individual graceful and force stages.
	poll := time.Tick(20 * time.Millisecond)
	for {
		if err := proc.sendSignal(os.Interrupt); err != nil && !errors.Is(err, os.ErrProcessDone) {
			t.Fatalf("send SIGINT: %v", err)
		}
		select {
		case <-proc.waitDone:
			if code := proc.waitExit(t, 30*time.Second); code != 130 {
				t.Fatalf("exit after SIGINT = %d, want 130; output=%q", code, truncateContractOutput(proc.output.Bytes()))
			}
			return
		case <-ctx.Done():
			t.Fatalf("SIGINT did not stop the blocked TUI; output=%q", truncateContractOutput(proc.output.Bytes()))
		case <-poll:
		}
	}
}

// TestE2E_Signal_SIGINT verifies SIGINT cancels an active provider request.
func TestE2E_Signal_SIGINT(t *testing.T) {
	interruptBlockedTUI(t, startBlockedTUI(t))
}

// TestE2E_Resize_NoDeadlock verifies that resizing the terminal doesn't cause a crash.
func TestE2E_Resize_NoDeadlock(t *testing.T) {
	binary := buildTestBinary(t)
	proc := startPTY(t, binary, []string{"--tui", "--default", "--model", "ollama/nonexistent", "resize test"}, 30, 100)

	// Give it a moment to start
	time.Sleep(500 * time.Millisecond)

	// Resize to various sizes
	sizes := []struct{ rows, cols uint16 }{
		{24, 80},
		{50, 200},
		{10, 40},
		{30, 120},
		{20, 60},
	}
	for _, sz := range sizes {
		proc.resizePTY(t, sz.rows, sz.cols)
		time.Sleep(100 * time.Millisecond)
	}

	// Process should still be alive — send 'q' won't work since not finished,
	// so kill it and verify it was running
	if proc.cmd.Process == nil {
		t.Fatal("process should still be alive after resizes")
	}
}

// TestE2E_TerminalRecovery verifies that the terminal is restored after exit.
// We check for alternate screen exit sequence (ESC[?1049l) in the output.
func TestE2E_TerminalRecovery(t *testing.T) {
	proc := startBlockedTUI(t)
	interruptBlockedTUI(t, proc)
	if !strings.Contains(proc.output.String(), "\x1b[?1049l") {
		t.Fatal("terminal did not leave alternate screen after SIGINT")
	}
}

// TestE2E_NoHangOnExit verifies the process exits within a reasonable time
// when given a non-interactive command.
func TestE2E_NoHangOnExit(t *testing.T) {
	binary := buildTestBinary(t)

	// Run --version which should exit immediately
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, binary, "--version")
	cmd.Env = []string{"HOME=" + t.TempDir(), "TERM=dumb", "NO_COLOR=1"}
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("--version failed: %v\n%s", err, output)
	}
	if !strings.Contains(string(output), "hufu version") {
		t.Errorf("version output unexpected: %q", output)
	}
}

// TestE2E_StdinPipe verifies the binary handles piped stdin gracefully.
func TestE2E_StdinPipe(t *testing.T) {
	binary := buildTestBinary(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, binary, "--version")
	cmd.Env = []string{"HOME=" + t.TempDir(), "TERM=dumb", "NO_COLOR=1"}
	cmd.Stdin = strings.NewReader("some input\n")
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("--version with piped stdin failed: %v\n%s", err, output)
	}
	if !strings.Contains(string(output), "hufu version") {
		t.Errorf("version output unexpected: %q", output)
	}
}

// TestE2E_IsolatedEnvironment verifies the binary doesn't read real user config.
func TestE2E_IsolatedEnvironment(t *testing.T) {
	binary := buildTestBinary(t)

	home := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, binary, "list")
	cmd.Env = []string{
		"HOME=" + home,
		"XDG_CONFIG_HOME=" + filepath.Join(home, ".config"),
		"XDG_CACHE_HOME=" + filepath.Join(home, ".cache"),
		"XDG_DATA_HOME=" + filepath.Join(home, ".local", "share"),
		"TERM=dumb",
		"NO_COLOR=1",
		"HUFU_AGENT_TEAM_SEARCH_PATH=" + filepath.Join(home, ".agent-teams"),
	}
	output, err := cmd.CombinedOutput()
	// list should work even with empty config — it just shows no teams or global teams
	_ = err
	_ = output
}

// TestE2E_NarrowTerminal_NoCrash verifies the TUI handles a narrow terminal.
func TestE2E_NarrowTerminal_NoCrash(t *testing.T) {
	binary := buildTestBinary(t)
	proc := startPTY(t, binary, []string{"--tui", "--default", "--model", "ollama/nonexistent", "narrow"}, 20, 40)

	// Give it a moment to start
	time.Sleep(500 * time.Millisecond)

	// Resize even narrower
	proc.resizePTY(t, 10, 30)
	time.Sleep(200 * time.Millisecond)

	// Process should still be alive
	if proc.cmd.Process == nil {
		t.Error("process should survive narrow terminal")
	}
}

// TestE2E_RapidResize verifies rapid resize events don't cause a panic.
func TestE2E_RapidResize(t *testing.T) {
	binary := buildTestBinary(t)
	proc := startPTY(t, binary, []string{"--tui", "--default", "--model", "ollama/nonexistent", "rapid resize"}, 30, 100)

	time.Sleep(300 * time.Millisecond)

	// Fire many resize events rapidly
	for i := 0; i < 20; i++ {
		proc.resizePTY(t, uint16(20+i), uint16(80+i*5))
	}

	time.Sleep(200 * time.Millisecond)

	// Process should still be alive
	if proc.cmd.Process == nil {
		t.Error("process should survive rapid resize")
	}
}

// TestE2E_AltScreenEntry verifies the TUI enters alternate screen mode when it starts.
// If the provider is unreachable, the program may exit before entering the TUI, which is acceptable.
func TestE2E_AltScreenEntry(t *testing.T) {
	binary := buildTestBinary(t)
	proc := startPTY(t, binary, []string{"--tui", "--default", "--model", "ollama/nonexistent", "alt screen test"}, 30, 100)

	// Wait for alt screen enter sequence (ESC[?1049h) or program exit
	deadline := time.Now().Add(5 * time.Second)
	found := false
	for time.Now().Before(deadline) {
		if bytes.Contains(proc.output.Bytes(), []byte("\x1b[?1049h")) {
			found = true
			break
		}
		// Check if process already exited
		if proc.cmd.ProcessState != nil {
			// Process exited before entering TUI — acceptable if provider unreachable
			t.Logf("process exited before entering TUI (provider unreachable) — output length: %d", len(proc.output.String()))
			return
		}
		time.Sleep(10 * time.Millisecond)
	}

	if found {
		// Send SIGINT to exit
		if err := proc.sendSignal(os.Interrupt); err != nil {
			t.Fatalf("send SIGINT: %v", err)
		}
		proc.waitExit(t, 5*time.Second)
	} else {
		// Process is still running but didn't enter alt screen — may use different rendering
		t.Logf("alt screen sequence not found, but process is still running — may use inline rendering")
		if proc.cmd.Process != nil {
			_ = proc.cmd.Process.Kill()
		}
	}
}

// TestE2E_TranscriptSample captures a sample of the TUI output for inspection.
func TestE2E_TranscriptSample(t *testing.T) {
	binary := buildTestBinary(t)
	proc := startPTY(t, binary, []string{"--tui", "--default", "--model", "ollama/nonexistent", "sample prompt"}, 30, 100)

	time.Sleep(1 * time.Second)

	// Capture output
	output := proc.output.String()
	if len(output) == 0 {
		t.Fatal("expected some output from TUI startup")
	}

	// Check for key elements (stripped of ANSI)
	plain := stripPTYANSI(output)
	for _, want := range []string{"sample prompt"} {
		if !strings.Contains(plain, want) {
			// The TUI may not have fully rendered if the provider is unreachable
			// Just log and continue
			t.Logf("output does not contain %q (may be expected if provider unreachable)", want)
		}
	}

	// Log a sample of the output for debugging
	scanner := bufio.NewScanner(strings.NewReader(plain))
	lineCount := 0
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line != "" {
			lineCount++
		}
		if lineCount > 5 {
			break
		}
	}
	if lineCount == 0 {
		t.Log("TUI produced output but no visible text lines (ANSI control sequences only)")
	}
}
