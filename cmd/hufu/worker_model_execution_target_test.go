package main

import (
	"context"
	"testing"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/execution"
	"github.com/kjelly/hufu/internal/team"
)

// admitWorkerTaskTarget applies overrides exactly as loadTeamCommon does,
// builds a real coordinator, admits one task for coder, and returns the
// canonical execution target frozen on the durable TODO occurrence.
func admitWorkerTaskTarget(t *testing.T, coderProvider string, overrides ModelCLIOverrides) execution.ExecutionTarget {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CODEX_HOME", t.TempDir())
	session := &team.TeamSession{
		Workspace: t.TempDir(),
		Config:    agent.TeamConfig{Name: "worker-model-target"},
		Agents: map[string]*agent.AgentDef{
			"coder": {
				Name:             "coder",
				Role:             "worker",
				SubagentProvider: coderProvider,
				Generation:       agent.GenerationParams{Model: "ollama/markdown-model"},
			},
		},
	}
	applyCLIModelOverrides(&session.Config, overrides)
	if err := applyCLIGenerationOverridesToAgents(session, overrides); err != nil {
		t.Fatal(err)
	}
	c, err := team.NewCoordinator(session, "", "", nil, nil, nil, team.RoleModels{}, 0, false, false, false, nil, nil, nil, false, "", false, false, nil, false, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	c.SetSessionData(team.NewSession())
	c.SetStepConfirmFn(func(context.Context, []team.TaskDef) (bool, error) { return false, nil })
	if _, err := c.ExecuteTasks(t.Context(), []team.TaskDef{{Agent: "coder", Goal: "implement feature X"}}); err == nil {
		t.Fatal("expected step confirmation to stop execution after task admission")
	}
	items := c.TaskTracker().TodoList().Items()
	if len(items) != 1 {
		t.Fatalf("admitted tasks = %d, want 1", len(items))
	}
	return items[0].ExecutionTarget
}

// TestWorkerModelOverrideFreezesCanonicalExecutionTarget proves that an
// overridden worker reaches the same canonical ExecutionTarget resolution as
// the global --model flag: a qualified selector is split into backend and
// model exactly once, it outranks a frontmatter subagent-provider, and a bare
// target inherits that provider for compatibility.
func TestWorkerModelOverrideFreezesCanonicalExecutionTarget(t *testing.T) {
	tests := []struct {
		name     string
		provider string
		target   string
		want     execution.ExecutionTarget
	}{
		{name: "explicit codex selector", target: "codex/gpt-6-sol", want: execution.ExecutionTarget{Backend: "codex", Model: "gpt-6-sol"}},
		{name: "codex selector with codex frontmatter provider", provider: "codex", target: "codex/gpt-6-sol", want: execution.ExecutionTarget{Backend: "codex", Model: "gpt-6-sol"}},
		{name: "ollama selector", target: "ollama/qwen3.5:27b", want: execution.ExecutionTarget{Backend: "ollama", Model: "qwen3.5:27b"}},
		{name: "ollama selector outranks codex frontmatter provider", provider: "codex", target: "ollama/qwen3.5:27b", want: execution.ExecutionTarget{Backend: "ollama", Model: "qwen3.5:27b"}},
		{name: "bare target inherits codex frontmatter provider", provider: "codex", target: "gpt-6-sol", want: execution.ExecutionTarget{Backend: "codex", Model: "gpt-6-sol"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			worker := admitWorkerTaskTarget(t, tt.provider, ModelCLIOverrides{WorkerModels: []WorkerModelOverride{{Agent: "coder", Target: tt.target}}})
			if worker != tt.want {
				t.Fatalf("--worker-model frozen target = %#v, want %#v", worker, tt.want)
			}
			global := admitWorkerTaskTarget(t, tt.provider, ModelCLIOverrides{Model: tt.target})
			if global != worker {
				t.Fatalf("--worker-model target %#v differs from --model target %#v", worker, global)
			}
		})
	}
}
