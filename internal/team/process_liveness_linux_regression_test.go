//go:build linux

package team

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"
)

// These direct children stay waitable until Cleanup. This reproduces the
// zombie state without relying on PID 1 and leaves no new orphan zombies.
func startLivenessTestProcess(t *testing.T, pgid int) *exec.Cmd {
	t.Helper()
	cmd := exec.Command("sleep", "30")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pgid: pgid}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	return cmd
}

func waitForLivenessTestState(t *testing.T, pid int, want string) {
	t.Helper()
	if !waitUntil(t, time.Second, func() bool {
		state, _, err := readTestProcessStat(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
		return err == nil && state == want
	}) {
		t.Fatalf("process %d did not reach state %s", pid, want)
	}
}

func TestProcessLivenessIgnoresZombies(t *testing.T) {
	for _, id := range []int{0, -1} {
		if processAlive(id) || processGroupAlive(id) {
			t.Fatalf("invalid process identity %d reported alive", id)
		}
	}
	t.Run("zombie-only group", func(t *testing.T) {
		cmd := startLivenessTestProcess(t, 0)
		pid := cmd.Process.Pid
		if !processAlive(pid) || !processGroupAlive(pid) {
			t.Fatal("live leader was not detected")
		}
		if err := cmd.Process.Kill(); err != nil {
			t.Fatal(err)
		}
		waitForLivenessTestState(t, pid, "Z")
		if err := syscall.Kill(pid, 0); err != nil {
			t.Fatalf("zombie PID no longer signalable: %v", err)
		}
		if !isPIDAlive(pid) || !terminalProcessGroupAlive(pid) {
			t.Fatal("signal-based identity checks no longer see the unreaped zombie")
		}
		if processAlive(pid) || processGroupAlive(pid) {
			t.Fatal("unreaped zombie was mistaken for a live process or group")
		}
	})
	t.Run("live member with zombie leader", func(t *testing.T) {
		leader := startLivenessTestProcess(t, 0)
		pgid := leader.Process.Pid
		member := startLivenessTestProcess(t, pgid)
		if err := leader.Process.Kill(); err != nil {
			t.Fatal(err)
		}
		waitForLivenessTestState(t, pgid, "Z")
		if processAlive(pgid) || !processAlive(member.Process.Pid) || !processGroupAlive(pgid) {
			t.Fatal("live group member was hidden by a zombie leader")
		}
		if err := member.Process.Signal(syscall.SIGSTOP); err != nil {
			t.Fatal(err)
		}
		waitForLivenessTestState(t, member.Process.Pid, "T")
		if !processAlive(member.Process.Pid) || !processGroupAlive(pgid) {
			t.Fatal("stopped group member must still count as alive")
		}
		if err := member.Process.Kill(); err != nil {
			t.Fatal(err)
		}
		waitForLivenessTestState(t, member.Process.Pid, "Z")
		if processAlive(member.Process.Pid) || processGroupAlive(pgid) {
			t.Fatal("group containing only zombies was mistaken for a live group")
		}
	})
}

func TestProcessGroupLivenessProcObservations(t *testing.T) {
	for _, tc := range []struct {
		name  string
		stats []string
		alive bool
	}{
		{name: "empty"},
		{name: "zombies", stats: []string{"42 (leader) Z 1 42", "43 (child) Z 1 42"}},
		{name: "running leader", stats: []string{"42 (leader) R 1 42"}, alive: true},
		{name: "sleeping member", stats: []string{"42 (leader) Z 1 42", "43 (child) S 1 42"}, alive: true},
		{name: "stopped member", stats: []string{"43 (child) T 1 42"}, alive: true},
		{name: "uninterruptible member", stats: []string{"43 (child) D 1 42"}, alive: true},
		{name: "other group", stats: []string{"43 (child) R 1 7"}},
		{name: "complex comm", stats: []string{"43 (a name ) with (parens)) Z 1 42"}},
		{name: "missing parentheses", stats: []string{"invalid"}, alive: true},
		{name: "short stat", stats: []string{"43 (child) Z"}, alive: true},
		{name: "invalid state", stats: []string{"43 (child) ZZ 1 42"}, alive: true},
		{name: "invalid group", stats: []string{"43 (child) Z 1 invalid"}, alive: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			for i, stat := range tc.stats {
				dir := filepath.Join(root, strconv.Itoa(42+i))
				if err := os.Mkdir(dir, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, "stat"), []byte(stat), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			// A non-PID entry and a process that vanished during the scan.
			for _, name := range []string{"self", "999"} {
				if err := os.Mkdir(filepath.Join(root, name), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			if got := testProcessGroupAliveInProc(root, 42); got != tc.alive {
				t.Fatalf("group alive = %v, want %v", got, tc.alive)
			}
		})
	}
	t.Run("observation errors", func(t *testing.T) {
		root := t.TempDir()
		if !testProcessGroupAliveInProc(filepath.Join(root, "missing"), 42) {
			t.Fatal("unavailable proc root hid a possible leak")
		}
		// Reading a directory as a stat file reliably fails even as root.
		if err := os.MkdirAll(filepath.Join(root, "42", "stat"), 0o700); err != nil {
			t.Fatal(err)
		}
		if !testProcessGroupAliveInProc(root, 42) {
			t.Fatal("unreadable proc stat hid a possible leak")
		}
	})
}

func TestReadProcessLivenessStat(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stat")
	if err := os.WriteFile(path, []byte("42 (name with ) and (parens)) S 1 123 0"), 0o600); err != nil {
		t.Fatal(err)
	}
	state, pgid, err := readTestProcessStat(path)
	if err != nil || state != "S" || pgid != 123 {
		t.Fatalf("proc stat = (%q, %d, %v), want (S, 123, nil)", state, pgid, err)
	}
}
