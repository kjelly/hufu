package processutil

import (
	"os/exec"
	"time"
)

// RunContextCommand supervises a CommandContext with bounded cancellation and
// one owner for Start and Wait. Existing SysProcAttr fields are preserved.
func RunContextCommand(cmd *exec.Cmd, waitDelay time.Duration) error {
	ConfigureCommandReaping(cmd, waitDelay)
	wait, err := StartAndWait(cmd)
	if err != nil {
		return err
	}
	return <-wait
}
