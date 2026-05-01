package sidecar

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

type mockAgent struct {
	response string
	err      error
}

func (m *mockAgent) Generate(ctx context.Context, call interface{}) (interface{}, error) {
	return m, m.err
}

func TestMatchSkillsJSON(t *testing.T) {
	tests := []struct {
		name     string
		response string
		skills   []SkillSummary
		want     []string
		wantErr  bool
	}{
		{
			name:     "valid JSON array",
			response: `["code-reviewer", "git-commit"]`,
			skills: []SkillSummary{
				{Name: "code-reviewer", Description: "Review code"},
				{Name: "git-commit", Description: "Commit changes"},
			},
			want:    []string{"code-reviewer", "git-commit"},
			wantErr: false,
		},
		{
			name:     "JSON in markdown code block",
			response: "```json\n[\"code-reviewer\"]\n```",
			skills: []SkillSummary{
				{Name: "code-reviewer", Description: "Review code"},
				{Name: "git-commit", Description: "Commit changes"},
			},
			want:    []string{"code-reviewer"},
			wantErr: false,
		},
		{
			name:     "JSON in code block without language",
			response: "```\n[\"git-commit\"]\n```",
			skills: []SkillSummary{
				{Name: "code-reviewer", Description: "Review code"},
				{Name: "git-commit", Description: "Commit changes"},
			},
			want:    []string{"git-commit"},
			wantErr: false,
		},
		{
			name:     "empty array",
			response: `[]`,
			skills: []SkillSummary{
				{Name: "code-reviewer", Description: "Review code"},
			},
			want:    nil,
			wantErr: false,
		},
		{
			name:     "invalid JSON",
			response: `not a json array`,
			skills: []SkillSummary{
				{Name: "code-reviewer", Description: "Review code"},
			},
			wantErr: true,
		},
		{
			name:     "unknown skill names filtered",
			response: `["code-reviewer", "unknown-skill"]`,
			skills: []SkillSummary{
				{Name: "code-reviewer", Description: "Review code"},
			},
			want:    []string{"code-reviewer"},
			wantErr: false,
		},
		{
			name:     "all unknown names returns empty",
			response: `["unknown-skill", "another-unknown"]`,
			skills: []SkillSummary{
				{Name: "code-reviewer", Description: "Review code"},
			},
			want:    nil,
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			names, err := parseMatchSkillsResponse(tt.response, tt.skills)
			if (err != nil) != tt.wantErr {
				t.Errorf("parseMatchSkillsResponse() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if len(names) != len(tt.want) {
				t.Errorf("got %v, want %v", names, tt.want)
				return
			}
			for i, name := range names {
				if name != tt.want[i] {
					t.Errorf("names[%d] = %q, want %q", i, name, tt.want[i])
				}
			}
		})
	}
}

func parseMatchSkillsResponse(response string, skills []SkillSummary) ([]string, error) {
	result := strings.TrimSpace(response)

	extracted := jsonCodeBlockRe.FindStringSubmatch(result)
	if len(extracted) >= 2 {
		result = strings.TrimSpace(extracted[1])
	}

	var names []string
	if err := json.Unmarshal([]byte(result), &names); err != nil {
		return nil, err
	}

	validMap := map[string]bool{}
	for _, sk := range skills {
		validMap[strings.ToLower(sk.Name)] = true
	}
	var filtered []string
	for _, name := range names {
		if validMap[strings.ToLower(strings.TrimSpace(name))] {
			filtered = append(filtered, strings.TrimSpace(name))
		}
	}
	return filtered, nil
}
