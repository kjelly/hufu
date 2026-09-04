package team

import (
	"context"
	"strings"
	"testing"
	"time"

	"charm.land/fantasy"

	"github.com/kjelly/hufu/internal/agent"
)

func TestWithoutCoordinatorRequestPreflightPreservesParentContext(t *testing.T) {
	type preservedContextKey struct{}
	parent, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	parent = context.WithValue(parent, preservedContextKey{}, "preserved")
	parent = withCoordinatorRequestPreflight(parent, newCoordinatorRequestPreflight("model", "goal", "system", nil))

	worker := withoutCoordinatorRequestPreflight(parent)
	if coordinatorRequestPreflightFromContext(worker) != nil {
		t.Fatal("worker context retained coordinator request preflight")
	}
	if got := worker.Value(preservedContextKey{}); got != "preserved" {
		t.Fatalf("worker context value = %v, want preserved parent value", got)
	}
	if worker.Done() != parent.Done() {
		t.Fatal("worker context did not preserve parent cancellation channel")
	}
	workerDeadline, workerOK := worker.Deadline()
	parentDeadline, parentOK := parent.Deadline()
	if !workerOK || !parentOK || !workerDeadline.Equal(parentDeadline) {
		t.Fatalf("worker deadline = %v/%t, parent deadline = %v/%t", workerDeadline, workerOK, parentDeadline, parentOK)
	}
}

func TestWithoutCoordinatorRequestPreflightHidesOnlyPreflight(t *testing.T) {
	type preservedContextKey struct{}
	preflight := newCoordinatorRequestPreflight("model", "goal", "system", nil)
	parent := context.WithValue(withCoordinatorRequestPreflight(t.Context(), preflight), preservedContextKey{}, "preserved")

	worker := withoutCoordinatorRequestPreflight(parent)
	if coordinatorRequestPreflightFromContext(worker) != nil {
		t.Fatal("worker context retained coordinator request preflight")
	}
	if got := worker.Value(preservedContextKey{}); got != "preserved" {
		t.Fatalf("worker context value = %v, want preserved parent value", got)
	}
}

func TestCoordinatorRequestPreflightRetainsBoundAdmissionContext(t *testing.T) {
	bound := agent.ProviderAdmissionContext{
		ModelID:             "local/model",
		ProviderIdentity:    "local",
		ProviderBaseURL:     "http://127.0.0.1:11434/v1",
		Bound:               true,
		ContextWindow:       32_768,
		MaxOutputTokens:     1_024,
		SafetyMarginTokens:  256,
		ContextWindowSource: "provider_runtime",
	}
	preflight := newCoordinatorRequestPreflightWithAdmission("local/model", "goal", "system", nil, bound)
	if got := preflight.admissionContextValue(); got != bound {
		t.Fatalf("preflight admission context = %#v, want %#v", got, bound)
	}
}

func TestCoordinatorRequestPreflightUsesBoundEstimatorForPrepare(t *testing.T) {
	const modelID = "preflight-bound-estimator-prepare"
	preserveRegisteredModelSpec(t, GlobalModelSpecRegistry(), modelID)
	GlobalModelSpecRegistry().RegisterSpec(ModelContextSpec{
		ModelID:            modelID,
		ContextWindow:      128_000,
		MaxOutputTokens:    512,
		SafetyMarginTokens: 64,
		Estimator:          conservativeTokenEstimator,
	})

	system := strings.Repeat("bound estimator system text ", 1_000)
	prompt := strings.Repeat("bound estimator prompt ", 100)
	bound := agent.ProviderAdmissionContext{
		ModelID:            modelID,
		ProviderIdentity:   "local",
		Bound:              true,
		MaxOutputTokens:    512,
		SafetyMarginTokens: 64,
		Estimator:          "qwen",
	}
	request := providerCallFromContextRequest(modelID, system, prompt, nil, nil)
	request.AdmissionContext = bound
	boundTokens, err := defaultCounter.CountProviderRequest(t.Context(), modelID, request)
	if err != nil {
		t.Fatalf("count bound request: %v", err)
	}
	legacyRequest := request
	legacyRequest.AdmissionContext = agent.ProviderAdmissionContext{}
	legacyTokens, err := defaultCounter.CountProviderRequest(t.Context(), modelID, legacyRequest)
	if err != nil {
		t.Fatalf("count registry request: %v", err)
	}
	if legacyTokens <= boundTokens {
		t.Fatalf("registry tokens = %d, bound tokens = %d; want global estimated count to be larger", legacyTokens, boundTokens)
	}

	bound.ContextWindow = boundTokens + bound.MaxOutputTokens + bound.SafetyMarginTokens
	preflight := newCoordinatorRequestPreflightWithAdmission(modelID, prompt, system, nil, bound)
	_, _, applied, err := preflight.prepare(t.Context(), nil, prompt, bound.MaxOutputTokens, 0)
	if err != nil {
		t.Fatalf("prepare() error = %v", err)
	}
	if applied {
		t.Fatalf("prepare() projected a request that fits the bound qwen budget: bound=%d legacy=%d", boundTokens, legacyTokens)
	}
}

func TestCoordinatorPreflightShapingUsesBoundEstimator(t *testing.T) {
	const modelID = "preflight-bound-estimator-shaping"
	preserveRegisteredModelSpec(t, GlobalModelSpecRegistry(), modelID)
	GlobalModelSpecRegistry().RegisterSpec(ModelContextSpec{
		ModelID:   modelID,
		Estimator: conservativeTokenEstimator,
	})
	bound := agent.ProviderAdmissionContext{
		ModelID:          modelID,
		ProviderIdentity: "local",
		Bound:            true,
		Estimator:        "qwen",
	}

	system := "Required coordinator contract.\n\n## Environment & Rules\n" + strings.Repeat("environment detail ", 600)
	systemBudget, _ := defaultCounter.countTextWithAdmission(t.Context(), modelID, system, bound)
	shapedSystem := shrinkCoordinatorSystemToBudget(t.Context(), modelID, system, systemBudget, bound)
	if shapedSystem != system {
		t.Fatalf("bound estimator unnecessarily shaped system prompt: got %d bytes, want %d", len(shapedSystem), len(system))
	}
	legacySystem := shrinkCoordinatorSystemToBudget(t.Context(), modelID, system, systemBudget, agent.ProviderAdmissionContext{})
	if legacySystem == system {
		t.Fatal("regression setup did not make registry estimator shape the system prompt")
	}

	required := fantasy.NewAgentTool("agent", "required", func(context.Context, coordinatorPreflightTestInput, fantasy.ToolCall) (fantasy.ToolResponse, error) {
		return fantasy.NewTextResponse("ok"), nil
	})
	optional := fantasy.NewAgentTool("team_info", strings.Repeat("optional guidance ", 1_000), func(context.Context, coordinatorPreflightTestInput, fantasy.ToolCall) (fantasy.ToolResponse, error) {
		return fantasy.NewTextResponse("ok"), nil
	})
	tools := []fantasy.AgentTool{required, optional}
	toolBudget, _ := defaultCounter.countToolsWithAdmission(t.Context(), modelID, tools, bound)
	projectedTools := projectCoordinatorToolsToBudget(t.Context(), modelID, tools, toolBudget, bound)
	if len(projectedTools) != len(tools) {
		t.Fatalf("bound estimator unnecessarily projected tools: got %d, want %d", len(projectedTools), len(tools))
	}
	legacyTools := projectCoordinatorToolsToBudget(t.Context(), modelID, tools, toolBudget, agent.ProviderAdmissionContext{})
	if len(legacyTools) >= len(tools) {
		t.Fatalf("regression setup did not make registry estimator project optional tools: got %d tools", len(legacyTools))
	}
}

func TestCoordinatorPromptExplainsWorkerSkillInstructions(t *testing.T) {
	c := &Coordinator{
		session: &TeamSession{
			Workspace: t.TempDir(),
			Config:    agent.TeamConfig{Name: "team"},
			Agents: map[string]*agent.AgentDef{
				"coordinator": {Name: "coordinator", Role: "coordinator", Tools: "all"},
			},
		},
		coreTools:   workerInvariantCoreTools(t),
		taskTracker: NewTaskTracker(),
	}
	prompt := c.BuildOrchestratorPrompt()
	for _, want := range []string{"explicitly granted", "full skill instructions by default"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("coordinator prompt missing %q: %s", want, prompt)
		}
	}
}

type coordinatorPreflightTestInput struct {
	Value string `json:"value"`
}

func TestCoordinatorRequestPreflightShapesFullRequest(t *testing.T) {
	modelID := "preflight-test-model"
	GlobalModelSpecRegistry().RegisterSpec(ModelContextSpec{
		ModelID:            modelID,
		ContextWindow:      5000,
		MaxOutputTokens:    512,
		SafetyMarginTokens: 256,
	})
	tool := fantasy.NewAgentTool("agent", strings.Repeat("delegation guidance ", 80), func(context.Context, coordinatorPreflightTestInput, fantasy.ToolCall) (fantasy.ToolResponse, error) {
		return fantasy.NewTextResponse("ok"), nil
	})
	finish := fantasy.NewAgentTool("finish", strings.Repeat("finish guidance ", 80), func(context.Context, coordinatorPreflightTestInput, fantasy.ToolCall) (fantasy.ToolResponse, error) {
		return fantasy.NewTextResponse("ok"), nil
	})
	optional := fantasy.NewAgentTool("team_info", strings.Repeat("optional guidance ", 120), func(context.Context, coordinatorPreflightTestInput, fantasy.ToolCall) (fantasy.ToolResponse, error) {
		return fantasy.NewTextResponse("ok"), nil
	})
	system := "Required coordinator contract.\n\n## Tools\n" + strings.Repeat("verbose tool instructions ", 100) +
		"\n\n## Project Context (AGENTS.md)\n" + strings.Repeat("project convention ", 2000) +
		"\n\n## Environment & Rules\n" + strings.Repeat("environment detail ", 100)

	preflight := newCoordinatorRequestPreflight(modelID, "review the project", system, []fantasy.AgentTool{tool, finish, optional})
	shapedSystem, shapedTools, applied, err := preflight.prepare(context.Background(), nil, "review the project", 512, 0)
	if err != nil {
		t.Fatalf("prepare() error = %v", err)
	}
	if !applied {
		t.Fatal("prepare() did not apply shaping to an oversized request")
	}
	if len(shapedSystem) >= len(system) {
		t.Fatalf("system prompt was not reduced: %d >= %d", len(shapedSystem), len(system))
	}
	if len(shapedTools) >= len([]fantasy.AgentTool{tool, finish, optional}) {
		t.Fatal("tool projection did not remove an optional tool")
	}
	if !containsPreflightTool(shapedTools, "agent") || !containsPreflightTool(shapedTools, "finish") {
		t.Fatal("tool projection removed a required coordinator tool")
	}
	budget := CalculateContextBudget(GlobalModelSpecRegistry().GetSpec(modelID), 0, 0).Available
	if got := requestContextTokens(context.Background(), modelID, shapedSystem, "review the project", nil, shapedTools, agent.ProviderAdmissionContext{}); got > budget {
		t.Fatalf("shaped request has %d tokens, budget is %d", got, budget)
	}
}

func TestCoordinatorRequestPreflightLearnsObservedWindow(t *testing.T) {
	modelID := "preflight-observed-model"
	GlobalModelSpecRegistry().RegisterSpec(ModelContextSpec{
		ModelID:            modelID,
		ContextWindow:      128000,
		MaxOutputTokens:    1024,
		SafetyMarginTokens: 256,
	})
	preflight := newCoordinatorRequestPreflight(modelID, "goal", "required coordinator contract", nil)
	preflight.observeWindow(39936)
	_, _, applied, err := preflight.prepare(context.Background(), nil, "goal", 1024, 0)
	if err != nil {
		t.Fatalf("prepare() error = %v", err)
	}
	if applied {
		t.Fatal("small request should not need shaping after observing a usable window")
	}
	if preflight.window != 39936 {
		t.Fatalf("observed window = %d, want 39936", preflight.window)
	}
}

func TestCoordinatorRequestPreflightRestoresFullConfigurationOnLaterStep(t *testing.T) {
	modelID := "preflight-restore-model"
	GlobalModelSpecRegistry().RegisterSpec(ModelContextSpec{
		ModelID:            modelID,
		ContextWindow:      5000,
		MaxOutputTokens:    128,
		SafetyMarginTokens: 64,
	})
	tool := fantasy.NewAgentTool("agent", "required", func(context.Context, coordinatorPreflightTestInput, fantasy.ToolCall) (fantasy.ToolResponse, error) {
		return fantasy.NewTextResponse("ok"), nil
	})
	optional := fantasy.NewAgentTool("optional", strings.Repeat("optional guidance ", 100), func(context.Context, coordinatorPreflightTestInput, fantasy.ToolCall) (fantasy.ToolResponse, error) {
		return fantasy.NewTextResponse("ok"), nil
	})
	preflight := newCoordinatorRequestPreflight(modelID, "goal", strings.Repeat("system detail ", 200), []fantasy.AgentTool{tool, optional})
	largeStep := []fantasy.Message{fantasy.NewUserMessage(strings.Repeat("prior result ", 1000))}
	_, projectedTools, applied, err := preflight.prepare(context.Background(), largeStep, "goal", 128, 0)
	if err != nil {
		t.Fatalf("first prepare() error = %v", err)
	}
	if !applied || len(projectedTools) >= 2 {
		t.Fatalf("first step projection = applied %v, tools %d; want a reduced projection", applied, len(projectedTools))
	}
	_, restoredTools, restored, err := preflight.prepare(context.Background(), nil, "goal", 128, 1)
	if err != nil {
		t.Fatalf("later prepare() error = %v", err)
	}
	if !restored || len(restoredTools) != 2 {
		t.Fatalf("later step tools = %d, want complete configuration", len(restoredTools))
	}
}

func containsPreflightTool(tools []fantasy.AgentTool, name string) bool {
	for _, tool := range tools {
		if tool != nil && tool.Info().Name == name {
			return true
		}
	}
	return false
}
