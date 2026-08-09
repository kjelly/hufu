package team

import (
	"context"
	"reflect"
	"testing"

	"github.com/kjelly/hufu/internal/agent"
)

func TestNearestModelNames(t *testing.T) {
	available := map[string]bool{
		"glm-5.2:cloud":             true,
		"glm-5.2:local":             true,
		"qwen3:8b":                  true,
		"minimax-m2.7:cloud":        true,
		"ollama/minimax-m2.7:cloud": true,
	}
	cases := []struct {
		name    string
		missing string
		want    []string
	}{
		{"missing tag colon", "glm-5.2cloud", []string{"glm-5.2:cloud", "glm-5.2:local"}},
		{"wrong tag", "qwen3:70b", []string{"qwen3:8b"}},
		{"prefixed available", "minimax-m2.7cloud", []string{"minimax-m2.7:cloud", "ollama/minimax-m2.7:cloud"}},
		{"no match", "llama9:1b", nil},
		{"too short", "g", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := nearestModelNames(tc.missing, available, 3)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("suggestions = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestModelAvailableOnProvider(t *testing.T) {
	available := map[string]bool{
		"ollama/minimax-m2.7:cloud": true,
		"qwen3:8b":                  true,
	}

	if !modelAvailableOnProvider("ollama/minimax-m2.7:cloud", "ollama", available) {
		t.Fatal("expected provider-prefixed model to match")
	}
	if !modelAvailableOnProvider("minimax-m2.7:cloud", "ollama", available) {
		t.Fatal("expected bare model to match provider-prefixed availability")
	}
	if modelAvailableOnProvider("llama3:latest", "ollama", available) {
		t.Fatal("unexpected match for missing model")
	}
}

func TestRunContinuesPastModelValidationWarning(t *testing.T) {
	workspace := t.TempDir()
	pm, err := agent.NewProviderManager("http://127.0.0.1:11434/v1", "", nil)
	if err != nil {
		t.Fatalf("failed to build provider manager: %v", err)
	}

	c := &Coordinator{
		session: &TeamSession{
			Config:    agent.TeamConfig{Name: "test"},
			Workspace: workspace,
			Agents:    map[string]*agent.AgentDef{},
		},
		providerManager: pm,
		taskTracker:     NewTaskTracker(),
		reportStatus:    func(StatusEvent) {},
	}
	c.validateModelsErr = context.DeadlineExceeded
	c.validateModelsOnce.Do(func() {})

	_, err = c.Run(context.Background(), "hello")
	if err == nil {
		t.Fatal("expected run to fail later because no coordinator model is configured")
	}
	if got := err.Error(); got == "" || got == context.DeadlineExceeded.Error() {
		t.Fatalf("run returned the validation warning instead of continuing: %v", err)
	}
}
