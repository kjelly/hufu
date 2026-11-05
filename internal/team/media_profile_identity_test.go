package team

import (
	"testing"

	"github.com/kjelly/hufu/internal/execution"
)

func TestMediaProfileMatchesTargetPreservesProviderNamespace(t *testing.T) {
	for _, test := range []struct {
		name, profile, backend, model string
		matches                       bool
	}{
		{"Ollama leaf", "vision", "ollama", "vision", true},
		{"Ollama qualified", "ollama/vision", "ollama", "vision", true},
		{"legacy local selector", "local/vision", "ollama", "vision", true},
		{"namespaced Ollama leaf", "library/vision", "ollama", "library/vision", true},
		{"named backend qualified", "fixture/vision", "fixture", "vision", true},
		{"named backend cannot use leaf", "vision", "fixture", "vision", false},
		{"other provider namespace", "other/vision", "fixture", "vision", false},
		{"other model", "other", "ollama", "vision", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := mediaProfileMatchesTarget(test.profile, execution.ExecutionTarget{Backend: test.backend, Model: test.model}); got != test.matches {
				t.Fatalf("profile target match=%t want=%t", got, test.matches)
			}
		})
	}
}
