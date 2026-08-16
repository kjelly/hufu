package team

import (
	"strings"
	"testing"

	"github.com/kjelly/hufu/internal/agent"
)

func TestDefaultCoordinatorPromptOmitsMemoryMutationAliases(t *testing.T) {
	c := &Coordinator{
		session: &TeamSession{Workspace: t.TempDir(), Config: agent.TeamConfig{Name: "team"}, Agents: map[string]*agent.AgentDef{
			"coordinator": {Name: "coordinator", Role: "coordinator", Tools: "all"},
		}},
		coreTools:   workerInvariantCoreTools(t),
		taskTracker: NewTaskTracker(),
	}
	prompt := c.BuildOrchestratorPrompt()
	for _, alias := range []string{"stm_write", "ltm_update", "memory_save", "ltm.md"} {
		if strings.Contains(prompt, alias) {
			t.Fatalf("default coordinator prompt mentions unavailable alias/truth source %q", alias)
		}
	}
	if !strings.Contains(prompt, "structured") {
		t.Fatal("default prompt does not describe runtime structured-result capture")
	}
}

func TestCoordinatorPromptMatchesDelegationAllowlist(t *testing.T) {
	c := &Coordinator{
		session: &TeamSession{Workspace: t.TempDir(), Config: agent.TeamConfig{
			Name:       "review-team",
			Delegation: agent.DelegationPolicy{AllowedWorkers: []string{"reviewer"}},
		}, Agents: map[string]*agent.AgentDef{
			"coordinator": {Name: "coordinator", Role: "coordinator"},
			"reviewer":    {Name: "reviewer", Role: "worker", Description: "allowed reviewer"},
			"helper":      {Name: "Helper", Role: "worker", Description: "built-in fallback"},
		}},
		coreTools:   workerInvariantCoreTools(t),
		taskTracker: NewTaskTracker(),
	}
	prompt := c.BuildOrchestratorPrompt()
	if !strings.Contains(prompt, "Valid names: reviewer") || !strings.Contains(prompt, "### reviewer") {
		t.Fatalf("prompt omitted allowlisted reviewer: %s", prompt)
	}
	if strings.Contains(prompt, "Valid names: reviewer, Helper") || strings.Contains(prompt, "### Helper\n") {
		t.Fatalf("prompt exposed allowlist-excluded helper: %s", prompt)
	}
}

func TestCoordinatorPromptDescribesOptedInAliasAsCandidate(t *testing.T) {
	c := &Coordinator{
		session: &TeamSession{Workspace: t.TempDir(), Config: agent.TeamConfig{Name: "team"}, Agents: map[string]*agent.AgentDef{
			"coordinator": {Name: "coordinator", Role: "coordinator", Tools: "ltm_update"},
		}},
		coreTools:   workerInvariantCoreTools(t),
		taskTracker: NewTaskTracker(),
	}
	prompt := c.BuildOrchestratorPrompt()
	if !strings.Contains(prompt, "### ltm_update") || !strings.Contains(prompt, "canonical candidate") {
		t.Fatalf("opted-in compatibility prompt is missing candidate semantics:\n%s", prompt)
	}
	if strings.Contains(prompt, "Append to a specific section of long-term memory (ltm.md)") {
		t.Fatal("compatibility prompt still describes Markdown as persistent truth")
	}
}
