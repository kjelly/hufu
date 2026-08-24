package team

import (
	"context"
	"strings"
	"testing"

	"charm.land/fantasy"
)

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
	if got := requestContextTokens(context.Background(), modelID, shapedSystem, "review the project", nil, shapedTools); got > budget {
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
