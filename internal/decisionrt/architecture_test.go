package decisionrt_test

import (
	"os/exec"
	"strings"
	"testing"
)

func TestCoreDependencyBoundary(t *testing.T) {
	command := exec.CommandContext(t.Context(), "go", "list", "-deps", "-f", "{{.ImportPath}}", ".")
	output, err := command.Output()
	if err != nil {
		t.Fatalf("go list: %v", err)
	}
	dependencies := make(map[string]struct{})
	for dependency := range strings.Lines(string(output)) {
		dependencies[strings.TrimSpace(dependency)] = struct{}{}
	}
	for _, forbidden := range []string{
		"github.com/kjelly/hufu/internal/team",
		"github.com/kjelly/hufu/internal/agent",
		"github.com/kjelly/hufu/internal/sidecar",
		"github.com/kjelly/hufu/cmd/hufu",
	} {
		if _, ok := dependencies[forbidden]; ok {
			t.Errorf("core dependency graph contains %s", forbidden)
		}
	}
}
