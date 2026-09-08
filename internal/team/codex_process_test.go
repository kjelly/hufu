package team

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// These are supplementary (not among PR-08/09/10's named tests): codex_process.go
// ties ProcessSupervisor (Phase 3) to CodexRPCClient (this phase) and would
// otherwise have zero test coverage of its own.

// TestStartCodexAppServerInitializesOverRealProcess proves the full startup
// sequence — executable validation, ProcessSupervisor-managed pipes, and the
// initialize gate — succeeds end to end against the fake server fixture.
func TestStartCodexAppServerInitializesOverRealProcess(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("ProcessSupervisor has no Windows implementation yet (§16.2)")
	}
	dir := t.TempDir()
	scriptPath := filepath.Join(dir, "script.json")
	data, err := json.Marshal(fakeCodexScript{
		Steps: []fakeCodexStep{{Result: rawJSON(t, map[string]any{"protocol_version": "v2", "server_version": "fake-1.0"})}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(scriptPath, data, 0o644); err != nil {
		t.Fatal(err)
	}

	sup := NewProcessSupervisor()
	proc, err := StartCodexAppServer(context.Background(), sup, CodexProcessConfig{
		Argv:           []string{os.Args[0]},
		Env:            append(os.Environ(), fakeCodexServerScriptEnvVar+"="+scriptPath),
		StartupTimeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatalf("StartCodexAppServer: %v", err)
	}
	t.Cleanup(func() {
		_ = sup.KillTree(context.Background(), proc.Handle())
		_ = proc.Client.Close()
	})
}

// TestStartCodexAppServerRejectsMissingExecutable proves a nonexistent
// executable fails preflight before any process, pipe, or model interaction
// (§13.2 step 1).
func TestStartCodexAppServerRejectsMissingExecutable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("ProcessSupervisor has no Windows implementation yet (§16.2)")
	}
	sup := NewProcessSupervisor()
	_, err := StartCodexAppServer(context.Background(), sup, CodexProcessConfig{
		Argv: []string{"hufu-codex-definitely-does-not-exist"},
	})
	if err == nil {
		t.Fatal("expected a missing executable to fail before starting any process")
	}
}
