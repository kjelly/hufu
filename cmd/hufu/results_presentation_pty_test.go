//go:build linux

package main

import (
	"bytes"
	"context"
	"io"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/creack/pty"
	"github.com/kjelly/hufu/internal/utils"
)

func TestResultsPresentationBinaryTTYAndPipe(t *testing.T) {
	binary := buildTestBinary(t)
	workspace, _ := resultsTestFixture(t)
	for _, test := range []struct {
		name       string
		flags      []string
		noColorEnv string
		color      bool
		term       string
	}{
		{"plain", nil, "", false, ""},
		{"markdown", []string{"--render", "markdown", "--theme", "dark"}, "", true, ""},
		{"light", []string{"--render", "markdown", "--theme", "light"}, "", true, ""},
		{"no_color", []string{"--render", "markdown", "--no-color"}, "", false, ""},
		{"no_color_env", []string{"--render", "markdown"}, "1", false, ""},
		{"mono", []string{"--render", "markdown", "--theme", "mono"}, "", false, ""},
		{"display_plain", []string{"--render", "markdown", "--display-mode", "plain"}, "", false, ""},
		{"epaper", []string{"--render", "markdown", "--display-preset", "epaper"}, "", false, ""},
		{"dumb", []string{"--render", "markdown"}, "", false, "dumb"},
		{"ansi", []string{"--render", "markdown"}, "", true, "xterm"},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
			defer cancel()
			args := append([]string{"results", "--team", "review", "--workspace", workspace}, test.flags...)
			command := exec.CommandContext(ctx, binary, args...)
			command.Dir = t.TempDir()
			home := t.TempDir()
			command.Env = []string{"HOME=" + home, "XDG_CONFIG_HOME=" + filepath.Join(home, ".config"), "TERM=" + firstNonEmpty(test.term, "xterm-256color"), "NO_COLOR=" + test.noColorEnv}
			master, err := pty.StartWithSize(command, &pty.Winsize{Rows: 24, Cols: 60})
			if err != nil {
				t.Fatal(err)
			}
			defer master.Close()
			var output safeBuffer
			done := make(chan struct{})
			go func() { _, _ = io.Copy(&output, master); close(done) }()
			waitErr := command.Wait()
			select {
			case <-done:
			case <-ctx.Done():
				_ = master.Close()
				<-done
				t.Fatal("PTY output did not drain")
			}
			if waitErr != nil {
				t.Fatalf("exit: %v: %s", waitErr, output.String())
			}
			got := output.String()
			if !strings.Contains(utils.SanitizeTerminalText(got), "coordinator final response") || strings.Contains(got, "\x1b[") != test.color {
				t.Fatalf("TTY presentation mismatch: %q", got)
			}
			if test.name == "ansi" && (strings.Contains(got, "38;5;") || strings.Contains(got, "38;2;")) {
				t.Fatal("16-color terminal received unsupported colors")
			}
		})
	}
	command := exec.CommandContext(t.Context(), binary, "results", "--team", "review", "--workspace", workspace, "--render", "markdown", "--theme", "dark")
	command.Dir = t.TempDir()
	command.Env = []string{"HOME=" + t.TempDir(), "TERM=xterm-256color"}
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		t.Fatalf("pipe: %v: %s", err, stderr.String())
	}
	if stdout.String() != "coordinator final response\n" {
		t.Fatalf("pipe modified raw content: %q", stdout.String())
	}
	if !strings.Contains(stderr.String(), "Run: run-review") {
		t.Fatal("pipe lost source provenance")
	}
}
