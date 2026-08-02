package sidecar

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"charm.land/fantasy"

	"github.com/anomalyco/hufu/internal/tools"
)

type usageAgent struct{}

func (usageAgent) Generate(context.Context, fantasy.AgentCall) (*fantasy.AgentResult, error) {
	return &fantasy.AgentResult{
		TotalUsage: fantasy.Usage{TotalTokens: 23},
		Response: fantasy.Response{Content: fantasy.ResponseContent{
			fantasy.TextContent{Text: "ok"},
		}},
	}, nil
}

func (usageAgent) Stream(context.Context, fantasy.AgentStreamCall) (*fantasy.AgentResult, error) {
	return nil, nil
}

func TestGenerateNotifiesUsageObserver(t *testing.T) {
	s := &Sidecar{agent: usageAgent{}}
	var observed int64
	s.SetUsageObserver(func(result *fantasy.AgentResult) {
		observed = result.TotalUsage.TotalTokens
	})

	got, err := s.generate(context.Background(), "prompt")
	if err != nil {
		t.Fatalf("generate() error = %v", err)
	}
	if got != "ok" {
		t.Fatalf("generate() = %q, want %q", got, "ok")
	}
	if observed != 23 {
		t.Fatalf("observer saw %d tokens, want 23", observed)
	}
}

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

func TestNormalizeAskUserSelection(t *testing.T) {
	opts := []tools.AskUserTUIOption{
		{Label: "Keep", Value: "keep"},
		{Label: "Remove", Value: "remove"},
	}

	resp, ok := normalizeAskUserSelection(tools.AskUserResponse{Answers: []string{"Remove"}}, opts, "single_choice", false)
	if !ok {
		t.Fatal("expected label-based selection to normalize")
	}
	if len(resp.Answers) != 1 || resp.Answers[0] != "remove" {
		t.Fatalf("unexpected normalized answers: %+v", resp.Answers)
	}

	resp, ok = normalizeAskUserSelection(tools.AskUserResponse{Answers: []string{"2"}}, opts, "single_choice", false)
	if !ok {
		t.Fatal("expected numeric selection to normalize")
	}
	if len(resp.Answers) != 1 || resp.Answers[0] != "remove" {
		t.Fatalf("unexpected normalized numeric answer: %+v", resp.Answers)
	}

	resp, ok = normalizeAskUserSelection(tools.AskUserResponse{Free: "custom"}, opts, "mixed", true)
	if !ok {
		t.Fatal("expected free-text fallback to normalize when allowed")
	}
	if resp.Free != "custom" {
		t.Fatalf("unexpected free text: %q", resp.Free)
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

func TestParseReviewToolCallResponse(t *testing.T) {
	tests := []struct {
		name         string
		response     string
		wantApproved bool
		wantReason   string
		wantErr      bool
	}{
		{
			name:         "approved true",
			response:     `{"approved": true, "reason": ""}`,
			wantApproved: true,
			wantReason:   "",
			wantErr:      false,
		},
		{
			name:         "approved false with reason",
			response:     `{"approved": false, "reason": "violates rule: no sudo"}`,
			wantApproved: false,
			wantReason:   "violates rule: no sudo",
			wantErr:      false,
		},
		{
			name:         "JSON in code block",
			response:     "```json\n{\"approved\": false, \"reason\": \"rm -rf\"}\n```",
			wantApproved: false,
			wantReason:   "rm -rf",
			wantErr:      false,
		},
		{
			name:     "invalid JSON",
			response: "not json",
			wantErr:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := parseReviewToolCallResponse(tt.response)
			if (err != nil) != tt.wantErr {
				t.Errorf("parseReviewToolCallResponse() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if err != nil {
				return
			}
			if result.Approved != tt.wantApproved {
				t.Errorf("Approved = %v, want %v", result.Approved, tt.wantApproved)
			}
			if result.Reason != tt.wantReason {
				t.Errorf("Reason = %q, want %q", result.Reason, tt.wantReason)
			}
		})
	}
}
