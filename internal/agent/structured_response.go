package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"charm.land/fantasy"
)

// StructuredResponseFunctionName names a provider response channel, not an
// executable agent tool. Its arguments remain untrusted JSON claims.
const StructuredResponseFunctionName = "return_result"

var ErrStructuredResponseTool = errors.New("unexpected structured response tool")

// StructuredResponseConfig requests data through a forced function response.
// No handler is registered, and the admitted model sees the complete wire
// schema before transport. The caller still validates the exact argument bytes.
type StructuredResponseConfig struct {
	Schema   map[string]any
	MaxBytes int
}

type structuredResponseModel struct {
	fantasy.LanguageModel
	config StructuredResponseConfig
}

func newStructuredResponseModel(model fantasy.LanguageModel, config *StructuredResponseConfig) (fantasy.LanguageModel, error) {
	if config.Schema["type"] != "object" || config.MaxBytes <= 0 {
		return nil, errors.New("structured response requires an object schema and a positive byte limit")
	}
	return &structuredResponseModel{LanguageModel: model, config: StructuredResponseConfig{
		Schema: cloneProviderSchemaValue(config.Schema).(map[string]any), MaxBytes: config.MaxBytes,
	}}, nil
}

func (m *structuredResponseModel) responseCall(call fantasy.Call) (fantasy.Call, error) {
	if len(call.Tools) != 0 {
		return call, errors.New("structured response cannot expose executable tools")
	}
	call.Tools = []fantasy.Tool{fantasy.FunctionTool{
		Name: StructuredResponseFunctionName, Description: "Return the complete result as JSON data. This response function has no executable handler.",
		InputSchema: cloneProviderSchemaValue(m.config.Schema).(map[string]any),
	}}
	call.ToolChoice = new(fantasy.SpecificToolChoice(StructuredResponseFunctionName))
	return call, nil
}

func (m *structuredResponseModel) responseText(text string, calls []fantasy.ToolCallContent) (string, error) {
	if len(calls) > 1 {
		return "", fmt.Errorf("%w: expected one response function", ErrStructuredResponseTool)
	}
	if len(calls) == 1 {
		if calls[0].ToolName != StructuredResponseFunctionName || calls[0].ProviderExecuted {
			return "", fmt.Errorf("%w: only the data response function is permitted", ErrStructuredResponseTool)
		}
		text = calls[0].Input
	}
	// Older endpoints may ignore tool_choice and send text. Preserve that text
	// verbatim for the caller's strict validation and bounded format repair.
	if len(text) > m.config.MaxBytes {
		return "", errors.New("structured response exceeds its byte limit")
	}
	return text, nil
}

func (m *structuredResponseModel) Generate(ctx context.Context, call fantasy.Call) (*fantasy.Response, error) {
	call, err := m.responseCall(call)
	if err != nil {
		return nil, err
	}
	response, err := m.LanguageModel.Generate(ctx, call)
	if err != nil || response == nil {
		return response, err
	}
	if len(response.Content.ToolResults()) != 0 {
		return nil, ErrStructuredResponseTool
	}
	text, err := m.responseText(response.Content.Text(), response.Content.ToolCalls())
	if err != nil {
		return nil, err
	}
	result := *response
	result.Content = nil
	for _, content := range response.Content {
		if content.GetType() == fantasy.ContentTypeReasoning {
			result.Content = append(result.Content, content)
		}
	}
	result.Content = append(result.Content, fantasy.TextContent{Text: text})
	if len(response.Content.ToolCalls()) != 0 {
		result.FinishReason = fantasy.FinishReasonStop
	}
	return &result, nil
}

func (m *structuredResponseModel) Stream(ctx context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
	call, err := m.responseCall(call)
	if err != nil {
		return nil, err
	}
	stream, err := m.LanguageModel.Stream(ctx, call)
	if err != nil {
		return nil, err
	}
	return func(yield func(fantasy.StreamPart) bool) {
		var text strings.Builder
		var calls []fantasy.ToolCallContent
		var finish *fantasy.StreamPart
		inputBytes := 0
		for part := range stream {
			switch part.Type {
			case fantasy.StreamPartTypeTextStart, fantasy.StreamPartTypeTextEnd,
				fantasy.StreamPartTypeToolInputStart, fantasy.StreamPartTypeToolInputEnd:
				continue
			case fantasy.StreamPartTypeTextDelta:
				text.WriteString(part.Delta)
			case fantasy.StreamPartTypeToolInputDelta:
				inputBytes += len(part.Delta)
			case fantasy.StreamPartTypeToolCall:
				if len(calls) != 0 || part.ToolCallName != StructuredResponseFunctionName || part.ProviderExecuted {
					yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeError, Error: ErrStructuredResponseTool})
					return
				}
				calls = append(calls, fantasy.ToolCallContent{ToolName: part.ToolCallName, Input: part.ToolCallInput, ProviderExecuted: part.ProviderExecuted})
			case fantasy.StreamPartTypeToolResult:
				yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeError, Error: ErrStructuredResponseTool})
				return
			case fantasy.StreamPartTypeFinish:
				finish = &part
			default:
				if !yield(part) || part.Type == fantasy.StreamPartTypeError {
					return
				}
			}
			if text.Len() > m.config.MaxBytes || inputBytes > m.config.MaxBytes {
				yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeError, Error: errors.New("structured response exceeds its byte limit")})
				return
			}
		}
		if finish == nil {
			yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeError, Error: errors.New("structured response stream ended without a finish event")})
			return
		}
		body, err := m.responseText(text.String(), calls)
		if err != nil {
			yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeError, Error: err})
			return
		}
		for _, part := range []fantasy.StreamPart{
			{Type: fantasy.StreamPartTypeTextStart, ID: StructuredResponseFunctionName},
			{Type: fantasy.StreamPartTypeTextDelta, ID: StructuredResponseFunctionName, Delta: body},
			{Type: fantasy.StreamPartTypeTextEnd, ID: StructuredResponseFunctionName},
		} {
			if !yield(part) {
				return
			}
		}
		if len(calls) != 0 {
			finish.FinishReason = fantasy.FinishReasonStop
		}
		yield(*finish)
	}, nil
}
