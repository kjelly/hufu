//go:build !windows

package team

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"syscall"
)

// unixProcessSupervisor uses a dedicated process group per §16.2's Unix
// recommendation: Setpgid on start makes the child its own group leader, so
// signaling -pgid reaches every descendant, not only the direct child.
type unixProcessSupervisor struct{}

func newPlatformProcessSupervisor() ProcessSupervisor { return unixProcessSupervisor{} }

type unixProcessHandle struct {
	cmd  *exec.Cmd
	pgid int
}

func (h *unixProcessHandle) Pid() int { return h.cmd.Process.Pid }

func (h *unixProcessHandle) Wait() error { return h.cmd.Wait() }

// waitRaw reaps the process directly (a plain wait4/waitid on the pid),
// bypassing *exec.Cmd.Wait's join on any bridging io-copy goroutines it
// spawned for non-*os.File Stdin/Stdout. See codex_process.go's
// processRawWaiter doc comment for why this matters.
func (h *unixProcessHandle) waitRaw() error {
	_, err := h.cmd.Process.Wait()
	return err
}

func (unixProcessSupervisor) Start(_ context.Context, spec ProcessSpec) (ProcessHandle, error) {
	if len(spec.Argv) == 0 {
		return nil, fmt.Errorf("process supervisor: argv is required")
	}
	cmd := exec.Command(spec.Argv[0], spec.Argv[1:]...)
	cmd.Dir = spec.Dir
	cmd.Env = spec.Env
	cmd.Stdin = spec.Stdin
	cmd.Stdout = spec.Stdout
	cmd.Stderr = spec.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("process supervisor: start: %w", err)
	}
	return &unixProcessHandle{cmd: cmd, pgid: cmd.Process.Pid}, nil
}

func (unixProcessSupervisor) Interrupt(_ context.Context, handle ProcessHandle) error {
	return signalProcessTree(handle, syscall.SIGINT)
}

func (unixProcessSupervisor) TerminateTree(_ context.Context, handle ProcessHandle) error {
	return signalProcessTree(handle, syscall.SIGTERM)
}

func (unixProcessSupervisor) KillTree(_ context.Context, handle ProcessHandle) error {
	return signalProcessTree(handle, syscall.SIGKILL)
}

// signalProcessTree delegates to the existing signalProcessGroup helper
// (terminal_session.go), which already implements "signal -pgid" correctly
// for this codebase's PTY/terminal session process management.
func signalProcessTree(handle ProcessHandle, sig syscall.Signal) error {
	h, ok := handle.(*unixProcessHandle)
	if !ok || h == nil {
		return fmt.Errorf("process supervisor: invalid process handle")
	}
	if err := signalProcessGroup(h.pgid, sig); err != nil && !errors.Is(err, syscall.ESRCH) {
		return fmt.Errorf("process supervisor: signal %v to process group %d: %w", sig, h.pgid, err)
	}
	return nil
}
