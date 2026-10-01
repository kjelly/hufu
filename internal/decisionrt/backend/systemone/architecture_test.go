package systemone_test

import (
	"os/exec"
	"strings"
	"testing"
)

// TestDependencyBoundary keeps the adapter on the standard library and the
// provider-neutral decisionrt core.
func TestDependencyBoundary(t *testing.T) {
	command := exec.CommandContext(t.Context(), "go", "list", "-f", `{{join .Imports "\n"}}`, ".")
	output, err := command.Output()
	if err != nil {
		t.Fatalf("go list: %v", err)
	}
	for imported := range strings.Lines(string(output)) {
		imported = strings.TrimSpace(imported)
		if strings.Contains(imported, ".") && imported != "github.com/kjelly/hufu/internal/decisionrt" {
			t.Errorf("systemone imports %s", imported)
		}
	}
}
