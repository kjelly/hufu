//go:build linux

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestProbeBinaryTUICompletionAndTerminalRestore(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: mise run test-integration")
	}
	binary := buildTestBinary(t)
	config, err := filepath.Abs("../../internal/improver/testdata/extraction/experiment.yaml")
	if err != nil {
		t.Fatal(err)
	}
	workspace := filepath.Join(processContractTempDir(t), "probe")
	process := startPTY(t, binary, []string{"improve", "probe", "run", "--config", config,
		"--workspace", workspace, "--tui"}, 30, 120)
	process.waitForOutputRaw(t, "\x1b[?1049h", 10*time.Second)
	process.resizePTY(t, 24, 80)
	process.waitForOutput(t, "simulation_only", 10*time.Second)
	process.writeInput(t, "q")
	if code := process.waitExit(t, 10*time.Second); code != 0 {
		t.Fatalf("TUI exit = %d", code)
	}
	process.waitForOutputRaw(t, "\x1b[?1049l", time.Second)
	state, err := unix.IoctlGetTermios(int(process.master.Fd()), unix.TCGETS)
	if err != nil || state.Lflag&unix.ICANON == 0 || state.Lflag&unix.ECHO == 0 {
		t.Fatalf("terminal flags were not restored: %+v, %v", state, err)
	}
}

func TestProbeBinaryTUICancelDrainsBeforeExit(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: mise run test-integration")
	}
	binary := buildTestBinary(t)
	root := t.TempDir()
	source := filepath.Join(root, "source")
	if err := os.CopyFS(source, os.DirFS("../../internal/improver/testdata/extraction")); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(source, "experiment.yaml")
	raw, err := os.ReadFile(config)
	if err != nil {
		t.Fatal(err)
	}
	text := strings.NewReplacer("replicates: 1", "replicates: 5", "pipeline_tokens: 60000", "pipeline_tokens: 200000",
		"pipeline_seconds: 300", "pipeline_seconds: 900", "max_probe_runs: 4", "max_probe_runs: 12").Replace(string(raw))
	if err := os.WriteFile(config, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	workspace := filepath.Join(root, "probe")
	process := startPTY(t, binary, []string{"improve", "probe", "run", "--config", config,
		"--workspace", workspace, "--tui"}, 30, 120)
	process.waitForOutputRaw(t, "\x1b[?1049h", 10*time.Second)
	process.writeInput(t, "\x03")
	if code := process.waitExit(t, 10*time.Second); code != 130 {
		t.Fatalf("cancel exit = %d; output: %s", code, stripPTYANSI(process.output.String()))
	}
	process.waitForOutputRaw(t, "\x1b[?1049l", time.Second)
	// A new writer can acquire the lease only after the process drained.
	// The readonly CLI must also be able to inspect its durable terminal state.
	_, err = executeProbeCommand(t, "show", "--experiment", "extraction_pair", "--workspace", workspace, "--json")
	if err != nil {
		t.Fatalf("cancelled experiment is not inspectable: %v", err)
	}
}
