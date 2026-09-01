package team

import (
	"strings"
	"testing"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/skill"
)

func TestDefaultOrchestratorSystemUsesWorkerSkillContract(t *testing.T) {
	workspace := t.TempDir()
	orch := &agent.AgentDef{Name: "coordinator", Role: "coordinator", System: ""}
	c := &Coordinator{
		session: &TeamSession{
			Workspace: workspace,
			Config:    agent.TeamConfig{Name: "team"},
			Agents: map[string]*agent.AgentDef{
				"coordinator": orch,
				"worker":      {Name: "worker", Role: "worker"},
			},
		},
		taskTracker:  NewTaskTracker(),
		reportStatus: func(StatusEvent) {},
	}
	c.setAutoLoadedSkills([]*skill.SkillDef{{Name: "code-review", Description: "review workflow", Path: "skills/code-review/SKILL.md", Content: "FULL REVIEW INSTRUCTIONS"}})

	prompt, err := c.buildSystemPrompt(t.Context(), orch, "review the code", true)
	if err != nil {
		t.Fatalf("buildSystemPrompt: %v", err)
	}

	for _, stale := range []string{
		"so workers know which skills to load",
		"so workers can load them if needed",
		"so workers can load it themselves if needed",
		"rather than the full skill content",
	} {
		if strings.Contains(prompt, stale) {
			t.Fatalf("default orchestrator prompt contains stale worker skill guidance %q:\n%s", stale, prompt)
		}
	}
	for _, required := range []string{
		"Workers receive full skill instructions by default",
		"tell a worker to call `load_skill` only when that tool is explicitly granted to the worker",
		"use load_skill to get the detailed instructions",
	} {
		if !strings.Contains(prompt, required) {
			t.Fatalf("default orchestrator prompt missing worker skill contract %q:\n%s", required, prompt)
		}
	}
}

func TestCustomOrchestratorSystemIncludesGeneratedWorkerSkillContract(t *testing.T) {
	workspace := t.TempDir()
	orch := &agent.AgentDef{Name: "coordinator", Role: "coordinator", System: "Custom coordinator policy: preserve the user contract."}
	c := &Coordinator{
		session: &TeamSession{
			Workspace: workspace,
			Config:    agent.TeamConfig{Name: "team"},
			Agents: map[string]*agent.AgentDef{
				"coordinator": orch,
				"worker":      {Name: "worker", Role: "worker"},
			},
		},
		taskTracker:  NewTaskTracker(),
		reportStatus: func(StatusEvent) {},
	}

	prompt, err := c.buildSystemPrompt(t.Context(), orch, "review the code", false)
	if err != nil {
		t.Fatalf("buildSystemPrompt: %v", err)
	}
	if !strings.Contains(prompt, "Custom coordinator policy: preserve the user contract.") {
		t.Fatalf("custom coordinator system was not preserved:\n%s", prompt)
	}
	if !strings.Contains(prompt, "You are the coordinator of team") {
		t.Fatalf("generated orchestrator guidance missing from custom system:\n%s", prompt)
	}
	for _, stale := range []string{
		"so workers know which skills to load",
		"so workers can load them if needed",
	} {
		if strings.Contains(prompt, stale) {
			t.Fatalf("custom coordinator prompt contains stale worker skill guidance %q:\n%s", stale, prompt)
		}
	}
	if !strings.Contains(prompt, "tell a worker to call `load_skill` only when that tool is explicitly granted to the worker") {
		t.Fatalf("custom coordinator prompt missing grant-only worker skill contract:\n%s", prompt)
	}
}

func TestAutoSkillCoordinatorPromptUsesGrantOnlyWorkerGuidance(t *testing.T) {
	c := &Coordinator{
		session: &TeamSession{
			Workspace: t.TempDir(),
			Config:    agent.TeamConfig{Name: "team"},
			Agents: map[string]*agent.AgentDef{
				"coordinator": {Name: "coordinator", Role: "coordinator"},
				"worker":      {Name: "worker", Role: "worker"},
			},
		},
		taskTracker:  NewTaskTracker(),
		reportStatus: func(StatusEvent) {},
	}

	prompt := c.BuildOrchestratorPrompt(&skill.SkillDef{
		Name:        "code-review",
		Description: "review workflow",
		Path:        "skills/code-review/SKILL.md",
		Content:     "FULL REVIEW INSTRUCTIONS",
	})
	marker := "## Auto-Loaded Skills"
	sectionStart := strings.Index(prompt, marker)
	if sectionStart < 0 {
		t.Fatalf("auto-skill section missing:\n%s", prompt)
	}
	section := prompt[sectionStart:]
	for _, stale := range []string{
		"so workers know which skills to load",
		"so workers can load them if needed",
	} {
		if strings.Contains(section, stale) {
			t.Fatalf("auto-skill section contains stale worker load guidance %q:\n%s", stale, section)
		}
	}
	for _, required := range []string{
		"Their full instructions are supplied to workers by default",
		"tell workers to call `load_skill` only when that tool is explicitly granted",
	} {
		if !strings.Contains(section, required) {
			t.Fatalf("auto-skill section missing grant-only guidance %q:\n%s", required, section)
		}
	}
}
