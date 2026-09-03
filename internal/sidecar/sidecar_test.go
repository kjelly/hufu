package sidecar

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"charm.land/fantasy"

	"github.com/kjelly/hufu/internal/agent"
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

type errorAgent struct{ err error }

type boundSnapshotKey struct{}

type sidecarRecordingLanguageModel struct {
	fantasy.LanguageModel
	calls    int
	lastCall fantasy.Call
}

func (m *sidecarRecordingLanguageModel) Generate(_ context.Context, call fantasy.Call) (*fantasy.Response, error) {
	m.calls++
	m.lastCall = call
	return &fantasy.Response{Content: fantasy.ResponseContent{fantasy.TextContent{Text: "ok"}}}, nil
}

func (m *sidecarRecordingLanguageModel) Provider() string { return "test-provider" }

func (m *sidecarRecordingLanguageModel) Model() string { return "test-model" }

type sidecarRecordingAdmission struct {
	contexts []agent.ProviderAdmissionContext
}

func (a *sidecarRecordingAdmission) AdmitProviderRequest(_ context.Context, request agent.ProviderRequest) error {
	a.contexts = append(a.contexts, request.AdmissionContext)
	return nil
}

func (a errorAgent) Generate(context.Context, fantasy.AgentCall) (*fantasy.AgentResult, error) {
	return nil, a.err
}

func (a errorAgent) Stream(context.Context, fantasy.AgentStreamCall) (*fantasy.AgentResult, error) {
	return nil, a.err
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

func TestGenerateNotifiesErrorObserverOnlyForFailedGeneration(t *testing.T) {
	var observed []error
	observe := func(_ context.Context, err error) { observed = append(observed, err) }

	success := &Sidecar{agent: usageAgent{}}
	success.SetErrorObserver(observe)
	if _, err := success.generate(t.Context(), "prompt", ClassifierProfile); err != nil {
		t.Fatal(err)
	}
	if len(observed) != 0 {
		t.Fatalf("successful generation notified errors = %d, want zero", len(observed))
	}

	failure := errors.New("provider rejected request")
	failed := &Sidecar{agent: errorAgent{err: failure}}
	failed.SetErrorObserver(observe)
	if _, err := failed.generate(t.Context(), "prompt", ClassifierProfile); !errors.Is(err, failure) {
		t.Fatalf("failed generation error = %v, want %v", err, failure)
	}
	if len(observed) != 1 || !errors.Is(observed[0], failure) {
		t.Fatalf("failed generation notifications = %v, want one provider error", observed)
	}
}

type cancelingErrorAgent struct {
	cancel context.CancelFunc
	err    error
}

func (a cancelingErrorAgent) Generate(ctx context.Context, _ fantasy.AgentCall) (*fantasy.AgentResult, error) {
	a.cancel()
	return nil, a.err
}

func (a cancelingErrorAgent) Stream(context.Context, fantasy.AgentStreamCall) (*fantasy.AgentResult, error) {
	return nil, a.err
}

func TestGenerateErrorObserverReceivesCanceledGenerationContext(t *testing.T) {
	type contextMarkerKey struct{}
	providerErr := errors.New("provider rejected request")
	parent, cancel := context.WithCancel(t.Context())
	parent = context.WithValue(parent, contextMarkerKey{}, "generation")

	var observed context.Context
	s := &Sidecar{agent: cancelingErrorAgent{cancel: cancel, err: providerErr}}
	s.SetErrorObserver(func(ctx context.Context, err error) {
		if !errors.Is(err, providerErr) {
			t.Errorf("observer error = %v, want provider error", err)
		}
		observed = ctx
	})

	if _, err := s.generate(parent, "prompt", ClassifierProfile); !errors.Is(err, providerErr) {
		t.Fatalf("generate() error = %v, want provider error", err)
	}
	if observed == nil {
		t.Fatal("error observer did not receive generation context")
	}
	if !errors.Is(observed.Err(), context.Canceled) {
		t.Fatalf("observer context error = %v, want context cancellation", observed.Err())
	}
	if got := observed.Value(contextMarkerKey{}); got != "generation" {
		t.Fatalf("observer context marker = %v, want generation", got)
	}
}

func TestCompactStructuredTransientUsesProjectionHookWithoutDurableEffects(t *testing.T) {
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
	s.SetProjectionPromptPreparer(func(_ context.Context, _, prompt string) (string, error) {
		return "projection-prepared: " + prompt, nil
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
	if !strings.HasPrefix(capture.captured.Prompt, "projection-prepared: ") {
		t.Fatalf("transient compaction prompt was not projection-prepared: %q", capture.captured.Prompt)
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

func TestCompactStructuredTransientBindsBeforeProjectionPreparation(t *testing.T) {
	capture := &callCapturingAgent{response: "ok"}
	s := &Sidecar{agent: capture, modelID: "bound-sidecar"}
	s.SetInvocationBinder(func(ctx context.Context, modelID string) (context.Context, agent.ProviderAdmissionContext, error) {
		if modelID != "bound-sidecar" {
			t.Fatalf("binder model = %q, want bound-sidecar", modelID)
		}
		ctx = context.WithValue(ctx, boundSnapshotKey{}, true)
		return ctx, agent.ProviderAdmissionContext{ModelID: modelID, Bound: true}, nil
	})
	s.SetProjectionPromptPreparer(func(ctx context.Context, _, prompt string) (string, error) {
		if got, _ := ctx.Value(boundSnapshotKey{}).(bool); !got {
			t.Fatal("projection preparation ran before invocation binding")
		}
		return "compiled: " + prompt, nil
	})

	if _, err := s.CompactStructuredTransient(t.Context(), "history", "", "goal"); err != nil {
		t.Fatalf("transient compaction error = %v", err)
	}
	if !strings.HasPrefix(capture.captured.Prompt, "compiled: ") {
		t.Fatalf("transient prompt = %q, want compiled prompt", capture.captured.Prompt)
	}
}

func TestGenerateRebindsCachedLanguageModelForInvocation(t *testing.T) {
	b0 := agent.ProviderAdmissionContext{
		ModelID:             "sidecar-model",
		ProviderIdentity:    "provider-b0",
		ProviderBaseURL:     "https://b0.example/v1",
		Bound:               true,
		ContextWindow:       4_096,
		MaxOutputTokens:     256,
		SafetyMarginTokens:  64,
		ContextWindowSource: "provider_runtime",
	}
	b1 := agent.ProviderAdmissionContext{
		ModelID:             "sidecar-model",
		ProviderIdentity:    "provider-b1",
		ProviderBaseURL:     "https://b1.example/v1",
		Bound:               true,
		ContextWindow:       8_192,
		MaxOutputTokens:     512,
		SafetyMarginTokens:  128,
		ContextWindowSource: "provider_runtime",
	}
	underlying := &sidecarRecordingLanguageModel{}
	admission := &sidecarRecordingAdmission{}
	cached := agent.NewAdmittedLanguageModelWithContext("sidecar-model", underlying, admission, b0)
	s := &Sidecar{
		agent:            fantasy.NewAgent(cached),
		languageModel:    underlying,
		modelID:          "sidecar-model",
		requestAdmission: admission,
		admissionContext: b0,
	}
	var prepared agent.ProviderAdmissionContext
	s.SetInvocationBinder(func(ctx context.Context, modelID string) (context.Context, agent.ProviderAdmissionContext, error) {
		if modelID != "sidecar-model" {
			t.Fatalf("binder model = %q, want sidecar-model", modelID)
		}
		return ctx, b1, nil
	})
	s.SetPromptPreparer(func(ctx context.Context, _, prompt string) (string, error) {
		var ok bool
		prepared, ok = InvocationAdmissionContextFromContext(ctx)
		if !ok {
			t.Fatal("prompt preparation did not receive an invocation admission context")
		}
		return prompt, nil
	})

	if _, err := s.generate(t.Context(), "prompt", ClassifierProfile); err != nil {
		t.Fatalf("generate() error = %v", err)
	}
	if underlying.calls != 1 {
		t.Fatalf("underlying language model calls = %d, want 1", underlying.calls)
	}
	if prepared != b1 {
		t.Fatalf("prompt preparation context = %#v, want B1 %#v", prepared, b1)
	}
	if len(admission.contexts) != 1 || admission.contexts[0] != b1 {
		t.Fatalf("admission contexts = %#v, want one B1 context %#v", admission.contexts, b1)
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

func TestGenerateProvidesProfileOutputReservationToInvocationBinder(t *testing.T) {
	capture := &callCapturingAgent{response: "ok"}
	s := &Sidecar{agent: capture, modelID: "profile-bound-sidecar"}
	reservations := make([]int, 0, 3)
	s.SetInvocationBinder(func(ctx context.Context, modelID string) (context.Context, agent.ProviderAdmissionContext, error) {
		if modelID != "profile-bound-sidecar" {
			t.Fatalf("binder model = %q, want profile-bound-sidecar", modelID)
		}
		reservation, ok := OutputReservationFromContext(ctx)
		if !ok {
			t.Fatal("invocation binder did not receive an output reservation")
		}
		reservations = append(reservations, reservation)
		return ctx, agent.ProviderAdmissionContext{ModelID: modelID, Bound: true, ContextWindow: 100_000}, nil
	})
	for _, profile := range []Profile{ClassifierProfile, CompactorProfile, JudgeProfile} {
		if _, err := s.generate(t.Context(), "prompt", profile); err != nil {
			t.Fatalf("generate(%+v) error = %v", profile, err)
		}
	}
	want := []int{int(ClassifierProfile.MaxOutputTokens), int(CompactorProfile.MaxOutputTokens), int(JudgeProfile.MaxOutputTokens)}
	if !slices.Equal(reservations, want) {
		t.Fatalf("binder output reservations = %v, want %v", reservations, want)
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
