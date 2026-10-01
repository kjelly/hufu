package team

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
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

func TestValidateConfiguredModelsConfirmsModelsMissingFromTheList(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/v1/models":
			// Ollama lists only pulled models; cloud models are omitted.
			_, _ = writer.Write([]byte(`{"object":"list","data":[{"id":"minimax-m3:cloud"}]}`))
		case "/v1/models/glm-5.3-flash:cloud":
			_, _ = writer.Write([]byte(`{"id":"glm-5.3-flash","object":"model"}`))
		default:
			writer.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	tests := []struct {
		name    string
		model   string
		problem string
	}{
		{name: "listed", model: "ollama/minimax-m3:cloud"},
		{name: "unlisted cloud model", model: "ollama/glm-5.3-flash:cloud"},
		{name: "typo", model: "ollama/glm-5.3-flsh:cloud", problem: `model "ollama/glm-5.3-flsh:cloud" not found on provider "ollama"`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			session := &TeamSession{
				Workspace: t.TempDir(),
				Config:    agent.TeamConfig{Name: "models"},
				Agents:    map[string]*agent.AgentDef{"worker": {Name: "worker", Role: "worker", Generation: agent.GenerationParams{Model: test.model}}},
			}
			if err := session.SetCompatibilityWorkspaceScope(t.TempDir()); err != nil {
				t.Fatal(err)
			}
			c, err := NewCoordinator(session, server.URL+"/v1", "", nil, nil, nil, RoleModels{}, 1, false, false, false, nil, nil, nil, false, "", false, false, nil, false, false)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { c.CloseContextPreflight() })
			err = c.ValidateConfiguredModels(t.Context())
			if test.problem == "" {
				if err != nil {
					t.Fatalf("ValidateConfiguredModels = %v, want no problem", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), test.problem) {
				t.Fatalf("ValidateConfiguredModels = %v, want %q", err, test.problem)
			}
		})
	}
}
