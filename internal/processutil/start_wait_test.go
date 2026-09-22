package processutil

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestStartAndWaitReportsStartFailure(t *testing.T) {
	wait, err := StartAndWait(exec.Command(filepath.Join(t.TempDir(), "missing-executable")))
	if err == nil {
		t.Fatal("StartAndWait accepted a missing executable")
	}
	if wait != nil {
		t.Fatalf("wait channel = %#v, want nil after start failure", wait)
	}
}

func TestStartAndWaitOwnsProcessReaping(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=TestStartAndWaitHelperProcess")
	cmd.Env = append(os.Environ(), "HUFU_PROCESSUTIL_HELPER=1")
	wait, err := StartAndWait(cmd)
	if err != nil {
		t.Fatalf("StartAndWait: %v", err)
	}
	waitErr := <-wait
	exitErr, ok := errors.AsType[*exec.ExitError](waitErr)
	if !ok || exitErr.ExitCode() != 7 {
		t.Fatalf("wait error = %v, want exit code 7", waitErr)
	}
	if cmd.ProcessState == nil || !cmd.ProcessState.Exited() {
		t.Fatalf("process was not reaped: %#v", cmd.ProcessState)
	}
}

func TestStartAndWaitHelperProcess(t *testing.T) {
	if os.Getenv("HUFU_PROCESSUTIL_HELPER") != "1" {
		return
	}
	os.Exit(7)
}
