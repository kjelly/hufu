//go:build !unix

package processutil

import (
	"os/exec"
	"time"
)

// ConfigureCommandReaping retains the platform's CommandContext cancellation
// and bounds pipe draining where Unix process groups are unavailable.
func ConfigureCommandReaping(cmd *exec.Cmd, waitDelay time.Duration) {
	cmd.WaitDelay = waitDelay
}
