package team

import (
	"context"
	"testing"

	"charm.land/fantasy"
	"github.com/kjelly/hufu/internal/agent"
)

func TestExecutionTargetHasLanguageModelCapabilityRejectsAgentBackends(t *testing.T) {
	c := &Coordinator{session: &TeamSession{Config: agent.TeamConfig{
		SubagentProviders: map[string]agent.SubagentProviderConfig{"review-bot": {Type: "codex-app-server"}},
	}}}
	for _, target := range []string{"codex/gpt-5.6-luna", "review-bot/gpt-5.6-luna"} {
		if c.executionTargetHasLanguageModelCapability(target) {
			t.Fatalf("executionTargetHasLanguageModelCapability(%q) = true, want false", target)
		}
	}
	if !c.executionTargetHasLanguageModelCapability("local/qwen3:8b") {
		t.Fatal("local LLM target was rejected")
	}
}

func TestFilterWorkerToolsSkipsProfileLookupForAgentBackend(t *testing.T) {
	c := &Coordinator{session: &TeamSession{Config: agent.TeamConfig{}}, modelProfileRuntime: &ModelProfileRuntime{}}
	tool := fantasy.NewAgentTool("read", "read", func(context.Context, struct{}, fantasy.ToolCall) (fantasy.ToolResponse, error) {
		return fantasy.NewTextResponse("ok"), nil
	})
	filtered, err := c.filterWorkerToolsForModel(t.Context(), "codex/gpt-5.6-luna", []fantasy.AgentTool{tool}, true, []string{"read"})
	if err != nil {
		t.Fatalf("filterWorkerToolsForModel() error = %v", err)
	}
	if len(filtered) != 1 || filtered[0] != tool {
		t.Fatalf("filtered tools = %#v, want the original agent-backend tools", filtered)
	}
}
