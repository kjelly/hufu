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
)

const defaultBashTimeout = 120 * time.Second

const maxBashTimeout = 600 * time.Second

// commandReapDelay bounds how long Wait blocks after a timeout kill before
// giving up on the process and closing its pipes. Without it a child the
// kill could not reach (a root-owned sudo, EPERM) blocks Wait forever — a
// real run leaked a tool call for over two hours this way.
const commandReapDelay = 10 * time.Second

// configureCommandReaping makes a context-timeout kill reach the whole
// process tree, not just the direct child. exec.CommandContext's default
// cancel SIGKILLs only the child: grandchildren (e.g. a guestfish appliance)
// survive holding the output pipes, and a root-owned sudo child cannot be
// signalled at all. Must run after setNetNamespace, which replaces
// SysProcAttr.
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
	env := os.Environ()
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

	waitErr := cmd.Wait()
	wg.Wait()

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
	cmd.Env = filtered

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

	waitErr := cmd.Wait()
	wg.Wait()

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
		return base
	})
}
