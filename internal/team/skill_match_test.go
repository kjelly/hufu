package team

import (
	"testing"

	"github.com/anomalyco/hufu/internal/skill"
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
