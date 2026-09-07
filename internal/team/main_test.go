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
func TestMain(m *testing.M) {
	tools.SetProcessUnattended(true)
	os.Exit(m.Run())
}
