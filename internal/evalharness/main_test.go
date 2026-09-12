package evalharness

import (
	"os"
	"testing"

	"github.com/kjelly/hufu/internal/providerproxy"
)

// TestMain intercepts the provider hard-abort boundary's re-exec: a real
// team.Coordinator run always re-execs os.Executable() with
// providerproxy.ChildArg to become its own provider proxy child (see
// internal/team/provider_boundary.go). In production os.Executable() is the
// hufu binary, which cmd/hufu/main.go handles the same way; under `go test`
// it is this test binary, so this package must handle the re-exec itself or
// every case that drives a real Coordinator.Run would fail with "provider
// proxy exited before readiness". This must run before flag/test machinery
// does anything else, mirroring internal/team/main_test.go's fake-app-server
// interception.
func TestMain(m *testing.M) {
	if len(os.Args) == 2 && os.Args[1] == providerproxy.ChildArg {
		os.Exit(providerproxy.RunChild(os.Stdin, os.Stdout))
	}
	os.Exit(m.Run())
}
