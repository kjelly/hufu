package team

import (
	"testing"

	"github.com/kjelly/hufu/internal/agent"
)

func TestResolveAgentModelKeepsCoordinatorSeparateFromWorker(t *testing.T) {
	c := &Coordinator{session: &TeamSession{Config: agent.TeamConfig{
		WorkerModel:      "codex/gpt-5.6-luna",
		CoordinatorModel: "ollama/minimax-m2.7:cloud",
	}}}

	coordinator := &agent.AgentDef{
		Name: "coordinator", Role: "coordinator",
		Generation: agent.GenerationParams{Model: "agent-frontmatter-model"},
	}
	if got := c.resolveAgentModel(coordinator, ""); got != "ollama/minimax-m2.7:cloud" {
		t.Fatalf("coordinator model = %q, want %q", got, "ollama/minimax-m2.7:cloud")
	}

	worker := &agent.AgentDef{Name: "coder", Role: "worker"}
	if got := c.resolveAgentModel(worker, ""); got != "codex/gpt-5.6-luna" {
		t.Fatalf("worker model = %q, want %q", got, "codex/gpt-5.6-luna")
	}
}
