//go:build linux || darwin
// +build linux darwin

package tools

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"charm.land/fantasy"
	"github.com/anomalyco/hufu/internal/utils"
)

const defaultBashTimeout = 120 * time.Second

const maxBashTimeout = 600 * time.Second

// commandReapDelay bounds how long Wait blocks after a timeout kill before
// giving up on the process and closing its pipes. Without it a child the
// kill could not reach (a root-owned sudo, EPERM) blocks Wait forever — a
// real run leaked a tool call for over two hours this way.
const commandReapDelay = 10 * time.Second

// outputDrainGrace bounds how long we give the stdout/stderr reader
// goroutines to drain already-buffered output before reaping the process.
//
// Cmd.Wait closes the pipes it handed us the moment it reaps the child (see
// the Cmd.StdoutPipe doc: "Wait will close the pipe after seeing the command
// exit ... it is incorrect to call Wait before all reads from the pipe have
// completed"). Calling Wait first and only then waiting on the readers — the
// previous ordering here — raced Wait's reap against the readers' first
// scheduler slot: under load (worse under -race) Wait could win and close
// the pipes before a single byte had been read, silently truncating a
// successful command's output to nothing.
//
// Simply flipping the order (always drain before reaping) trades that bug
// for a worse one: a command that intentionally backgrounds a long-running
// process (`server &`) exits its direct child immediately, but the
// backgrounded grandchild inherits the same pipe and can keep it open
// indefinitely, so draining unconditionally would block the tool call for
// the full timeout instead of returning right away. The data a normal
// command produced is already sitting in the kernel pipe buffer by the time
// its process exits, so a short grace window is enough for the readers to
// pick it up; past that window we assume an orphan is holding the pipe and
// reap anyway, letting Wait's own pipe-close unblock the readers with
// whatever they already captured — the same behavior this code had before.
const outputDrainGrace = 250 * time.Millisecond

// waitAndDrain reaps cmd while avoiding the truncation/hang tradeoff
// described above: it gives the reader goroutines tracked by wg up to
// outputDrainGrace to reach EOF on their own, then reaps regardless. wg must
// track only the goroutines reading cmd's stdout/stderr pipes.
func waitAndDrain(cmd *exec.Cmd, wg *sync.WaitGroup) error {
	drained := make(chan struct{})
	go func() {
		wg.Wait()
		close(drained)
	}()

	select {
	case <-drained:
	case <-time.After(outputDrainGrace):
	}

	waitErr := cmd.Wait()
	<-drained
	return waitErr
}

// configureCommandReaping makes a context-timeout kill reach the whole
// process tree, not just the direct child. exec.CommandContext's default
// cancel SIGKILLs only the child: grandchildren (e.g. a guestfish appliance)
// survive holding the output pipes, and a root-owned sudo child cannot be
// signalled at all. Preserves existing SysProcAttr fields if setNetNamespace
// was called first.
func configureCommandReaping(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
			// EPERM for root-owned children (sudo). Try the plain kill and
			// otherwise rely on WaitDelay to unblock Wait; the orphan keeps
			// running but the tool call returns.
			return cmd.Process.Kill()
		}
		return nil
	}
	cmd.WaitDelay = commandReapDelay
}

// runShellCommand runs name+args under a derived context with the given timeout,
// sets Dir and the SHELL env var, then collects stdout/stderr and builds a response.
// It is used by the bash and sudo tools.
func runShellCommand(ctx context.Context, timeout time.Duration, workDir string, networkBlock bool, name string, args []string, envReplacer func(env []string) []string) (fantasy.ToolResponse, error) {
	cmdCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(cmdCtx, name, args...)
	if workDir != "" {
		cmd.Dir = workDir
	}
	if networkBlock {
		if err := setNetNamespace(cmd); err != nil {
			return fantasy.NewTextErrorResponse(fmt.Sprintf("failed to set network namespace: %v", err)), nil
		}
	}
	configureCommandReaping(cmd)
	bashPath, err := exec.LookPath("bash")
	if err != nil {
		bashPath = "/bin/bash"
	}
	env := SanitizeSubprocessEnv(os.Environ())
	env = append(env, "SHELL="+bashPath)
	if envReplacer != nil {
		env = envReplacer(env)
	}
	cmd.Env = env

	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return fantasy.NewTextErrorResponse("failed to create stdout pipe"), nil
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return fantasy.NewTextErrorResponse("failed to create stderr pipe"), nil
	}
	if err := cmd.Start(); err != nil {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("failed to start command: %v", err)), nil
	}

	var wg sync.WaitGroup
	var stdout, stderr bytes.Buffer
	wg.Add(2)
	go func() { defer wg.Done(); _, _ = io.Copy(&stdout, stdoutPipe) }()
	go func() { defer wg.Done(); _, _ = io.Copy(&stderr, stderrPipe) }()

	waitErr := waitAndDrain(cmd, &wg)

	exitCode := 0
	if waitErr != nil {
		// Check the deadline before the exit error: the timeout kill makes
		// Wait report a plain signal death (exit -1), which would otherwise
		// masquerade as the command's own result.
		if cmdCtx.Err() == context.DeadlineExceeded {
			return fantasy.NewTextErrorResponse(timeoutResponseMessage(timeout, stdout.String(), stderr.String())), nil
		}
		if exitErr, ok := waitErr.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		}
	}
	return buildBashResponse(stdout.String(), stderr.String(), exitCode), nil
}

func runShellCommandRestricted(ctx context.Context, timeout time.Duration, workDir string, restrictedPath string, networkBlock bool, command string) (fantasy.ToolResponse, error) {
	cmdCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(cmdCtx, "bash", "-r", "-c", command)
	if workDir != "" {
		cmd.Dir = workDir
	}
	if networkBlock {
		if err := setNetNamespace(cmd); err != nil {
			return fantasy.NewTextErrorResponse(fmt.Sprintf("failed to set network namespace: %v", err)), nil
		}
	}
	configureCommandReaping(cmd)
	bashPath, err := exec.LookPath("bash")
	if err != nil {
		bashPath = "/bin/bash"
	}
	env := os.Environ()
	filtered := make([]string, 0, len(env))
	for _, e := range env {
		if !strings.HasPrefix(e, "PATH=") {
			filtered = append(filtered, e)
		}
	}
	pathVal := restrictedPath
	if pathVal == "" {
		pathVal = os.Getenv("PATH")
	}
	filtered = append(filtered, "PATH="+pathVal)
	filtered = append(filtered, "SHELL="+bashPath)
	cmd.Env = utils.SanitizeSubprocessEnv(filtered)

	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return fantasy.NewTextErrorResponse("failed to create stdout pipe"), nil
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return fantasy.NewTextErrorResponse("failed to create stderr pipe"), nil
	}
	if err := cmd.Start(); err != nil {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("failed to start command: %v", err)), nil
	}

	var wg sync.WaitGroup
	var stdout, stderr bytes.Buffer
	wg.Add(2)
	go func() { defer wg.Done(); _, _ = io.Copy(&stdout, stdoutPipe) }()
	go func() { defer wg.Done(); _, _ = io.Copy(&stderr, stderrPipe) }()

	waitErr := waitAndDrain(cmd, &wg)

	exitCode := 0
	if waitErr != nil {
		if cmdCtx.Err() == context.DeadlineExceeded {
			return fantasy.NewTextErrorResponse(timeoutResponseMessage(timeout, stdout.String(), stderr.String())), nil
		}
		if exitErr, ok := waitErr.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		}
	}
	return buildBashResponse(stdout.String(), stderr.String(), exitCode), nil
}

// runBashDirenv executes a bash command with the project's .envrc/.env
// environment loaded from the working directory, replacing the inherited
// environment with a minimal base plus the project env.
func runBashDirenv(ctx context.Context, timeout time.Duration, cfg ToolConfig, command string) (fantasy.ToolResponse, error) {
	if cfg.WorkDir == "" {
		return fantasy.NewTextErrorResponse("working_directory is required in direnv mode"), nil
	}
	projectEnv, err := LoadProjectEnv(cfg.WorkDir, true)
	if err != nil {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("failed to load project env: %v", err)), nil
	}
	return runShellCommand(ctx, timeout, cfg.WorkDir, cfg.NetworkBlock, "bash", []string{"-c", command}, func(env []string) []string {
		bashPath, _ := exec.LookPath("bash")
		if bashPath == "" {
			bashPath = "/bin/bash"
		}
		homeDir := os.Getenv("HOME")
		if homeDir == "" {
			homeDir = "/root"
		}
		usr := os.Getenv("USER")
		if usr == "" {
			usr = "unknown"
		}
		base := []string{"HOME=" + homeDir, "USER=" + usr}
		base = append(base, projectEnv...)
		base = append(base, "PATH="+os.Getenv("PATH"))
		base = append(base, "SHELL="+bashPath)
		return SanitizeSubprocessEnv(base)
	})
}

// SanitizeSubprocessEnv strips secret environment variables like HUFU_HMAC_SECRET from subprocess environments.
func SanitizeSubprocessEnv(env []string) []string {
	return utils.SanitizeSubprocessEnv(env)
}
