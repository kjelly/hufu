package team

import (
	"strings"
	"testing"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/skill"
)

func TestExtractSkillKeywords(t *testing.T) {
	tests := []struct {
		name     string
		skill    *skill.SkillDef
		contains []string
		excludes []string
	}{
		{
			name: "code-reviewer skill",
			skill: &skill.SkillDef{
				Name:        "code-reviewer",
				Description: "Use this skill to review code. It supports local changes and remote Pull Requests.",
			},
			contains: []string{"code", "reviewer", "code-reviewer", "code reviewer", "review", "pull"},
			excludes: []string{"the", "and", "it", "this", "both"},
		},
		{
			name: "git-commit skill",
			skill: &skill.SkillDef{
				Name:        "git-commit",
				Description: "Execute git commit with conventional commit message analysis.",
			},
			contains: []string{"git", "commit", "git-commit", "git commit", "conventional", "message", "analysis"},
			excludes: []string{"with"},
		},
		{
			name: "short name skill",
			skill: &skill.SkillDef{
				Name:        "go-reviewer",
				Description: "Expert code reviewer for idiomatic Go.",
			},
			contains: []string{"go-reviewer", "go reviewer", "reviewer", "expert"},
			excludes: []string{"go"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			keywords := extractSkillKeywords(tt.skill)
			kwMap := map[string]bool{}
			for _, kw := range keywords {
				kwMap[kw] = true
			}
			for _, want := range tt.contains {
				if !kwMap[want] {
					t.Errorf("expected keyword %q, got keywords: %v", want, keywords)
				}
			}
			for _, notWant := range tt.excludes {
				if kwMap[notWant] {
					t.Errorf("did not expect keyword %q, got keywords: %v", notWant, keywords)
				}
			}
		})
	}
}

func TestExtractSkillKeywordsDedup(t *testing.T) {
	s := &skill.SkillDef{
		Name:        "review",
		Description: "Review code review changes",
	}
	keywords := extractSkillKeywords(s)
	seen := map[string]bool{}
	for _, kw := range keywords {
		if seen[kw] {
			t.Errorf("duplicate keyword %q", kw)
		}
		seen[kw] = true
	}
}

func TestExtractSkillKeywordsFiltersShort(t *testing.T) {
	s := &skill.SkillDef{
		Name:        "ab",
		Description: "A big cat ran",
	}
	keywords := extractSkillKeywords(s)
	for _, kw := range keywords {
		if kw == "a" {
			t.Errorf("short keyword %q should be filtered", kw)
		}
	}
}

func TestMatchSkillsForPrompt(t *testing.T) {
	c := &Coordinator{
		skills: []*skill.SkillDef{
			{
				Name:        "code-reviewer",
				Description: "Use this skill to review code. It supports local changes and remote Pull Requests.",
			},
			{
				Name:        "git-commit",
				Description: "Execute git commit with conventional commit message analysis.",
			},
		},
	}

	tests := []struct {
		name         string
		prompt       string
		wantContains []string
		wantCount    int
	}{
		{
			name:         "English review matches code-reviewer",
			prompt:       "review the code changes",
			wantContains: []string{"code-reviewer"},
			wantCount:    1,
		},
		{
			name:         "commit keyword matches git-commit",
			prompt:       "please commit my changes",
			wantContains: []string{"git-commit"},
			wantCount:    2,
		},
		{
			name:         "pull request matches code-reviewer",
			prompt:       "review this Pull Request",
			wantContains: []string{"code-reviewer"},
			wantCount:    1,
		},
		{
			name:         "review and commit matches both",
			prompt:       "review git diff and commit the changes",
			wantContains: []string{"code-reviewer", "git-commit"},
			wantCount:    2,
		},
		{
			name:         "unrelated prompt matches nothing",
			prompt:       "what is the weather today",
			wantContains: nil,
			wantCount:    0,
		},
		{
			name:         "empty prompt matches nothing",
			prompt:       "",
			wantContains: nil,
			wantCount:    0,
		},
		{
			name:         "Chinese prompt with English keywords",
			prompt:       "幫我 review 最近5次commit的程式碼品質",
			wantContains: []string{"code-reviewer", "git-commit"},
			wantCount:    2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			matched := c.matchSkillsForPrompt(tt.prompt)
			if len(matched) != tt.wantCount {
				t.Errorf("got %d matches, want %d; matched names: %v", len(matched), tt.wantCount, skillNames(matched))
				return
			}
			gotNames := map[string]bool{}
			for _, s := range matched {
				gotNames[s.Name] = true
			}
			for _, want := range tt.wantContains {
				if !gotNames[want] {
					t.Errorf("expected match %q, got: %v", want, skillNames(matched))
				}
			}
		})
	}
}

func TestMatchSkillsForPromptEmpty(t *testing.T) {
	c := &Coordinator{}
	matched := c.matchSkillsForPrompt("review code")
	if matched != nil {
		t.Errorf("expected nil for empty skills, got %v", matched)
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

	text, names := c.buildSuggestedSkillsText(agentDef, "reviewer", "review the code changes")
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

	text, _ := c.buildSuggestedSkillsText(agentDef, "reviewer", "review the code changes")
	if text != "" {
		t.Errorf("expected empty text since code-reviewer is already in agent skills, got: %s", text)
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

	text, names := c.buildSuggestedSkillsText(agentDef, "reviewer", "review code")
	if text != "" {
		t.Errorf("expected empty text for no auto-loaded skills, got: %s", text)
	}
	if len(names) != 0 {
		t.Errorf("expected empty names, got %v", names)
	}
}

func TestBuildSuggestedSkillsTextRelevance(t *testing.T) {
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

	tests := []struct {
		name      string
		agentDef  *agent.AgentDef
		taskDesc  string
		wantSkill string
		skipSkill string
	}{
		{
			name: "reviewer agent with review task gets code-reviewer",
			agentDef: &agent.AgentDef{
				Name:        "reviewer",
				Description: "Code reviewer — audits quality",
			},
			taskDesc:  "review the latest commits for quality",
			wantSkill: "code-reviewer",
			skipSkill: "",
		},
		{
			name: "developer agent with commit task gets git-commit",
			agentDef: &agent.AgentDef{
				Name:        "developer",
				Description: "Implementation specialist — writes and commits code",
			},
			taskDesc:  "commit the changes",
			wantSkill: "git-commit",
			skipSkill: "code-reviewer",
		},
		{
			name: "unrelated agent and task gets nothing",
			agentDef: &agent.AgentDef{
				Name:        "designer",
				Description: "UI/UX designer",
			},
			taskDesc:  "create a new color palette",
			wantSkill: "",
			skipSkill: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := &Coordinator{
				reportStatus:     func(event StatusEvent) {},
				session:          &TeamSession{Config: agent.TeamConfig{Name: "test-team"}},
				skillUsage:       make(map[string]*skillUsageState),
				autoLoadedSkills: []*skill.SkillDef{codeReviewer, gitCommit},
			}

			text, names := c.buildSuggestedSkillsText(tt.agentDef, tt.agentDef.Name, tt.taskDesc)

			if tt.wantSkill == "" {
				if text != "" {
					t.Errorf("expected empty text for irrelevant agent+task, got: %s", text)
				}
				return
			}

			if !strings.Contains(text, tt.wantSkill) {
				t.Errorf("expected %q in suggestion text, but it was not found", tt.wantSkill)
			}

			if tt.skipSkill != "" && strings.Contains(text, "**"+tt.skipSkill+"**") {
				t.Errorf("did not expect %q in suggestion text, but it was found", tt.skipSkill)
			}

			found := false
			for _, n := range names {
				if n == tt.wantSkill {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("expected %q in names, got %v", tt.wantSkill, names)
			}
		})
	}
}
