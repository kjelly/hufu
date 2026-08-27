package sidecar

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"charm.land/fantasy"

	"github.com/kjelly/hufu/internal/tools"
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

	got, err := s.generate(context.Background(), "prompt", ClassifierProfile)
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

func TestCompactStructuredTransientBypassesDurableHooksOnlyForItsInvocation(t *testing.T) {
	capture := &callCapturingAgent{response: "ok"}
	s := &Sidecar{agent: capture}
	var observed int
	s.SetUsageObserver(func(*fantasy.AgentResult) {
		observed++
	})
	mutations := struct {
		manifests  int
		sessions   int
		events     int
		selections int
		metrics    int
	}{}
	s.SetPromptPreparer(func(_ context.Context, _, prompt string) (string, error) {
		mutations.manifests++
		mutations.sessions++
		mutations.events++
		mutations.selections++
		mutations.metrics++
		return "prepared: " + prompt, nil
	})

	if _, err := s.CompactStructuredTransient(context.Background(), "history", "", "goal"); err != nil {
		t.Fatalf("transient compaction error = %v", err)
	}
	if observed != 0 {
		t.Fatalf("transient compaction notified observer %d times, want 0", observed)
	}
	if mutations.manifests != 0 || mutations.sessions != 0 || mutations.events != 0 || mutations.selections != 0 || mutations.metrics != 0 {
		t.Fatalf("transient compaction mutated durable projection state: %+v", mutations)
	}
	if _, err := s.CompactStructured(context.Background(), "history", "", "goal"); err != nil {
		t.Fatalf("durable compaction error = %v", err)
	}
	if observed != 1 {
		t.Fatalf("durable compaction notified observer %d times, want 1", observed)
	}
	if mutations.manifests != 1 || mutations.sessions != 1 || mutations.events != 1 || mutations.selections != 1 || mutations.metrics != 1 {
		t.Fatalf("durable compaction did not use the normal prompt-preparation boundary: %+v", mutations)
	}
	if !strings.HasPrefix(capture.captured.Prompt, "prepared: ") {
		t.Fatalf("durable compaction prompt was not prepared: %q", capture.captured.Prompt)
	}
}

type callCapturingAgent struct {
	captured fantasy.AgentCall
	response string
}

func (a *callCapturingAgent) Generate(_ context.Context, call fantasy.AgentCall) (*fantasy.AgentResult, error) {
	a.captured = call
	return &fantasy.AgentResult{
		Response: fantasy.Response{Content: fantasy.ResponseContent{fantasy.TextContent{Text: a.response}}},
	}, nil
}

func (a *callCapturingAgent) Stream(context.Context, fantasy.AgentStreamCall) (*fantasy.AgentResult, error) {
	return nil, nil
}

func TestGenerateAppliesProfile(t *testing.T) {
	capture := &callCapturingAgent{response: "ok"}
	s := &Sidecar{agent: capture}

	if _, err := s.generate(context.Background(), "prompt", CompactorProfile); err != nil {
		t.Fatalf("generate() error = %v", err)
	}
	if capture.captured.MaxOutputTokens == nil || *capture.captured.MaxOutputTokens != CompactorProfile.MaxOutputTokens {
		t.Errorf("MaxOutputTokens = %v, want %d", capture.captured.MaxOutputTokens, CompactorProfile.MaxOutputTokens)
	}
	if capture.captured.Temperature == nil || *capture.captured.Temperature != CompactorProfile.Temperature {
		t.Errorf("Temperature = %v, want %g", capture.captured.Temperature, CompactorProfile.Temperature)
	}
	if capture.captured.ProviderOptions == nil {
		t.Error("ProviderOptions not set for a profile with a reasoning effort")
	}
}

func TestExecuteDefaultsToClassifierProfile(t *testing.T) {
	capture := &callCapturingAgent{response: "ok"}
	s := &Sidecar{agent: capture}

	if _, err := s.Execute(context.Background(), "task"); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if capture.captured.MaxOutputTokens == nil || *capture.captured.MaxOutputTokens != ClassifierProfile.MaxOutputTokens {
		t.Errorf("Execute MaxOutputTokens = %v, want ClassifierProfile's %d", capture.captured.MaxOutputTokens, ClassifierProfile.MaxOutputTokens)
	}
}

func TestExecuteProfileOverridesDefault(t *testing.T) {
	capture := &callCapturingAgent{response: "ok"}
	s := &Sidecar{agent: capture}

	if _, err := s.ExecuteProfile(context.Background(), "task", JudgeProfile); err != nil {
		t.Fatalf("ExecuteProfile() error = %v", err)
	}
	if capture.captured.MaxOutputTokens == nil || *capture.captured.MaxOutputTokens != JudgeProfile.MaxOutputTokens {
		t.Errorf("ExecuteProfile MaxOutputTokens = %v, want JudgeProfile's %d", capture.captured.MaxOutputTokens, JudgeProfile.MaxOutputTokens)
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
