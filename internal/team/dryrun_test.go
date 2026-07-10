package team

import (
	"context"
	"testing"
	"time"

	"github.com/anomalyco/hufu/internal/agent"
	"github.com/anomalyco/hufu/internal/skill"
)

// newTestCoordinatorForDryRun builds a Coordinator with no provider, sidecar,
// or memory store. It only has session data and skills/agents populated, so
// DryRun() can exercise its static-preview path without touching the LLM.
func newTestCoordinatorForDryRun(t *testing.T, agents map[string]*agent.AgentDef, skills []*skill.SkillDef, cfg agent.TeamConfig) *Coordinator {
	t.Helper()
	workspace := t.TempDir()
	c := &Coordinator{
		reportStatus: func(event StatusEvent) {},
		session: &TeamSession{
			Config:    cfg,
			Dir:       workspace,
			Workspace: workspace,
			Agents:    agents,
			Skills:    skills,
		},
		skills: skills,
	}
	return c
}

func TestDryRun_NoLLMCall_Structure(t *testing.T) {
	agents := map[string]*agent.AgentDef{
		"coordinator": {Name: "coordinator", Role: "coordinator", Tools: "ask_user"},
		"developer":   {Name: "developer", Role: "worker", Tools: "read,write,bash", Skills: "code-review"},
		"reviewer":    {Name: "reviewer", Role: "worker", Tools: "read,bash"},
	}
	skills := []*skill.SkillDef{
		{Name: "code-reviewer", Description: "Use this skill to review code in pull requests"},
		{Name: "git-commit", Description: "Execute git commit with conventional commit message analysis"},
	}
	cfg := agent.TeamConfig{
		Name: "demo",
		Generation: agent.GenerationParams{
			Model: "ollama/qwen3:8b",
		},
		Skills: "code-reviewer,git-commit",
	}
	c := newTestCoordinatorForDryRun(t, agents, skills, cfg)

	result, err := c.DryRun(context.Background(), "Please review my pull request and run a code review")
	if err != nil {
		t.Fatalf("DryRun returned error: %v", err)
	}

	// Result should reflect the user prompt verbatim
	if result.UserPrompt != "Please review my pull request and run a code review" {
		t.Errorf("UserPrompt = %q, want verbatim prompt", result.UserPrompt)
	}

	// Team + model populated from session
	if result.TeamName != "demo" {
		t.Errorf("TeamName = %q, want %q", result.TeamName, "demo")
	}
	if result.Model == "" {
		t.Error("Model is empty, want a value from session/agent config")
	}

	// All agents listed (including coordinator)
	agentNames := map[string]bool{}
	for _, a := range result.Agents {
		agentNames[a.Name] = true
	}
	for _, want := range []string{"coordinator", "developer", "reviewer"} {
		if !agentNames[want] {
			t.Errorf("Agents missing %q, got: %v", want, agentNames)
		}
	}

	// No duplicate agent names (DryRun must dedupe by def.Name)
	if len(agentNames) != len(result.Agents) {
		duplicates := []string{}
		seen := map[string]bool{}
		for _, a := range result.Agents {
			if seen[a.Name] {
				duplicates = append(duplicates, a.Name)
			}
			seen[a.Name] = true
		}
		t.Errorf("Agents contains duplicates: %v (full list: %v)", duplicates, agentNames)
	}

	// All skills listed in AllSkills
	if len(result.AllSkills) != len(skills) {
		t.Errorf("AllSkills has %d entries, want %d", len(result.AllSkills), len(skills))
	}

	// MatchedSkillNames is a subset of AllSkills and includes matches
	allSet := map[string]bool{}
	for _, s := range result.AllSkills {
		allSet[s.Name] = true
	}
	for _, m := range result.MatchedSkillNames {
		if !allSet[m] {
			t.Errorf("MatchedSkillNames contains %q but not in AllSkills", m)
		}
	}

	// "code review" / "pull request" should match the code-reviewer skill
	matched := false
	for _, m := range result.MatchedSkillNames {
		if m == "code-reviewer" {
			matched = true
		}
	}
	if !matched {
		t.Errorf("expected code-reviewer in MatchedSkillNames, got: %v", result.MatchedSkillNames)
	}

	// No LLM, so no FirstRoundTasks
	if len(result.FirstRoundTasks) != 0 {
		t.Errorf("FirstRoundTasks should be empty for LLM-free dry-run, got: %v", result.FirstRoundTasks)
	}
}

func TestDryRun_NoLLMCall_UnmatchedPrompt(t *testing.T) {
	skills := []*skill.SkillDef{
		{Name: "code-reviewer", Description: "Use this skill to review code"},
	}
	cfg := agent.TeamConfig{
		Name: "demo",
		Generation: agent.GenerationParams{
			Model: "ollama/qwen3:8b",
		},
	}
	agents := map[string]*agent.AgentDef{
		"coordinator": {Name: "coordinator", Role: "coordinator", Tools: "ask_user"},
	}
	c := newTestCoordinatorForDryRun(t, agents, skills, cfg)

	result, err := c.DryRun(context.Background(), "deploy the kubernetes cluster to production")
	if err != nil {
		t.Fatalf("DryRun returned error: %v", err)
	}

	if len(result.MatchedSkillNames) != 0 {
		t.Errorf("MatchedSkillNames = %v, want empty (prompt shares no keywords with skills)", result.MatchedSkillNames)
	}
	// AllSkills still populated
	if len(result.AllSkills) != 1 {
		t.Errorf("AllSkills = %d, want 1 (still listed, just not matched)", len(result.AllSkills))
	}
}

func TestDryRun_NoLLMCall_EmptySession(t *testing.T) {
	cfg := agent.TeamConfig{Name: "demo"}
	c := newTestCoordinatorForDryRun(t, nil, nil, cfg)

	result, err := c.DryRun(context.Background(), "anything")
	if err != nil {
		t.Fatalf("DryRun returned error: %v", err)
	}
	if result.TeamName != "demo" {
		t.Errorf("TeamName = %q, want %q", result.TeamName, "demo")
	}
	if len(result.Agents) != 0 {
		t.Errorf("Agents = %d, want 0", len(result.Agents))
	}
	if len(result.AllSkills) != 0 {
		t.Errorf("AllSkills = %d, want 0", len(result.AllSkills))
	}
}

// TestDryRun_NoLLMCall_CompletesQuickly asserts the dry-run does not block
// on LLM calls. It uses a context with a tight timeout; if the implementation
// ever sneaks in a model call, the timeout fires and the test fails with a
// "context deadline exceeded" error, *not* the fast LLM-free completion we
// expect.
func TestDryRun_NoLLMCall_CompletesQuickly(t *testing.T) {
	agents := map[string]*agent.AgentDef{
		"coordinator": {Name: "coordinator", Role: "coordinator", Tools: "ask_user"},
		"developer":   {Name: "developer", Role: "worker", Tools: "read,write,bash"},
	}
	cfg := agent.TeamConfig{
		Name:       "demo",
		Timeout:    9999,
		MaxRounds:  9999,
		Generation: agent.GenerationParams{Model: "ollama/qwen3:8b"},
	}
	c := newTestCoordinatorForDryRun(t, agents, nil, cfg)

	// Point the resolver at a port that will never accept a connection.
	// If the dry-run ever tries to dial it, the test should fail fast.
	c.session.Config.ProviderURL = "http://127.0.0.1:1/v1"

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	start := time.Now()
	_, err := c.DryRun(ctx, "test")
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("DryRun returned error: %v (elapsed %s)", err, elapsed)
	}
	if elapsed > 2*time.Second {
		t.Errorf("DryRun took %s; LLM-free preview should be sub-second", elapsed)
	}
}

func TestDryRun_NoLLMCall_DoesNotCreateProviderManager(t *testing.T) {
	// Smoke test: even with no providerManager set on the coordinator,
	// DryRun must not panic trying to use it.
	cfg := agent.TeamConfig{
		Name:       "demo",
		Generation: agent.GenerationParams{Model: "ollama/qwen3:8b"},
	}
	agents := map[string]*agent.AgentDef{
		"coordinator": {Name: "coordinator", Role: "coordinator", Tools: "ask_user"},
	}
	c := newTestCoordinatorForDryRun(t, agents, nil, cfg)
	// c.providerManager is nil; if DryRun() tries to use it, this will panic.

	_, err := c.DryRun(context.Background(), "test")
	if err != nil {
		t.Fatalf("DryRun returned error: %v", err)
	}
}

func TestSkillMatchesPromptKeywords(t *testing.T) {
	tests := []struct {
		name   string
		skill  *skill.SkillDef
		prompt string
		want   bool
	}{
		{
			name:   "name match",
			skill:  &skill.SkillDef{Name: "code-reviewer", Description: "Reviews code"},
			prompt: "Please run the code reviewer on this PR",
			want:   true,
		},
		{
			name:   "description keyword match",
			skill:  &skill.SkillDef{Name: "git-commit", Description: "Conventional commit message analysis"},
			prompt: "Write a commit message following conventions",
			want:   true,
		},
		{
			name:   "no match",
			skill:  &skill.SkillDef{Name: "code-reviewer", Description: "Reviews code"},
			prompt: "deploy the kubernetes cluster",
			want:   false,
		},
		{
			name:   "empty prompt",
			skill:  &skill.SkillDef{Name: "code-reviewer", Description: "Reviews code"},
			prompt: "",
			want:   false,
		},
		{
			name:   "case insensitive",
			skill:  &skill.SkillDef{Name: "Code-Reviewer", Description: "Reviews code"},
			prompt: "CODE REVIEW PLEASE",
			want:   true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := SkillMatchesPrompt(tt.skill, tt.prompt)
			if got != tt.want {
				t.Errorf("SkillMatchesPrompt(%q, %q) = %v, want %v",
					tt.skill.Name, tt.prompt, got, tt.want)
			}
		})
	}
}
