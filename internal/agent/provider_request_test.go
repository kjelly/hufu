package agent

import (
	"context"
	"errors"
	"strings"
	"testing"

	"charm.land/fantasy"
)

type rejectingAdmission struct {
	calls int
	err   error
}

func (a *rejectingAdmission) AdmitProviderRequest(context.Context, ProviderRequest) error {
	a.calls++
	return a.err
}

type countingLanguageModel struct {
	calls int
}

func (m *countingLanguageModel) Generate(context.Context, fantasy.Call) (*fantasy.Response, error) {
	m.calls++
	return &fantasy.Response{}, nil
}

func (m *countingLanguageModel) Stream(context.Context, fantasy.Call) (fantasy.StreamResponse, error) {
	m.calls++
	return nil, nil
}

func (m *countingLanguageModel) GenerateObject(context.Context, fantasy.ObjectCall) (*fantasy.ObjectResponse, error) {
	m.calls++
	return &fantasy.ObjectResponse{}, nil
}

func (m *countingLanguageModel) StreamObject(context.Context, fantasy.ObjectCall) (fantasy.ObjectStreamResponse, error) {
	m.calls++
	return nil, nil
}

func (*countingLanguageModel) Provider() string { return "test" }

func (*countingLanguageModel) Model() string { return "test-model" }

func TestAdmissionRejectsOversizedWorkerBeforeProvider(t *testing.T) {
	assertAdmissionRejectsBeforeProvider(t)
}

func TestAdmissionRejectsOversizedDirectAgentBeforeProvider(t *testing.T) {
	assertAdmissionRejectsBeforeProvider(t)
}

func TestAdmissionRejectsOversizedRepairBeforeProvider(t *testing.T) {
	assertAdmissionRejectsBeforeProvider(t)
}

func TestAdmissionRejectsOversizedSidecarBeforeProvider(t *testing.T) {
	assertAdmissionRejectsBeforeProvider(t)
}

func assertAdmissionRejectsBeforeProvider(t *testing.T) {
	t.Helper()
	providerErr := errors.New("context request cannot fit")
	admission := &rejectingAdmission{err: providerErr}
	model := &countingLanguageModel{}
	wrapped := NewAdmittedLanguageModel("test-model", model, admission)
	_, err := wrapped.Generate(context.Background(), fantasy.Call{Prompt: fantasy.Prompt{fantasy.NewUserMessage("large request")}})
	if !errors.Is(err, providerErr) {
		t.Fatalf("Generate error = %v, want admission error", err)
	}
	if admission.calls != 1 || model.calls != 0 {
		t.Fatalf("admission calls=%d provider calls=%d, want 1 and 0", admission.calls, model.calls)
	}
}

func TestAdmissionCountsSerializedToolsAndMultimodalParts(t *testing.T) {
	request := ProviderRequest{
		ModelID: "qwen3:8b",
		Call: &fantasy.Call{
			Prompt: fantasy.Prompt{{Role: fantasy.MessageRoleUser, Content: []fantasy.MessagePart{
				fantasy.TextPart{Text: "inspect this image"},
				fantasy.FilePart{Filename: "image.png", MediaType: "image/png", Data: []byte("binary-image-data")},
			}}},
			Tools: []fantasy.Tool{fantasy.FunctionTool{
				Name:        "large_tool",
				Description: "a tool with a schema",
				InputSchema: map[string]any{"type": "object", "properties": map[string]any{"value": map[string]any{"type": "string"}}},
			}},
		},
	}
	serialized, err := SerializeProviderRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	wire := string(serialized)
	for _, expected := range []string{"large_tool", "properties", "image_url", "YmluYXJ5LWltYWdlLWRhdGE="} {
		if !strings.Contains(wire, expected) {
			t.Fatalf("serialized request lacks %q: %s", expected, wire)
		}
	}
	textOnly := request
	textOnly.Call = &fantasy.Call{Prompt: fantasy.Prompt{fantasy.NewUserMessage("inspect this image")}}
	textTokens, err := CountProviderRequestTokens(textOnly)
	if err != nil {
		t.Fatal(err)
	}
	fullTokens, err := CountProviderRequestTokens(request)
	if err != nil {
		t.Fatal(err)
	}
	if fullTokens <= textTokens {
		t.Fatalf("full serialized request tokens=%d, text-only=%d; tool/media were not counted", fullTokens, textTokens)
	}
}
