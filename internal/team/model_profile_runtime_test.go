package team

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/config"
	"github.com/kjelly/hufu/internal/modelprofile"
	"github.com/kjelly/hufu/internal/providerintrospection"
)

type auxiliaryProfileIntrospector struct{}

func (auxiliaryProfileIntrospector) InspectModel(context.Context, providerintrospection.ProviderRef, string) (providerintrospection.RuntimeModelInfo, error) {
	return providerintrospection.RuntimeModelInfo{ConfiguredContext: 32_768, MaxOutputTokens: 256}, nil
}

func TestAdmissionContextForAuxiliaryUsesConfiguredTeamMaxOutput(t *testing.T) {
	providerManager, err := agent.NewProviderManager("http://127.0.0.1:11434/v1", "", nil)
	if err != nil {
		t.Fatalf("NewProviderManager failed: %v", err)
	}
	profileRuntime := &ModelProfileRuntime{
		manager: providerManager,
		resolver: modelprofile.NewRuntimeResolver(func(providerintrospection.ProviderRef) providerintrospection.ModelIntrospector {
			return auxiliaryProfileIntrospector{}
		}, modelprofile.ProfileCacheOptions{}),
	}
	c := &Coordinator{
		providerManager:     providerManager,
		modelProfileRuntime: profileRuntime,
		session: &TeamSession{Config: agent.TeamConfig{
			Generation: agent.GenerationParams{MaxTokens: "4096"},
		}},
	}

	bound := c.admissionContextFor(t.Context(), "auxiliary-model", nil)
	if bound.MaxOutputTokens != 4096 {
		t.Fatalf("auxiliary admission max output = %d, want configured team value 4096 (provider advertised 256)", bound.MaxOutputTokens)
	}
}

func TestTaskSelectedModelUsesWireModelAndBoundAdmissionProfile(t *testing.T) {
	const (
		canonicalModel = "canonical-agent-model"
		selectedModel  = "selected-task-model"
	)
	var wireModel string
	provider := newIPv4TestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/models":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data": []map[string]any{{
					"id": selectedModel, "context_length": 32_768, "max_output_tokens": 1_024,
				}},
			})
		case "/v1/chat/completions":
			var request struct {
				Model string `json:"model"`
			}
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			wireModel = request.Model
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": "selected-response", "object": "chat.completion", "created": 1,
				"model": selectedModel,
				"choices": []map[string]any{{
					"index":         0,
					"message":       map[string]string{"role": "assistant", "content": "selected response"},
					"finish_reason": "stop",
				}},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer provider.Close()

	providerManager, err := agent.NewProviderManager(provider.URL+"/v1", "selected-secret", map[string]config.ProviderConfig{
		"local": {IntrospectionType: "openai-compatible"},
	})
	if err != nil {
		t.Fatal(err)
	}
	workspace := t.TempDir()
	def := &agent.AgentDef{
		Name: "worker", Role: "worker", System: "worker system",
		Generation: agent.GenerationParams{Model: canonicalModel},
	}
	session := &TeamSession{
		Dir: workspace, Workspace: workspace,
		Config: agent.TeamConfig{Name: "selected-model-lifecycle"},
		Agents: map[string]*agent.AgentDef{"worker": def},
	}
	c := &Coordinator{
		providerManager:     providerManager,
		modelProfileRuntime: NewModelProfileRuntime(providerManager, false),
		modelList:           []config.ModelEntry{{ID: selectedModel}},
		session:             session,
		taskTracker:         NewTaskTracker(),
		projectDir:          workspace,
		reportStatus:        func(StatusEvent) {},
		coreTools:           nil,
	}
	item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "use selected model"}})[0]
	task := TaskDef{
		Agent: "worker", Goal: "use selected model", Model: selectedModel,
		Execution: ExecutionContract{RequiresResult: true},
	}
	created, _, err := c.createTaskAgentWithResultTool(t.Context(), def, "", task, item.ID)
	if err != nil {
		t.Fatal(err)
	}
	result, err := agent.RunAgent(t.Context(), created, "perform selected-model work")
	if err != nil {
		t.Fatal(err)
	}
	if result != "selected response" {
		t.Fatalf("agent result = %q, want selected response", result)
	}
	if wireModel != selectedModel {
		t.Fatalf("wire model = %q, want selected model %q", wireModel, selectedModel)
	}
	if def.Generation.Model != canonicalModel {
		t.Fatalf("canonical agent model mutated to %q", def.Generation.Model)
	}
	bound := c.admissionContextFor(t.Context(), selectedModel, def)
	if bound.ModelID != selectedModel || bound.ContextWindow != 32_768 || bound.MaxOutputTokens != 1_024 {
		t.Fatalf("selected admission context = %#v, want model/profile for %q", bound, selectedModel)
	}
	if bound.ProviderIdentity != "local" || bound.ProviderBaseURL == "" || !bound.Bound {
		t.Fatalf("selected admission context is not provider-bound: %#v", bound)
	}
}

func TestModelContextSpecForInvocation(t *testing.T) {
	c := &Coordinator{}
	def := &agent.AgentDef{Generation: agent.GenerationParams{MaxTokens: "2048"}}

	t.Run("bound", func(t *testing.T) {
		want := ModelContextSpec{
			ModelID:             "bound-model",
			ContextWindow:       16_384,
			ContextWindowSource: "provider_metadata",
			MaxOutputTokens:     1_024,
			SafetyMarginTokens:  512,
		}
		ctx := withProviderBoundInvocationContext(t.Context(), providerBoundInvocationContext{
			ModelID:      "bound-model",
			ModelContext: want,
		})

		if got := c.modelContextSpecForInvocation(ctx, "bound-model", def); got != want {
			t.Fatalf("bound invocation model context = %#v, want %#v", got, want)
		}
	})

	t.Run("unbound", func(t *testing.T) {
		modelID := "unbound-model"
		want := globalRegistry.GetSpec(modelID).WithEffectiveMaxOutputTokens(2048)

		if got := c.modelContextSpecForInvocation(t.Context(), modelID, def); got != want {
			t.Fatalf("unbound invocation model context = %#v, want %#v", got, want)
		}
	})
}
