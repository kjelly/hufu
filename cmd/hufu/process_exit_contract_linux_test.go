//go:build linux

package main

import (
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// The interactive contract needs the Linux PTY harness. Keep the JSON
// contract in process_exit_contract_test.go so Darwin still compiles and runs it.
func TestDefaultExploratoryResponseInteractiveProcessExitContract(t *testing.T) {
	binary := buildProcessContractBinary(t)
	var calls atomic.Int64
	server := newContractTextServer(t, &calls)
	defer server.Close()
	args := []string{
		"--default", "--provider-url", server.URL + "/v1", "--model", "test", "--coordinator-model", "test",
		"--context-window", "131072", "--max-rounds", "2", "--timeout", "10",
	}

	interactiveArgs := append(args, "--workspace", filepath.Join(t.TempDir(), "interactive-workspace"), "--display-mode", "plain", "answer the question")
	process := startPTY(t, binary, interactiveArgs, 30, 120)
	if code := process.waitExit(t, 30*time.Second); code != 0 {
		t.Fatalf("interactive exit code = %d, want 0; output=%q", code, truncateContractOutput([]byte(process.output.String())))
	}
	terminalOutput := process.output.String()
	if !strings.Contains(terminalOutput, "Exploratory response delivered; goal remains unverified") {
		t.Fatalf("interactive output lacks unverified warning: %q", truncateContractOutput([]byte(terminalOutput)))
	}
	if strings.Contains(terminalOutput, "Error: team \"default\" failed") {
		t.Fatalf("interactive delivery was reported as a team failure: %q", truncateContractOutput([]byte(terminalOutput)))
	}
}
