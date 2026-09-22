// Package processutil provides shared subprocess lifecycle helpers.
package processutil

import "os/exec"

// StartAndWait starts cmd and makes one goroutine the sole owner of both Start
// and Wait. On Linux that goroutine stays on its creating OS thread until the
// child is reaped, so SysProcAttr.Pdeathsig cannot fire merely because the Go
// runtime retires the creator thread.
func StartAndWait(cmd *exec.Cmd) (<-chan error, error) {
	started := make(chan error, 1)
	wait := make(chan error, 1)
	go func() {
		unlock := lockProcessParentThread()
		defer unlock()
		if err := cmd.Start(); err != nil {
			started <- err
			return
		}
		started <- nil
		wait <- cmd.Wait()
	}()
	if err := <-started; err != nil {
		return nil, err
	}
	return wait, nil
}
