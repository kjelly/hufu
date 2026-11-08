package agent

import (
	"context"
	"errors"
	"strings"
	"testing"

	"charm.land/fantasy"
)

func TestStructuredResponsePreservesArgumentsAndAdmission(t *testing.T) {
	for _, tc := range []struct {
		name  string
		text  string
		calls []fantasy.ToolCallContent
		want  string
		bad   bool
	}{
		{name: "native arguments", text: "Let me analyze this task.\n", calls: []fantasy.ToolCallContent{{ToolName: StructuredResponseFunctionName, Input: `{"result":{ "ok": true }}`}}, want: `{ "ok": true }`},
		{name: "invalid arguments stay unchanged", calls: []fantasy.ToolCallContent{{ToolName: StructuredResponseFunctionName, Input: `{"result":{"ok":true,"ok":false}}`}}, want: `{"ok":true,"ok":false}`},
		{name: "empty arrays stay arrays", calls: []fantasy.ToolCallContent{{ToolName: StructuredResponseFunctionName, Input: `{"result":{ "items": [], "nested": {"items":[]} }}`}}, want: `{ "items": [], "nested": {"items":[]} }`},
		{name: "invalid field types stay unchanged", calls: []fantasy.ToolCallContent{{ToolName: StructuredResponseFunctionName, Input: `{"result":{"items":{}}}`}}, want: `{"items":{}}`},
		{name: "namesake result is preserved", calls: []fantasy.ToolCallContent{{ToolName: StructuredResponseFunctionName, Input: `{"result":{"result":{"items":[]}}}`}}, want: `{"result":{"items":[]}}`},
		{name: "text endpoint", text: `{"ok":true}`, want: `{"ok":true}`},
		{name: "prose is not extracted", text: "Let me analyze.\n{\"ok\":true}", want: "Let me analyze.\n{\"ok\":true}"},
		{name: "work tool denied", calls: []fantasy.ToolCallContent{{ToolName: "write", Input: `{}`}}, bad: true},
		{name: "multiple responses denied", calls: []fantasy.ToolCallContent{{ToolName: StructuredResponseFunctionName, Input: `{}`}, {ToolName: StructuredResponseFunctionName, Input: `{}`}}, bad: true},
		{name: "provider execution denied", calls: []fantasy.ToolCallContent{{ToolName: StructuredResponseFunctionName, Input: `{}`, ProviderExecuted: true}}, bad: true},
		{name: "missing envelope denied", calls: []fantasy.ToolCallContent{{ToolName: StructuredResponseFunctionName, Input: `{"ok":true}`}}, bad: true},
		{name: "duplicate envelope denied", calls: []fantasy.ToolCallContent{{ToolName: StructuredResponseFunctionName, Input: `{"result":{},"result":{}}`}}, bad: true},
		{name: "extra envelope member denied", calls: []fantasy.ToolCallContent{{ToolName: StructuredResponseFunctionName, Input: `{"result":{},"extra":true}`}}, bad: true},
		{name: "nonobject result denied", calls: []fantasy.ToolCallContent{{ToolName: StructuredResponseFunctionName, Input: `{"result":[]}`}}, bad: true},
		{name: "trailing response denied", calls: []fantasy.ToolCallContent{{ToolName: StructuredResponseFunctionName, Input: `{"result":{}} {}`}}, bad: true},
		{name: "incomplete envelope denied", calls: []fantasy.ToolCallContent{{ToolName: StructuredResponseFunctionName, Input: `{"result":{}`}}, bad: true},
	} {
		for _, streaming := range []bool{false, true} {
			method := "generate"
			if streaming {
				method = "stream"
			}
			t.Run(tc.name+"/"+method, func(t *testing.T) {
				usage := fantasy.Usage{InputTokens: 10, OutputTokens: 5, TotalTokens: 15}
				response := &fantasy.Response{Content: fantasy.ResponseContent{fantasy.TextContent{Text: tc.text}}, Usage: usage, FinishReason: fantasy.FinishReasonStop}
				for _, call := range tc.calls {
					response.Content = append(response.Content, call)
				}
				var base fantasy.LanguageModel = &resultLanguageModel{generateResponse: response}
				if streaming {
					base = &streamLanguageModel{streamFn: func(context.Context) fantasy.StreamResponse {
						return func(yield func(fantasy.StreamPart) bool) {
							for _, part := range []fantasy.StreamPart{
								{Type: fantasy.StreamPartTypeTextStart, ID: "preamble"},
								{Type: fantasy.StreamPartTypeTextDelta, ID: "preamble", Delta: tc.text},
								{Type: fantasy.StreamPartTypeTextEnd, ID: "preamble"},
							} {
								if !yield(part) {
									return
								}
							}
							for _, call := range tc.calls {
								if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeToolCall, ToolCallName: call.ToolName, ToolCallInput: call.Input, ProviderExecuted: call.ProviderExecuted}) {
									return
								}
							}
							yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeFinish, Usage: usage, FinishReason: fantasy.FinishReasonToolCalls})
						}
					}}
				}
				admission := &recordingAdmission{}
				admitted := NewAdmittedLanguageModelWithContext("test-model", base, admission, ProviderAdmissionContext{})
				schema := map[string]any{"type": "object", "properties": map[string]any{"ok": map[string]any{"type": "boolean"}}, "required": []string{"ok"}}
				model, err := newStructuredResponseModel(admitted, &StructuredResponseConfig{Schema: schema, MaxBytes: 1024})
				if err != nil {
					t.Fatal(err)
				}
				schema["properties"].(map[string]any)["injected"] = map[string]any{"type": "string"}
				schema["required"].([]string)[0] = "injected"
				var text string
				var observed fantasy.Usage
				if streaming {
					stream, streamErr := model.Stream(t.Context(), fantasy.Call{})
					err = streamErr
					if err == nil {
						for part := range stream {
							switch part.Type {
							case fantasy.StreamPartTypeTextDelta:
								text += part.Delta
							case fantasy.StreamPartTypeError:
								err = part.Error
							case fantasy.StreamPartTypeFinish:
								observed = part.Usage
							case fantasy.StreamPartTypeToolCall, fantasy.StreamPartTypeToolInputStart, fantasy.StreamPartTypeToolInputDelta, fantasy.StreamPartTypeToolInputEnd:
								t.Fatal("an executable tool escaped the response adapter")
							}
							if err != nil {
								break
							}
						}
					}
				} else {
					result, callErr := model.Generate(t.Context(), fantasy.Call{})
					err = callErr
					if result != nil {
						text, observed = result.Content.Text(), result.Usage
						if len(result.Content.ToolCalls()) != 0 {
							t.Fatal("an executable tool escaped the response adapter")
						}
					}
				}
				if tc.bad {
					if !errors.Is(err, ErrStructuredResponseTool) {
						t.Fatalf("unexpected response was not denied: %v", err)
					}
				} else if err != nil || text != tc.want || observed.TotalTokens != usage.TotalTokens {
					t.Fatalf("text=%q usage=%+v error=%v", text, observed, err)
				}
				admission.mu.Lock()
				defer admission.mu.Unlock()
				if len(admission.commits) != 1 {
					t.Fatalf("provider commits=%d", len(admission.commits))
				}
				actual := admission.commits[0].Call
				if actual == nil || len(actual.Tools) != 1 || actual.Tools[0].GetName() != StructuredResponseFunctionName || actual.ToolChoice == nil || *actual.ToolChoice != fantasy.SpecificToolChoice(StructuredResponseFunctionName) {
					t.Fatalf("admission did not see the complete response channel: %+v", actual)
				}
				function := actual.Tools[0].(fantasy.FunctionTool)
				properties, _ := function.InputSchema["properties"].(map[string]any)
				claims, _ := properties["result"].(map[string]any)
				if len(properties) != 1 || claims["type"] != "object" || function.InputSchema["additionalProperties"] != false {
					t.Fatalf("response schema lost its single object boundary: %v", function.InputSchema)
				}
				wire, err := SerializeProviderRequest(admission.commits[0])
				if err != nil || strings.Contains(string(wire), "injected") {
					t.Fatalf("caller mutated the frozen schema: %s %v", wire, err)
				}
			})
		}
	}
}

func TestStructuredResponseRejectsMissingFinishAndOversizedData(t *testing.T) {
	for _, tc := range []struct {
		name  string
		parts []fantasy.StreamPart
	}{
		{name: "missing finish", parts: []fantasy.StreamPart{{Type: fantasy.StreamPartTypeTextDelta, Delta: `{}`}}},
		{name: "oversized text", parts: []fantasy.StreamPart{{Type: fantasy.StreamPartTypeTextDelta, Delta: strings.Repeat("x", 17)}}},
		{name: "oversized arguments", parts: []fantasy.StreamPart{{Type: fantasy.StreamPartTypeToolInputDelta, Delta: strings.Repeat("x", 17)}}},
		{name: "oversized complete arguments", parts: []fantasy.StreamPart{
			{Type: fantasy.StreamPartTypeToolCall, ToolCallName: StructuredResponseFunctionName, ToolCallInput: `{"result":{"items":[]}}`},
			{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonToolCalls},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := &streamLanguageModel{streamFn: func(context.Context) fantasy.StreamResponse {
				return func(yield func(fantasy.StreamPart) bool) {
					for _, part := range tc.parts {
						if !yield(part) {
							return
						}
					}
				}
			}}
			model, err := newStructuredResponseModel(base, &StructuredResponseConfig{Schema: map[string]any{"type": "object"}, MaxBytes: 16})
			if err != nil {
				t.Fatal(err)
			}
			stream, err := model.Stream(t.Context(), fantasy.Call{})
			if err != nil {
				t.Fatal(err)
			}
			failed := false
			for part := range stream {
				failed = failed || part.Type == fantasy.StreamPartTypeError
				if part.Type == fantasy.StreamPartTypeTextDelta {
					t.Fatal("incomplete or oversized claims escaped")
				}
			}
			if !failed {
				t.Fatal("incomplete or oversized response was accepted")
			}
		})
	}
}
