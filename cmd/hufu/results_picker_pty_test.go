//go:build linux

package main

import (
	"bytes"
	"context"
	"io"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/creack/pty"
	"golang.org/x/sys/unix"
)

type resultsPickerCapture struct {
	buffer  safeBuffer
	changed chan struct{}
}

func (c *resultsPickerCapture) Write(data []byte) (int, error) {
	n, err := c.buffer.Write(data)
	select {
	case c.changed <- struct{}{}:
	default:
	}
	return n, err
}

func TestResultsPickerBinaryPTYSelectionCancelAndRestore(t *testing.T) {
	binary := buildProcessContractBinary(t)
	workspace, _ := resultsTestFixture(t)
	for _, scenario := range []string{"final", "report", "task", "cancel", "interrupt"} {
		t.Run(scenario, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
			defer cancel()
			master, slave, err := pty.Open()
			if err != nil {
				t.Fatal(err)
			}
			defer master.Close()
			defer slave.Close()
			if err := pty.Setsize(master, &pty.Winsize{Rows: 24, Cols: 100}); err != nil {
				t.Fatal(err)
			}
			before, err := unix.IoctlGetTermios(int(slave.Fd()), unix.TCGETS)
			if err != nil {
				t.Fatal(err)
			}
			args := []string{"result", "pick", "--workspace", workspace, "--team", "review", "--all-branches", "--quiet"}
			if scenario == "report" {
				args = append(args, "--view", "report")
			}
			if scenario == "task" {
				args = append(args, "--tasks")
			}
			command := exec.CommandContext(ctx, binary, args...)
			home := t.TempDir()
			command.Dir = home
			command.Env = []string{"HOME=" + home, "XDG_CONFIG_HOME=" + filepath.Join(home, "config"), "HUFU_STATE_HOME=" + filepath.Join(home, "state"), "TERM=xterm-256color", "NO_COLOR=1"}
			var stdout bytes.Buffer
			command.Stdin, command.Stderr, command.Stdout = slave, slave, &stdout
			command.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
			if err := command.Start(); err != nil {
				t.Fatal(err)
			}
			waited := make(chan error, 1)
			go func() { waited <- command.Wait() }()
			capture := &resultsPickerCapture{changed: make(chan struct{}, 1)}
			drained := make(chan struct{})
			go func() { _, _ = io.Copy(capture, master); close(drained) }()
			defer func() {
				cancel()
				_ = slave.Close()
				_ = master.Close()
				select {
				case <-drained:
				case <-time.After(2 * time.Second):
					t.Error("PTY output reader did not stop")
				}
			}()
			waitText := func(want string) {
				t.Helper()
				for !strings.Contains(capture.buffer.String(), want) {
					select {
					case <-capture.changed:
					case <-ctx.Done():
						t.Fatalf("picker did not show %q: %s", want, capture.buffer.String())
					}
				}
			}
			waitText("Search saved results")
			if err := pty.Setsize(master, &pty.Winsize{Rows: 18, Cols: 80}); err != nil {
				t.Fatal(err)
			}
			if _, err := io.WriteString(master, "逾時"); err != nil {
				t.Fatal(err)
			}
			waitText("> 逾時")
			input := "\r"
			switch scenario {
			case "task":
				input = "\x1b[B\r"
			case "cancel":
				input = "\x1b"
			case "interrupt":
				input = "\x03"
			}
			if _, err := io.WriteString(master, input); err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-waited:
				if err != nil {
					t.Fatalf("picker exit: %v: %s", err, capture.buffer.String())
				}
			case <-ctx.Done():
				t.Fatalf("picker did not exit: %s", capture.buffer.String())
			}
			switch scenario {
			case "report":
				if !strings.HasPrefix(stdout.String(), "# Execution Report — review") || !strings.Contains(stdout.String(), "coordinator final response") || strings.Contains(stdout.String(), "OTHER_TEAM_SENTINEL") {
					t.Fatalf("wrong report output: %q", stdout.String())
				}
			case "final":
				if stdout.String() != "coordinator final response\n" {
					t.Fatalf("UI polluted stdout or wrong final: %q", stdout.String())
				}
			case "task":
				if !strings.Contains(stdout.String(), "保存的分析與解法 2") || strings.Contains(stdout.String(), "保存的分析與解法 3") {
					t.Fatalf("wrong selected task: %q", stdout.String())
				}
			default:
				if stdout.Len() != 0 {
					t.Fatalf("cancel printed a result: %q", stdout.String())
				}
			}
			after, err := unix.IoctlGetTermios(int(slave.Fd()), unix.TCGETS)
			if err != nil || *before != *after {
				t.Fatalf("terminal settings not restored: %v", err)
			}
			waitText("\x1b[?1049l")
		})
	}
	command := exec.CommandContext(t.Context(), binary, "results", "pick", "--workspace", workspace, "--team", "review")
	command.Env = []string{"HOME=" + t.TempDir(), "TERM=dumb"}
	output, err := command.CombinedOutput()
	if err == nil || !strings.Contains(string(output), "requires terminal stdin and stderr") {
		t.Fatalf("non-TTY picker did not fail: %v: %s", err, output)
	}
}
