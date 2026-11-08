package agent

import (
	"context"
	json "encoding/json/v2"
	"errors"
	"io"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"charm.land/fantasy"
	"charm.land/fantasy/providers/openai"
	"charm.land/fantasy/schema"
	"github.com/kjelly/hufu/internal/config"
)

func responsesFixtureModel(t *testing.T, modelID string, handler roundTripFunc) fantasy.LanguageModel {
	t.Helper()
	manager, err := NewProviderManager("https://responses.invalid/v1", "", map[string]config.ProviderConfig{
		"remote": {ProviderURL: "https://responses.invalid/v1", APIProtocol: config.APIProtocolResponses},
	})
	if err != nil {
		t.Fatal(err)
	}
	provider := manager.GetProvider("remote/" + modelID)
	provider.setBoundary("https://responses.invalid/v1", &http.Client{Transport: handler})
	model, err := provider.LanguageModel(t.Context(), "remote/"+modelID)
	if err != nil {
		t.Fatal(err)
	}
	return model
}

func TestResponsesProviderErrorsPreserveCauseAndProtocol(t *testing.T) {
	for _, entry := range []string{"generate", "stream", "generate_object", "stream_object"} {
		t.Run(entry, func(t *testing.T) {
			calls := 0
			model := responsesFixtureModel(t, "custom", func(*http.Request) (*http.Response, error) {
				calls++
				return sdkJSONResponse(`{"error":{"message":"not found","type":"invalid_request_error","code":"not_found"}}`, http.StatusNotFound), nil
			})
			err := invokeResponsesFixture(t.Context(), model, entry)
			wrapped, ok := errors.AsType[*responsesAdapterError](err)
			if !ok || errors.Unwrap(wrapped) == nil || !strings.Contains(err.Error(), "api-protocol responses") || !strings.Contains(err.Error(), "generation settings") || calls != 1 {
				t.Fatalf("error lost protocol/cause or retried: calls=%d error=%v", calls, err)
			}
		})
	}
}

func TestResponsesRejectsUnsupportedBeforeInvocation(t *testing.T) {
	for _, entry := range []string{"generate", "stream", "generate_object", "stream_object"} {
		for _, tc := range []struct {
			name, model string
			options     fantasy.ProviderOptions
			prompt      fantasy.Prompt
			binding     string
			want        error
		}{
			{name: "unknown reasoning model", model: "custom", options: fantasy.ProviderOptions{openai.Name: &openai.ResponsesProviderOptions{ReasoningEffort: new(openai.ReasoningEffortMedium)}}, want: ErrResponsesSetting},
			{name: "gpt6 reasoning limitation", model: "gpt-6", options: fantasy.ProviderOptions{openai.Name: &openai.ResponsesProviderOptions{ReasoningEffort: new(openai.ReasoningEffortMedium)}}, want: ErrResponsesSetting},
			{name: "store", model: "gpt-5", options: fantasy.ProviderOptions{openai.Name: &openai.ResponsesProviderOptions{Store: new(true)}}, want: ErrResponsesSetting},
			{name: "previous response", model: "gpt-5", options: fantasy.ProviderOptions{openai.Name: &openai.ResponsesProviderOptions{PreviousResponseID: new("previous")}}, want: ErrResponsesSetting},
			{name: "wrong options key", model: "gpt-5", options: fantasy.ProviderOptions{"remote": &openai.ResponsesProviderOptions{ReasoningEffort: new(openai.ReasoningEffortMedium)}}, want: ErrResponsesSetting},
			{name: "image", model: "custom", prompt: fantasy.Prompt{{Role: fantasy.MessageRoleUser, Content: []fantasy.MessagePart{fantasy.FilePart{MediaType: "image/png", Data: []byte("image")}}}}, want: ErrResponsesContent},
			{name: "tool media", model: "custom", prompt: fantasy.Prompt{{Role: fantasy.MessageRoleTool, Content: []fantasy.MessagePart{fantasy.ToolResultPart{ToolCallID: "call", Output: fantasy.ToolResultOutputContentMedia{}}}}}, want: ErrResponsesContent},
			{name: "binding drift", model: "custom", binding: config.APIProtocolChatCompletions, want: ErrProtocolBinding},
		} {
			t.Run(entry+"/"+tc.name, func(t *testing.T) {
				transportCalls := 0
				inner := responsesFixtureModel(t, tc.model, func(*http.Request) (*http.Response, error) { transportCalls++; return sdkJSONResponse(`{}`, 200), nil })
				admission := &recordingAdmission{}
				bound := ProviderAdmissionContext{}
				if tc.binding != "" {
					bound.APIProtocol, _ = ResolveAPIProtocol(tc.binding)
				}
				model := NewAdmittedLanguageModelWithContext("remote/"+tc.model, inner, admission, bound)
				prompt := tc.prompt
				if prompt == nil {
					prompt = fantasy.Prompt{fantasy.NewUserMessage("question")}
				}
				// Sanitization requires a paired call to preserve the invalid media result.
				if tc.name == "tool media" {
					prompt = append(fantasy.Prompt{{Role: fantasy.MessageRoleAssistant, Content: []fantasy.MessagePart{fantasy.ToolCallPart{ToolCallID: "call", ToolName: "lookup", Input: "{}"}}}}, prompt...)
				}
				call := fantasy.Call{Prompt: prompt, ProviderOptions: tc.options}
				object := fantasy.ObjectCall{Prompt: prompt, ProviderOptions: tc.options, Schema: schema.Generate(reflect.TypeFor[struct {
					Value string `json:"value"`
				}]())}
				var err error
				switch entry {
				case "generate":
					_, err = model.Generate(t.Context(), call)
				case "stream":
					_, err = model.Stream(t.Context(), call)
				case "generate_object":
					_, err = model.GenerateObject(t.Context(), object)
				case "stream_object":
					_, err = model.StreamObject(t.Context(), object)
				}
				if !errors.Is(err, tc.want) || transportCalls != 0 || len(admission.requests) != 0 || len(admission.commits) != 0 {
					t.Fatalf("err=%v calls=%d admitted=%d committed=%d", err, transportCalls, len(admission.requests), len(admission.commits))
				}
			})
		}
	}
}

func TestResponsesReasoningOptionsSurviveWrappers(t *testing.T) {
	var captured map[string]any
	inner := responsesFixtureModel(t, "gpt-5", func(request *http.Request) (*http.Response, error) {
		raw, err := io.ReadAll(request.Body)
		if err != nil {
			return nil, err
		}
		if err = json.Unmarshal(raw, &captured); err != nil {
			return nil, err
		}
		return sdkJSONResponse(`{"status":"completed","output":[{"type":"function_call","id":"fc","call_id":"c","name":"return_result","arguments":"{\"result\":{\"value\":\"ok\"}}"}]}`, 200), nil
	})
	admitted := NewAdmittedLanguageModel("remote/gpt-5", inner, &recordingAdmission{})
	wrapped, err := newStructuredResponseModel(admitted, &StructuredResponseConfig{Schema: map[string]any{"type": "object"}, MaxBytes: 1024})
	if err != nil {
		t.Fatal(err)
	}
	old := &openai.ResponsesProviderOptions{Store: new(false), ReasoningSummary: new("auto")}
	existing := fantasy.ProviderOptions{openai.Name: old, "unrelated": &openai.ProviderOptions{}}
	options := MergeReasoningEffortProviderOptions(wrapped, existing, "none")
	got, ok := options[openai.Name].(*openai.ResponsesProviderOptions)
	if !ok || got == old || old.ReasoningEffort != nil || got.ReasoningSummary != old.ReasoningSummary || options["unrelated"] != existing["unrelated"] {
		t.Fatalf("options=%v original=%+v", options, old)
	}
	if ModelAPIProtocol(wrapped).Effective != config.APIProtocolResponses {
		t.Fatal("wrapper lost transport identity")
	}
	_, err = wrapped.Generate(t.Context(), fantasy.Call{Prompt: fantasy.Prompt{fantasy.NewUserMessage("question")}, ProviderOptions: options})
	if err != nil {
		t.Fatal(err)
	}
	reasoning, _ := captured["reasoning"].(map[string]any)
	if reasoning["effort"] != "none" || reasoning["summary"] != "auto" || captured["store"] != false {
		t.Fatalf("wire options=%v", captured)
	}
}

func TestResponsesEstimateCoversCapturedWire(t *testing.T) {
	for _, object := range []bool{false, true} {
		for _, text := range []string{"", strings.Repeat("中文🙂\\\"", 512), strings.Repeat("<>&\u2028\u2029", 512)} {
			t.Run(map[bool]string{false: "call", true: "object"}[object]+"/"+strconv.Itoa(len(text)), func(t *testing.T) {
				var wire map[string]any
				var wireBytes int
				model := responsesFixtureModel(t, "custom", func(request *http.Request) (*http.Response, error) {
					raw, err := io.ReadAll(request.Body)
					if err != nil {
						return nil, err
					}
					wireBytes = len(raw)
					if err = json.Unmarshal(raw, &wire); err != nil {
						return nil, err
					}
					if object {
						return sdkJSONResponse(`{"status":"completed","output":[{"type":"function_call","id":"fc","call_id":"c","name":"generate_object","arguments":"{\"value\":\"ok\"}"}]}`, 200), nil
					}
					return sdkJSONResponse(`{"status":"completed","output":[]}`, 200), nil
				})
				prompt := fantasy.Prompt{fantasy.NewSystemMessage("instructions"), fantasy.NewUserMessage(text),
					{Role: fantasy.MessageRoleAssistant, Content: []fantasy.MessagePart{fantasy.ToolCallPart{ToolCallID: "call", ToolName: "lookup", Input: `{"value":"` + strings.Repeat("argument", 20) + `"}`}}},
					{Role: fantasy.MessageRoleTool, Content: []fantasy.MessagePart{fantasy.ToolResultPart{ToolCallID: "call", Output: fantasy.ToolResultOutputContentText{Text: "result-marker" + text}}}},
					fantasy.NewUserMessage("next"),
				}
				request := ProviderRequest{ModelID: "remote/custom", AdmissionContext: ProviderAdmissionContext{APIProtocol: ModelAPIProtocol(model)}}
				if object {
					call := fantasy.ObjectCall{Prompt: prompt, Schema: schema.Generate(reflect.TypeFor[struct {
						Value string `json:"value"`
					}]())}
					request.ObjectCall = &call
					if _, err := model.GenerateObject(t.Context(), call); err != nil {
						t.Fatal(err)
					}
				} else {
					tools := make([]fantasy.Tool, 30)
					for i := range tools {
						tools[i] = fantasy.FunctionTool{Name: "tool_" + strings.Repeat("x", i), Description: "description-marker" + text, InputSchema: map[string]any{"type": "object", "properties": map[string]any{"value": map[string]any{"type": "string", "description": "schema-marker" + text}}}}
					}
					call := fantasy.Call{Prompt: prompt, Tools: tools}
					request.Call = &call
					if _, err := model.Generate(t.Context(), call); err != nil {
						t.Fatal(err)
					}
				}
				estimate, err := SerializeProviderRequest(request)
				if err != nil {
					t.Fatal(err)
				}
				tokens, err := CountProviderRequestTokens(request)
				if err != nil {
					t.Fatal(err)
				}
				// Actual wire JSON already contains item/tool framing. Its bytes plus
				// the base margin are the independent oracle. Do not re-marshal the
				// decoded body: that can hide differences in SDK string escaping.
				if tokens < wireBytes+responsesBaseAllowance || tokens != ProviderRequestTokenFloor(request, estimate) {
					t.Fatalf("tokens=%d actual bytes=%d estimate bytes=%d", tokens, wireBytes, len(estimate))
				}
				for _, marker := range []string{"instructions", "result-marker", "lookup"} {
					if !strings.Contains(string(estimate), marker) {
						t.Fatalf("missing %q", marker)
					}
				}
			})
		}
	}
}

func TestProviderRequestInspectionPreservesOwnedCall(t *testing.T) {
	call := fantasy.Call{Prompt: fantasy.Prompt{fantasy.NewUserMessage("question")}, Tools: []fantasy.Tool{fantasy.FunctionTool{Name: "original", InputSchema: map[string]any{"type": "object"}}}}
	extra := fantasy.NewAgentTool("extra", "extra tool", func(context.Context, struct{}, fantasy.ToolCall) (fantasy.ToolResponse, error) {
		t.Fatal("admission executed a tool")
		return fantasy.NewTextResponse(""), nil
	})
	request := ProviderRequest{ModelID: "m", Call: &call, Tools: []fantasy.AgentTool{extra}}
	for range 2 {
		encoded, err := SerializeProviderRequest(request)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Count(string(encoded), `"name":"extra"`) != 1 {
			t.Fatalf("tool inventory=%s", encoded)
		}
	}
	if len(call.Tools) != 1 {
		t.Fatalf("mutated tools=%v", call.Tools)
	}
}
