//go:build linux

package main

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/kjelly/hufu/internal/team"
	workspacepkg "github.com/kjelly/hufu/internal/workspace"
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
	master    *os.File
	slave     *os.File
	cmd       *exec.Cmd
	output    *safeBuffer
	waitDone  chan struct{}
	waitErr   error
	readDone  chan struct{}
	workspace string
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

// ptyTestEnv excludes inherited provider credentials and user configuration.
func ptyTestEnv(home string) []string {
	return []string{
		"HOME=" + home,
		"HUFU_STATE_HOME=" + filepath.Join(home, "state"),
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
}

// startPTY starts a command in a pseudo-terminal with the given window size.
func startPTY(t *testing.T, binary string, args []string, rows, cols uint16) *ptyProcess {
	t.Helper()
	home := ptyTestDir(t)
	return startPTYInDir(t, binary, args, rows, cols, home, home)
}

func startPTYInDir(t *testing.T, binary string, args []string, rows, cols uint16, home, dir string) *ptyProcess {
	t.Helper()

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
	cmd.Dir = dir
	cmd.Env = ptyTestEnv(home)

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

// Output polling is bounded and fails as soon as the drained stream closes.
func (p *ptyProcess) waitForOutput(t *testing.T, want string, timeout time.Duration) {
	t.Helper()
	p.waitForOutputAfter(t, want, 0, timeout)
}

func (p *ptyProcess) waitForOutputAfter(t *testing.T, want string, offset int, timeout time.Duration) {
	t.Helper()
	p.waitForPTYOutput(t, want, offset, false, timeout)
}

func (p *ptyProcess) waitForOutputRaw(t *testing.T, want string, timeout time.Duration) {
	t.Helper()
	p.waitForPTYOutput(t, want, 0, true, timeout)
}

func (p *ptyProcess) waitForPTYOutput(t *testing.T, want string, offset int, raw bool, timeout time.Duration) {
	t.Helper()
	if want == "" {
		t.Fatal("empty PTY output assertion")
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	poll := time.NewTicker(10 * time.Millisecond)
	defer poll.Stop()
	matches := func() bool {
		output := p.output.String()[offset:]
		if !raw {
			output = stripPTYANSI(output)
		}
		return strings.Contains(output, want)
	}
	for {
		if matches() {
			return
		}
		select {
		case <-p.readDone:
			// Read the final snapshot after observing reader completion.
			if matches() {
				return
			}
			t.Fatalf("PTY output closed before %q; output=%q", want, truncateContractOutput(p.output.Bytes()))
		case <-timer.C:
			t.Fatalf("PTY output missing %q after %s; output=%q", want, timeout, truncateContractOutput(p.output.Bytes()))
		case <-poll.C:
		}
	}
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

// TestE2E_VersionOutput verifies the real binary prints version info.
func TestE2E_VersionOutput(t *testing.T) {
	binary := buildTestBinary(t)
	proc := startPTY(t, binary, []string{"--version"}, 30, 100)
	if code := proc.waitExit(t, 10*time.Second); code != 0 {
		t.Fatalf("version exit = %d", code)
	}
	proc.waitForOutput(t, "hufu version", time.Second)
}

// TestE2E_HelpOutput verifies the real binary prints help text.
func TestE2E_HelpOutput(t *testing.T) {
	binary := buildTestBinary(t)
	proc := startPTY(t, binary, []string{"--help"}, 30, 100)
	proc.waitForOutput(t, "hufu discovers", 10*time.Second)
	proc.waitForOutput(t, "agent-team", 5*time.Second)
	if code := proc.waitExit(t, 10*time.Second); code != 0 {
		t.Fatalf("help exit = %d", code)
	}
}

// TestE2E_DoctorCommand checks both successful and failed provider preflight.
func TestE2E_DoctorCommand(t *testing.T) {
	for _, tc := range []struct {
		name         string
		status, exit int
		want         string
	}{
		{"ready", http.StatusOK, 0, "Ready with warnings."},
		{"unavailable", http.StatusServiceUnavailable, 1, "provider returned HTTP 503"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/models" {
					t.Errorf("unexpected provider request: %s", r.URL.Path)
				}
				w.WriteHeader(tc.status)
				fmt.Fprint(w, `{"data":[{"id":"test"}]}`)
			}))
			t.Cleanup(server.Close)
			home := ptyTestDir(t)
			project := filepath.Join(home, "project")
			if err := os.MkdirAll(filepath.Join(project, ".git"), 0o700); err != nil {
				t.Fatal(err)
			}
			manager, err := workspacepkg.NewManager(filepath.Join(home, "state"))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := manager.Resolve(t.Context(), workspacepkg.ResolveRequest{StartDir: project, TeamName: "default", Mode: workspacepkg.ResolveEnsure}); err != nil {
				t.Fatal(err)
			}
			// doctor has no root-local model/provider flags; configure its isolated CWD.
			cfg := fmt.Sprintf("provider-url: %s/v1\nmodel: test\n", server.URL)
			if err := os.WriteFile(filepath.Join(project, "hufu.yaml"), []byte(cfg), 0o600); err != nil {
				t.Fatal(err)
			}
			proc := startPTYInDir(t, buildTestBinary(t), []string{"doctor"}, 30, 100, home, project)
			if code := proc.waitExit(t, 15*time.Second); code != tc.exit {
				t.Fatalf("doctor exit = %d, want %d; output=%s", code, tc.exit, proc.output.String())
			}
			proc.waitForOutput(t, "hufu doctor", time.Second)
			proc.waitForOutput(t, tc.want, time.Second)
		})
	}
}

// TestE2E_ListCommand checks the empty, isolated discovery result.
func TestE2E_ListCommand(t *testing.T) {
	proc := startPTY(t, buildTestBinary(t), []string{"list"}, 30, 100)
	if code := proc.waitExit(t, 10*time.Second); code != 0 {
		t.Fatalf("list exit = %d", code)
	}
	proc.waitForOutput(t, "No teams found", time.Second)
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
	proc.workspace = filepath.Join(workspaceRoot, "default")
	select {
	case <-server.slow:
	case <-proc.waitDone:
		t.Fatalf("TUI exited before provider readiness; output=%q", truncateContractOutput(proc.output.Bytes()))
	case <-time.After(30 * time.Second):
		t.Fatalf("TUI never reached the blocking provider; output=%q", truncateContractOutput(proc.output.Bytes()))
	}
	proc.waitForOutputRaw(t, "\x1b[?1049h", 30*time.Second)
	proc.waitForOutput(t, chatFixtureSlowToken, 30*time.Second)
	proc.assertRunning(t)
	return proc
}

func interruptBlockedTUI(t *testing.T, proc *ptyProcess) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	// Acknowledge the first stage before sending the second interrupt. Repeated
	// signals can kill the process after its handler is removed during teardown.
	if err := proc.sendSignal(os.Interrupt); err != nil {
		t.Fatalf("send first SIGINT: %v", err)
	}
	poll := time.NewTicker(10 * time.Millisecond)
	defer poll.Stop()
	for {
		session, _, err := team.LoadSessionReadOnly(proc.workspace)
		if err != nil {
			t.Fatalf("read wrap-up checkpoint: %v", err)
		}
		if session != nil && session.PendingWrapUp != nil && session.PendingWrapUp.EventDurable {
			break
		}
		select {
		case <-proc.waitDone:
			t.Fatalf("TUI exited before acknowledging wrap-up; output=%q", truncateContractOutput(proc.output.Bytes()))
		case <-ctx.Done():
			t.Fatalf("SIGINT did not persist wrap-up; output=%q", truncateContractOutput(proc.output.Bytes()))
		case <-poll.C:
		}
	}
	if err := proc.sendSignal(os.Interrupt); err != nil {
		t.Fatalf("send second SIGINT: %v", err)
	}
	deadline, _ := ctx.Deadline()
	if code := proc.waitExit(t, time.Until(deadline)); code != 130 {
		t.Fatalf("exit after SIGINT = %d, want 130; output=%q", code, truncateContractOutput(proc.output.Bytes()))
	}
}

// assertRunning observes the single Wait owner's synchronized completion signal.
func (p *ptyProcess) assertRunning(t *testing.T) {
	t.Helper()
	select {
	case <-p.waitDone:
		t.Fatalf("PTY process exited unexpectedly: %v; output=%q", p.waitErr, truncateContractOutput(p.output.Bytes()))
	default:
	}
}

// One real PTY lifecycle covers ready rendering, kernel SIGWINCH delivery,
// rapid/narrow resize, SIGINT and alternate-screen restoration in order.
func TestE2E_TUILifecycle(t *testing.T) {
	proc := startBlockedTUI(t)
	offset := len(proc.output.String())
	for i := range 20 {
		proc.resizePTY(t, uint16(20+i), uint16(80+i*5))
	}
	proc.resizePTY(t, 20, 40)
	proc.waitForOutputAfter(t, "Terminal too narrow", offset, 10*time.Second)
	proc.assertRunning(t)
	offset = len(proc.output.String())
	proc.resizePTY(t, 30, 100)
	proc.waitForOutputAfter(t, chatFixtureSlowToken, offset, 10*time.Second)
	proc.assertRunning(t)
	// interruptBlockedTUI asserts the durable wrap-up checkpoint and exit 130.
	interruptBlockedTUI(t, proc)
	output := proc.output.String()
	enter := strings.Index(output, "\x1b[?1049h")
	leave := strings.LastIndex(output, "\x1b[?1049l")
	if enter < 0 || leave <= enter {
		t.Fatal("terminal did not leave alternate screen after SIGINT")
	}
}

// Keep all three stdin transports: a PTY above, /dev/null here and a real pipe.
func TestE2E_NonInteractiveVersion(t *testing.T) {
	binary := buildTestBinary(t)
	for _, piped := range []bool{false, true} {
		t.Run(fmt.Sprintf("pipe=%t", piped), func(t *testing.T) {
			home := ptyTestDir(t)
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, binary, "--version")
			cmd.Dir, cmd.Env = home, ptyTestEnv(home)
			if piped {
				cmd.Stdin = strings.NewReader("some input\n")
			}
			output, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("--version failed: %v\n%s", err, output)
			}
			if !strings.Contains(string(output), "hufu version") {
				t.Fatalf("unexpected version: %q", output)
			}
		})
	}
}

// Sentinels in a synthetic user config and project config prove both roots
// are read in the positive control and absent from a fresh HOME/CWD.
func TestE2E_IsolatedEnvironment(t *testing.T) {
	binary := buildTestBinary(t)
	home, project := ptyTestDir(t), ptyTestDir(t)
	configDir := filepath.Join(home, ".config", "hufu")
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatal(err)
	}
	for path, name := range map[string]string{
		filepath.Join(configDir, "hufu.yaml"): "user-isolation-sentinel",
		filepath.Join(project, "hufu.yaml"):   "project-isolation-sentinel",
	} {
		cfg := fmt.Sprintf("profiles:\n  %s:\n    model: test\n", name)
		if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, isolated := range []bool{false, true} {
		t.Run(fmt.Sprintf("isolated=%t", isolated), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, binary, "config", "list-profiles", "--json")
			cmd.Dir, cmd.Env = project, ptyTestEnv(home)
			want := `["project-isolation-sentinel","user-isolation-sentinel"]`
			if isolated {
				clean := ptyTestDir(t)
				cmd.Dir, cmd.Env = clean, ptyTestEnv(clean)
				want = `[]`
			}
			output, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("config list-profiles failed: %v\n%s", err, output)
			}
			if strings.TrimSpace(string(output)) != want {
				t.Fatalf("profiles = %s, want %s", output, want)
			}
		})
	}
}
