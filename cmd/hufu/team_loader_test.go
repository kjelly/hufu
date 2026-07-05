package main

import (
	"testing"

	"github.com/anomalyco/hufu/internal/agent"
	"github.com/anomalyco/hufu/internal/config"
	"github.com/anomalyco/hufu/internal/team"
)

func TestDisplayResolvedConfig(t *testing.T) {
	// Just ensure the function does not panic on empty inputs.
	session := &team.TeamSession{
		Agents: map[string]*agent.AgentDef{},
	}
	displayResolvedConfig(session, nil, "", "", "", "", 8)
}

func TestBuildAllowedPaths(t *testing.T) {
	tmpDir := t.TempDir()
	session := &team.TeamSession{
		Workspace: tmpDir,
		Dir:       tmpDir,
	}
	registry := team.NewTeamRegistry([]string{tmpDir})
	cfg := &config.Config{}
	paths := buildAllowedPaths(session, registry, cfg)
	if len(paths) == 0 {
		t.Error("expected at least one allowed path")
	}
}

func TestSortedAgents(t *testing.T) {
	agents := map[string]*agent.AgentDef{
		"charlie": {Name: "charlie"},
		"alpha":   {Name: "alpha"},
		"bravo":   {Name: "bravo"},
	}
	got := sortedAgents(agents)
	want := []string{"alpha", "bravo", "charlie"}
	if len(got) != 3 {
		t.Fatalf("expected 3 agents, got %d", len(got))
	}
	for i, a := range got {
		if a.Name != want[i] {
			t.Errorf("position %d: got %q, want %q", i, a.Name, want[i])
		}
	}
}

func TestAgentNamesFromSession(t *testing.T) {
	session := &team.TeamSession{
		Agents: map[string]*agent.AgentDef{
			"coordinator":  {Name: "coordinator", Role: "coordinator"},
			"orchestrator": {Name: "orchestrator", Role: "orchestrator"},
			"worker-a":     {Name: "worker-a", Role: "worker"},
			"worker-b":     {Name: "worker-b", Role: "worker"},
		},
	}
	got := agentNamesFromSession(session)
	// Should exclude coordinator and orchestrator
	if len(got) != 2 {
		t.Fatalf("expected 2 worker agents, got %d", len(got))
	}
	seen := map[string]bool{}
	for _, a := range got {
		seen[a.Name] = true
	}
	if !seen["worker-a"] || !seen["worker-b"] {
		t.Errorf("expected worker-a and worker-b, got %v", got)
	}
}
