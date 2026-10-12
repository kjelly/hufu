//go:build unix

package processutil

import (
	"os/exec"
	"syscall"
	"time"
)

// ConfigureCommandReaping gives a context-bound command ownership of a process
// group and bounds Wait when descendants retain its output pipes. TERM gives
// supervisors and PTY owners a brief opportunity to clean up their children;
// KILL then enforces cancellation of the remaining group.
func ConfigureCommandReaping(cmd *exec.Cmd, waitDelay time.Duration) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM); err != nil {
			return cmd.Process.Kill()
		}
		time.Sleep(100 * time.Millisecond)
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		return nil
	}
	cmd.WaitDelay = waitDelay
}
