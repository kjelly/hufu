package agent

import (
	"context"
	"testing"

	"charm.land/fantasy"
)

func assistantToolCall(input string) fantasy.Message {
	return fantasy.Message{Role: fantasy.MessageRoleAssistant, Content: []fantasy.MessagePart{
		fantasy.TextPart{Text: "dispatching"},
		fantasy.ToolCallPart{ToolCallID: "call-1", ToolName: "agent", Input: input},
	}}
}

func toolCallInput(t *testing.T, message fantasy.Message) string {
	t.Helper()
	for _, part := range message.Content {
		switch call := part.(type) {
		case fantasy.ToolCallPart:
			return call.Input
		case *fantasy.ToolCallPart:
			return call.Input
		}
	}
	t.Fatal("no tool call part")
	return ""
}

func TestSanitizeToolCallHistory(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{name: "valid object kept", input: `{"tasks":[{"agent":"executor","goal":"x"}]}`, want: `{"tasks":[{"agent":"executor","goal":"x"}]}`},
		{name: "whitespace around object kept", input: " {\"a\":1}\n", want: " {\"a\":1}\n"},
		{name: "two concatenated objects", input: `{"tasks":[]}{"options":[{"label":"List"}]}`, want: "{}"},
		{name: "trailing garbage", input: `{"a":1} trailing`, want: "{}"},
		{name: "truncated object", input: `{"a":`, want: "{}"},
		{name: "array", input: `[1,2]`, want: "{}"},
		{name: "null", input: `null`, want: "{}"},
		{name: "empty", input: ``, want: "{}"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			prompt := fantasy.Prompt{
				{Role: fantasy.MessageRoleUser, Content: []fantasy.MessagePart{fantasy.TextPart{Text: "plan"}}},
				assistantToolCall(test.input),
				{Role: fantasy.MessageRoleTool, Content: []fantasy.MessagePart{fantasy.ToolResultPart{ToolCallID: "call-1"}}},
			}
			got := sanitizeToolCallHistory(prompt)
			if gotInput := toolCallInput(t, got[1]); gotInput != test.want {
				t.Fatalf("input = %q, want %q", gotInput, test.want)
			}
			if toolCallInput(t, prompt[1]) != test.input {
				t.Fatal("the caller's prompt was modified")
			}
			if test.want == test.input && &got[0] != &prompt[0] {
				t.Fatal("a valid prompt was copied")
			}
			if len(got) != 3 || len(got[1].Content) != 2 || got[2].Role != fantasy.MessageRoleTool {
				t.Fatalf("history shape changed: %#v", got)
			}
		})
	}
}

func TestSanitizeToolCallHistoryHandlesPointerParts(t *testing.T) {
	prompt := fantasy.Prompt{{Role: fantasy.MessageRoleAssistant, Content: []fantasy.MessagePart{
		&fantasy.ToolCallPart{ToolCallID: "call-2", ToolName: "ask_user", Input: `{}{}`},
	}}}
	got := sanitizeToolCallHistory(prompt)
	if toolCallInput(t, got[0]) != "{}" || toolCallInput(t, prompt[0]) != `{}{}` {
		t.Fatalf("pointer part not sanitized without aliasing: %q / %q", toolCallInput(t, got[0]), toolCallInput(t, prompt[0]))
	}
}

// promptCapturingModel records the prompt each entry point receives.
type promptCapturingModel struct {
	countingLanguageModel
	prompts []fantasy.Prompt
}

func (m *promptCapturingModel) Generate(ctx context.Context, call fantasy.Call) (*fantasy.Response, error) {
	m.prompts = append(m.prompts, call.Prompt)
	return m.countingLanguageModel.Generate(ctx, call)
}

func (m *promptCapturingModel) Stream(ctx context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
	m.prompts = append(m.prompts, call.Prompt)
	return m.countingLanguageModel.Stream(ctx, call)
}

func (m *promptCapturingModel) GenerateObject(ctx context.Context, call fantasy.ObjectCall) (*fantasy.ObjectResponse, error) {
	m.prompts = append(m.prompts, call.Prompt)
	return m.countingLanguageModel.GenerateObject(ctx, call)
}

func (m *promptCapturingModel) StreamObject(ctx context.Context, call fantasy.ObjectCall) (fantasy.ObjectStreamResponse, error) {
	m.prompts = append(m.prompts, call.Prompt)
	return m.countingLanguageModel.StreamObject(ctx, call)
}

func TestAdmittedModelSendsSanitizedToolCallHistory(t *testing.T) {
	model := &promptCapturingModel{}
	events := []string{}
	wrapped := NewAdmittedLanguageModel("local/model", model, &recordingAdmission{events: &events})
	prompt := fantasy.Prompt{assistantToolCall(`{"tasks":[]}{"options":[]}`)}
	if _, err := wrapped.Generate(t.Context(), fantasy.Call{Prompt: prompt}); err != nil {
		t.Fatal(err)
	}
	if _, err := wrapped.Stream(t.Context(), fantasy.Call{Prompt: prompt}); err != nil {
		t.Fatal(err)
	}
	if _, err := wrapped.GenerateObject(t.Context(), fantasy.ObjectCall{Prompt: prompt}); err != nil {
		t.Fatal(err)
	}
	if _, err := wrapped.StreamObject(t.Context(), fantasy.ObjectCall{Prompt: prompt}); err != nil {
		t.Fatal(err)
	}
	if len(model.prompts) != 4 {
		t.Fatalf("provider calls = %d, want 4", len(model.prompts))
	}
	for index, sent := range model.prompts {
		if got := toolCallInput(t, sent[0]); got != "{}" {
			t.Errorf("method %d sent tool-call input %q, want {}", index, got)
		}
	}
}
