package main

import (
	"testing"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/team"
)

func TestExecutionCompatibilityWorkspaceWarningUsesOnlyExplicitArgument(t *testing.T) {
	defaultLines := newExecutionCompatibilityWarningState("").workspaceWarningLines()
	if got, want := defaultLines[1], "`hufu migrate inspect-execution` and then"; got != want {
		t.Fatalf("default inspect warning = %q, want %q", got, want)
	}
	if got, want := defaultLines[2], "`hufu migrate apply-execution --apply`"; got != want {
		t.Fatalf("default apply warning = %q, want %q", got, want)
	}
	explicitLines := newExecutionCompatibilityWarningState("../legacy workspace").workspaceWarningLines()
	if got, want := explicitLines[1], "`hufu migrate inspect-execution --workspace ../legacy workspace` and then"; got != want {
		t.Fatalf("explicit inspect warning = %q, want %q", got, want)
	}
	if got, want := explicitLines[2], "`hufu migrate apply-execution --workspace ../legacy workspace --apply`"; got != want {
		t.Fatalf("explicit apply warning = %q, want %q", got, want)
	}
}

func TestHasAuthoredLegacyLocalExecutionBackend(t *testing.T) {
	session := &team.TeamSession{Config: agent.TeamConfig{WorkerModel: "local/qwen3:8b"}}
	if !team.HasAuthoredLegacyLocalExecutionBackend(session) {
		t.Fatal("raw local selector was not detected")
	}
	session.Config.WorkerModel = "ollama/qwen3:8b"
	session.Config.DefaultLLMBackend = "ollama"
	session.Agents = map[string]*agent.AgentDef{"worker": {Generation: agent.GenerationParams{Model: "openai/gpt-5"}}}
	if team.HasAuthoredLegacyLocalExecutionBackend(session) {
		t.Fatal("canonical selector was incorrectly reported as local")
	}
	session.Config.DefaultLLMBackend = "LOCAL"
	if !team.HasAuthoredLegacyLocalExecutionBackend(session) {
		t.Fatal("raw local default backend was not detected")
	}
}
