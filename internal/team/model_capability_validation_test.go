package team

import (
	"context"
	"testing"

	"charm.land/fantasy"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/config"
	"github.com/kjelly/hufu/internal/modelprofile"
	"github.com/kjelly/hufu/internal/providerintrospection"
)

type capabilityTestIntrospector struct{}

func (capabilityTestIntrospector) InspectModel(_ context.Context, _ providerintrospection.ProviderRef, modelID string) (providerintrospection.RuntimeModelInfo, error) {
	if modelID == "weak" {
		return providerintrospection.RuntimeModelInfo{CapabilityEvidence: map[string]providerintrospection.CapabilityState{"tools": providerintrospection.CapabilityNo}}, nil
	}
	return providerintrospection.RuntimeModelInfo{CapabilityEvidence: map[string]providerintrospection.CapabilityState{"tools": providerintrospection.CapabilityYes}}, nil
}

func TestMergeModelRequirementsIsAdditive(t *testing.T) {
	got := mergeModelRequirements(
		agent.ModelRequirements{Tools: true, MinContext: 16_384},
		agent.ModelRequirements{Reasoning: true, MinContext: 32_768},
	)
	want := agent.ModelRequirements{Tools: true, Reasoning: true, MinContext: 32_768}
	if got != want {
		t.Fatalf("mergeModelRequirements() = %#v, want %#v", got, want)
	}
}

func TestCheckModelRequirementKnownNoAndInsufficientContext(t *testing.T) {
	validation := ModelCapabilityValidation{}
	profile := modelprofile.ModelProfile{
		EffectiveContext: 8_192,
		SupportsTools:    modelprofile.CapabilityNo,
		Sources: modelprofile.ModelProfileSources{
			EffectiveContext: modelprofile.ResolvedValue[int]{Value: 8_192, Source: modelprofile.SourceProviderRuntime},
			Capabilities: modelprofile.CapabilitySources{
				Tools: modelprofile.ResolvedValue[modelprofile.CapabilityState]{Value: modelprofile.CapabilityNo, Source: modelprofile.SourceProviderRuntime},
			},
		},
	}
	checkModelRequirement(&validation, "worker", "model", profile, agent.ModelRequirements{Tools: true, MinContext: 16_384})
	if len(validation.Errors) != 2 || len(validation.Warnings) != 0 {
		t.Fatalf("validation = %#v, want two hard errors", validation)
	}
}

func TestCheckModelRequirementUnknownIsWarning(t *testing.T) {
	validation := ModelCapabilityValidation{}
	checkModelRequirement(&validation, "worker", "model", modelprofile.ModelProfile{}, agent.ModelRequirements{Reasoning: true, MinContext: 16_384})
	if len(validation.Errors) != 0 || len(validation.Warnings) != 2 {
		t.Fatalf("validation = %#v, want two warnings", validation)
	}
}

func TestKnownIncapableModelDoesNotReceiveToolsOrProtocolSurface(t *testing.T) {
	manager, err := agent.NewProviderManager("http://127.0.0.1:11434/v1", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	runtime := &ModelProfileRuntime{
		manager: manager,
		resolver: modelprofile.NewRuntimeResolver(func(providerintrospection.ProviderRef) providerintrospection.ModelIntrospector {
			return capabilityTestIntrospector{}
		}, modelprofile.ProfileCacheOptions{}),
	}
	c := &Coordinator{modelProfileRuntime: runtime}
	tool := fantasy.NewAgentTool("bash", "run commands", func(context.Context, struct{}, fantasy.ToolCall) (fantasy.ToolResponse, error) {
		return fantasy.NewTextResponse("ok"), nil
	})
	filtered, err := c.filterWorkerToolsForModel(t.Context(), "weak", []fantasy.AgentTool{tool}, false, nil)
	if err != nil || len(filtered) != 0 {
		t.Fatalf("filtered tools = %v, err=%v; known incapable model must receive no tools", filtered, err)
	}
	if _, err := c.filterWorkerToolsForModel(t.Context(), "weak", []fantasy.AgentTool{tool}, true, []string{"bash"}); err == nil {
		t.Fatal("protocol/tool sequence was allowed for a known incapable model")
	}
}

func TestCapabilityValidationUsesConfiguredInvocationContext(t *testing.T) {
	manager, err := agent.NewProviderManager("http://127.0.0.1:11434/v1", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	runtime := &ModelProfileRuntime{
		manager: manager,
		resolver: modelprofile.NewRuntimeResolver(func(providerintrospection.ProviderRef) providerintrospection.ModelIntrospector {
			return configuredContextTestIntrospector{}
		}, modelprofile.ProfileCacheOptions{}),
	}
	worker := &agent.AgentDef{
		Name: "worker", Role: "worker",
		Generation:   agent.GenerationParams{Model: "configured-worker", ContextWindow: 8_192},
		Requirements: agent.ContractRequirements{Model: agent.ModelRequirements{MinContext: 16_384}},
	}
	c := &Coordinator{
		modelProfileRuntime: runtime,
		session: &TeamSession{
			Config: agent.TeamConfig{Generation: agent.GenerationParams{ContextWindow: 32_768}},
			Agents: map[string]*agent.AgentDef{"worker": worker},
		},
	}
	validation := c.ValidateModelCapabilities(t.Context())
	if err := validation.Err(); err == nil {
		t.Fatalf("configured context override was ignored: validation=%#v", validation)
	}
}

type configuredContextTestIntrospector struct{}

func (configuredContextTestIntrospector) InspectModel(context.Context, providerintrospection.ProviderRef, string) (providerintrospection.RuntimeModelInfo, error) {
	return providerintrospection.RuntimeModelInfo{ModelMaxContext: 32_768}, nil
}

func TestProtocolAgentConstructionFailsClosedForKnownToolIncapableModels(t *testing.T) {
	manager, err := agent.NewProviderManager("http://127.0.0.1:11434/v1", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	c := &Coordinator{
		providerManager: manager,
		modelProfileRuntime: &ModelProfileRuntime{
			manager: manager,
			resolver: modelprofile.NewRuntimeResolver(func(providerintrospection.ProviderRef) providerintrospection.ModelIntrospector {
				return capabilityTestIntrospector{}
			}, modelprofile.ProfileCacheOptions{}),
		},
		session: &TeamSession{},
	}
	tool := fantasy.NewAgentTool("submit_result", "submit the required result", func(context.Context, struct{}, fantasy.ToolCall) (fantasy.ToolResponse, error) {
		return fantasy.NewTextResponse("ok"), nil
	})
	for _, role := range []string{"coordinator", "plan_reviewer", "auxiliary"} {
		t.Run(role, func(t *testing.T) {
			_, err := c.createGatedAgent(t.Context(), manager.GetProvider("weak"), agent.AgentConfig{
				Def:               &agent.AgentDef{Name: role, Role: role, Generation: agent.GenerationParams{Model: "weak"}},
				TeamConfig:        &c.session.Config,
				InvocationModelID: "weak",
			}, []fantasy.AgentTool{tool})
			if err == nil {
				t.Fatal("known tool-incapable protocol agent was constructed")
			}
		})
	}
}

func TestCapabilityAwareRouteSkipsKnownIncapableAlternative(t *testing.T) {
	manager, err := agent.NewProviderManager("http://127.0.0.1:11434/v1", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	c := &Coordinator{
		modelProfileRuntime: &ModelProfileRuntime{
			manager: manager,
			resolver: modelprofile.NewRuntimeResolver(func(providerintrospection.ProviderRef) providerintrospection.ModelIntrospector {
				return capabilityTestIntrospector{}
			}, modelprofile.ProfileCacheOptions{}),
		},
		modelList: []config.ModelEntry{{ID: "weak"}, {ID: "strong"}},
		session:   &TeamSession{Config: agent.TeamConfig{Requirements: agent.ContractRequirements{Model: agent.ModelRequirements{Tools: true}}}},
	}
	def := &agent.AgentDef{Name: "worker", Role: "worker"}
	if got := c.selectCapabilityAwareModel(TaskDef{}, def, "weak"); got != "strong" {
		t.Fatalf("capability-aware model = %q, want strong alternative", got)
	}
}
