package team

import (
	"os"
	"testing"

	"github.com/kjelly/hufu/internal/tools"
)

// TestMain marks the package as unattended so tool permission tests exercise
// their explicit allowlists consistently on developer terminals and CI, where
// stdin is intentionally not a terminal. Production callers still set this
// state through Coordinator.SetUnattended.
//
// It also intercepts the fake-Codex-app-server re-exec: Phase 4's tests
// (codex_appserver_client_test.go et al.) spawn os.Args[0] — this same test
// binary — as their "fake app-server subprocess fixture" (spec.md §36 PR-08)
// with fakeCodexServerScriptEnvVar set to a script file path. This must run
// before flag/test machinery does anything else, since the child process's
// entire purpose is to behave as a JSON-RPC server on stdin/stdout, not to
// run the test suite.
func TestMain(m *testing.M) {
	if scriptPath := os.Getenv(fakeCodexServerScriptEnvVar); scriptPath != "" {
		runFakeCodexAppServer(scriptPath)
		os.Exit(0)
	}
	tools.SetProcessUnattended(true)
	os.Exit(m.Run())
}
