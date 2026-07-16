package tools

import (
	"os"
	"testing"
)

// TestMain pins the interactivity decision to "interactive" for the whole
// package. These tests exercise allowlist/permission semantics, not CI
// detection, and must behave identically on a developer TTY and on a CI
// runner — where CI env vars (CI, GITHUB_ACTIONS, ...) would otherwise force
// every permission check into the non-interactive deny branch. Tests that
// want the non-interactive branch can override the pin and restore it.
func TestMain(m *testing.M) {
	interactiveEnvPinned.Store(true)
	os.Exit(m.Run())
}
