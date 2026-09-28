package team

import (
	"slices"
	"strings"
	"testing"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/skill"
)

func TestComputeRelevantSkillsUsesOnlyStructuredSelections(t *testing.T) {
	selected := []*skill.SkillDef{{Name: "code-reviewer"}, {Name: "git-commit"}}
	c := &Coordinator{autoLoadedSkills: selected}
	agentDef := &agent.AgentDef{Name: "worker", Role: "worker"}

	got := c.computeRelevantSkills(agentDef, "unrelated prose must not change selection")
	if names := skillNames(got); !slices.Equal(names, []string{"code-reviewer", "git-commit"}) {
		t.Fatalf("computeRelevantSkills() = %v", names)
	}
}

func skillNames(skills []*skill.SkillDef) []string {
	names := make([]string, len(skills))
	for i, s := range skills {
		names[i] = s.Name
	}
	return names
}

func TestBuildSuggestedSkillsTextNoOverlap(t *testing.T) {
	c := &Coordinator{
		reportStatus: func(event StatusEvent) {},
		session:      &TeamSession{Config: agent.TeamConfig{Name: "test-team"}},
		skillUsage:   make(map[string]*skillUsageState),
		autoLoadedSkills: []*skill.SkillDef{
			{
				Name:        "code-reviewer",
				Description: "Review code quality",
				Content:     "Review code for bugs, security, and style.",
			},
			{
				Name:        "git-commit",
				Description: "Commit changes with git",
				Content:     "Execute git commit with conventional messages.",
			},
		},
	}

	agentDef := &agent.AgentDef{
		Name:   "reviewer",
		Skills: "",
	}

	text, names := c.buildSuggestedSkillsText(agentDef, "reviewer", "review the code changes", map[string]bool{"load_skill": true})
	if text == "" {
		t.Fatal("expected non-empty suggestion text")
	}
	if !strings.Contains(text, "code-reviewer") {
		t.Error("expected code-reviewer in suggestion text")
	}
	if !strings.Contains(text, "load_skill") {
		t.Error("expected 'load_skill' mention in suggestion text")
	}
	if len(names) == 0 {
		t.Error("expected at least one skill name")
	}
	found := false
	for _, n := range names {
		if n == "code-reviewer" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected code-reviewer in names, got %v", names)
	}
}

func TestBuildSuggestedSkillsTextWithOverlap(t *testing.T) {
	c := &Coordinator{
		reportStatus: func(event StatusEvent) {},
		session:      &TeamSession{Config: agent.TeamConfig{Name: "test-team"}},
		skillUsage:   make(map[string]*skillUsageState),
		autoLoadedSkills: []*skill.SkillDef{
			{
				Name:    "code-reviewer",
				Content: "Review code for bugs, security, and style.",
			},
			{
				Name:    "git-commit",
				Content: "Execute git commit with conventional messages.",
			},
		},
	}

	agentDef := &agent.AgentDef{
		Name:   "reviewer",
		Skills: "code-reviewer",
	}

	text, _ := c.buildSuggestedSkillsText(agentDef, "reviewer", "review the code changes", map[string]bool{"load_skill": true})
	if strings.Contains(text, "**code-reviewer**") || !strings.Contains(text, "**git-commit**") {
		t.Errorf("expected only the remaining structured-selected skill, got: %s", text)
	}
}

func TestBuildSuggestedSkillsTextEmpty(t *testing.T) {
	c := &Coordinator{
		reportStatus:     func(event StatusEvent) {},
		session:          &TeamSession{Config: agent.TeamConfig{Name: "test-team"}},
		skillUsage:       make(map[string]*skillUsageState),
		autoLoadedSkills: nil,
	}

	agentDef := &agent.AgentDef{
		Name: "reviewer",
	}

	text, names := c.buildSuggestedSkillsText(agentDef, "reviewer", "review code", map[string]bool{"load_skill": true})
	if text != "" {
		t.Errorf("expected empty text for no auto-loaded skills, got: %s", text)
	}
	if len(names) != 0 {
		t.Errorf("expected empty names, got %v", names)
	}
}

func TestBuildSuggestedSkillsTextDoesNotInferRelevanceFromProse(t *testing.T) {
	codeReviewer := &skill.SkillDef{
		Name:        "code-reviewer",
		Description: "Review code quality",
		Content:     "Review code for bugs, security, and style.",
	}
	gitCommit := &skill.SkillDef{
		Name:        "git-commit",
		Description: "Commit changes with git",
		Content:     "Execute git commit with conventional messages.",
	}

	c := &Coordinator{
		reportStatus:     func(event StatusEvent) {},
		session:          &TeamSession{Config: agent.TeamConfig{Name: "test-team"}},
		skillUsage:       make(map[string]*skillUsageState),
		autoLoadedSkills: []*skill.SkillDef{codeReviewer, gitCommit},
	}
	agentDef := &agent.AgentDef{Name: "designer", Description: "UI/UX designer"}
	_, first := c.buildSuggestedSkillsText(agentDef, agentDef.Name, "create a color palette", map[string]bool{"load_skill": true})
	_, second := c.buildSuggestedSkillsText(agentDef, agentDef.Name, "commit and review code", map[string]bool{"load_skill": true})
	want := []string{"code-reviewer", "git-commit"}
	if !slices.Equal(first, want) || !slices.Equal(second, want) {
		t.Fatalf("structured selections changed with prose: first=%v second=%v want=%v", first, second, want)
	}
}
