//go:build linux || darwin

package tools

import (
	"os"
	"testing"

	"github.com/creack/pty"
)

func TestDetectInteractiveEnvironmentRequiresTerminal(t *testing.T) {
	for _, name := range CIEnvVars {
		t.Setenv(name, "")
	}
	previous := os.Stdin
	t.Cleanup(func() { os.Stdin = previous })

	null, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = null.Close() })
	file, err := os.CreateTemp(t.TempDir(), "stdin")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = file.Close() })
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reader.Close(); _ = writer.Close() })
	master, terminal, err := pty.Open()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = master.Close(); _ = terminal.Close() })

	for _, tc := range []struct {
		name  string
		stdin *os.File
		want  bool
	}{
		{"null device", null, false},
		{"regular file", file, false},
		{"pipe", reader, false},
		{"terminal", terminal, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			os.Stdin = tc.stdin
			if got := detectInteractiveEnvironment(); got != tc.want {
				t.Fatalf("interactive = %v, want %v", got, tc.want)
			}
		})
	}
	for _, name := range CIEnvVars {
		t.Run(name, func(t *testing.T) {
			t.Setenv(name, "true")
			os.Stdin = terminal
			if detectInteractiveEnvironment() {
				t.Fatal("CI must remain non-interactive even with a terminal")
			}
		})
	}
}
