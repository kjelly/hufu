package agent

import (
	"context"
	"encoding/json"
	"fmt"

	"charm.land/fantasy"
	"charm.land/fantasy/providers/openaicompat"
	"charm.land/fantasy/schema"
)

// ProviderRequest is the provider-equivalent request shape used by Hufu's
// admission layer. Messages already include the system message and every
// multimodal/tool part that Fantasy will send for the step.
type ProviderRequest struct {
	ModelID    string
	Call       *fantasy.Call
	ObjectCall *fantasy.ObjectCall
	Messages   []fantasy.Message
	Tools      []fantasy.AgentTool
}

// SerializeProviderRequest returns the compact OpenAI-compatible request body
// used for admission. Fantasy's exported prompt conversion is the source of
// truth for message and multimodal wire representation.
func SerializeProviderRequest(request ProviderRequest) ([]byte, error) {
	call, err := requestCall(request)
	if err != nil {
		return nil, err
	}
	messages, _ := openaicompat.ToPromptFunc(call.Prompt, "", "")
	wire := map[string]any{"model": request.ModelID, "messages": messages}
	if tools := serializeProviderTools(call.Tools); len(tools) > 0 {
		wire["tools"] = tools
	}
	if call.ToolChoice != nil {
		wire["tool_choice"] = serializeToolChoice(*call.ToolChoice)
	}
	return json.Marshal(wire)
}

func requestCall(request ProviderRequest) (*fantasy.Call, error) {
	if request.Call != nil && request.ObjectCall != nil {
		return nil, fmt.Errorf("provider request has multiple calls")
	}
	call := request.Call
	if request.ObjectCall != nil {
		objectCall := request.ObjectCall
		call = &fantasy.Call{
			Prompt:          objectCall.Prompt,
			MaxOutputTokens: objectCall.MaxOutputTokens,
			Tools: []fantasy.Tool{fantasy.FunctionTool{
				Name:        objectCall.SchemaName,
				Description: objectCall.SchemaDescription,
				InputSchema: schema.ToMap(objectCall.Schema),
			}},
		}
	}
	if call == nil {
		call = &fantasy.Call{Prompt: request.Messages}
	}
	if len(request.Tools) > 0 {
		for _, tool := range request.Tools {
			if tool == nil {
				continue
			}
			info := tool.Info()
			inputSchema := map[string]any{"type": "object", "properties": info.Parameters, "required": info.Required}
			schema.Normalize(inputSchema)
			call.Tools = append(call.Tools, fantasy.FunctionTool{
				Name:        info.Name,
				Description: info.Description,
				InputSchema: inputSchema,
			})
		}
	}
	return call, nil
}

type providerWireTool struct {
	Type     string               `json:"type"`
	Function providerWireFunction `json:"function"`
}

type providerWireFunction struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
	Strict      bool           `json:"strict"`
}

func serializeProviderTools(tools []fantasy.Tool) []providerWireTool {
	serialized := make([]providerWireTool, 0, len(tools))
	for _, tool := range tools {
		function, ok := tool.(fantasy.FunctionTool)
		if !ok || function.GetType() != fantasy.ToolTypeFunction {
			continue
		}
		serialized = append(serialized, providerWireTool{
			Type: "function",
			Function: providerWireFunction{
				Name: function.Name, Description: function.Description,
				Parameters: function.InputSchema, Strict: false,
			},
		})
	}
	return serialized
}

func serializeToolChoice(choice fantasy.ToolChoice) any {
	switch choice {
	case fantasy.ToolChoiceAuto, fantasy.ToolChoiceNone, fantasy.ToolChoiceRequired:
		return string(choice)
	default:
		return map[string]any{"type": "function", "function": map[string]string{"name": string(choice)}}
	}
}

// CountProviderRequestTokens is retained as a small compatibility helper for
// callers that need a provider-independent conservative estimate.
func CountProviderRequestTokens(request ProviderRequest) (int, error) {
	data, err := SerializeProviderRequest(request)
	if err != nil {
		return 0, fmt.Errorf("serialize provider request: %w", err)
	}
	return (len(data) + 3) / 4, nil
}

type admittedLanguageModel struct {
	modelID   string
	inner     fantasy.LanguageModel
	admission RequestAdmission
}

// RequestAdmission is the shared pre-provider admission contract.
type RequestAdmission interface {
	AdmitProviderRequest(context.Context, ProviderRequest) error
}

func NewAdmittedLanguageModel(modelID string, inner fantasy.LanguageModel, admission RequestAdmission) fantasy.LanguageModel {
	if inner == nil || admission == nil {
		return inner
	}
	return &admittedLanguageModel{modelID: modelID, inner: inner, admission: admission}
}

func (m *admittedLanguageModel) Generate(ctx context.Context, call fantasy.Call) (*fantasy.Response, error) {
	if err := m.admission.AdmitProviderRequest(ctx, ProviderRequest{ModelID: m.modelID, Call: &call}); err != nil {
		return nil, err
	}
	return m.inner.Generate(ctx, call)
}

func (m *admittedLanguageModel) Stream(ctx context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
	if err := m.admission.AdmitProviderRequest(ctx, ProviderRequest{ModelID: m.modelID, Call: &call}); err != nil {
		return nil, err
	}
	return m.inner.Stream(ctx, call)
}

func (m *admittedLanguageModel) GenerateObject(ctx context.Context, call fantasy.ObjectCall) (*fantasy.ObjectResponse, error) {
	if err := m.admission.AdmitProviderRequest(ctx, ProviderRequest{ModelID: m.modelID, ObjectCall: &call}); err != nil {
		return nil, err
	}
	return m.inner.GenerateObject(ctx, call)
}

func (m *admittedLanguageModel) StreamObject(ctx context.Context, call fantasy.ObjectCall) (fantasy.ObjectStreamResponse, error) {
	if err := m.admission.AdmitProviderRequest(ctx, ProviderRequest{ModelID: m.modelID, ObjectCall: &call}); err != nil {
		return nil, err
	}
	return m.inner.StreamObject(ctx, call)
}

func (m *admittedLanguageModel) Provider() string { return m.inner.Provider() }

func (m *admittedLanguageModel) Model() string { return m.inner.Model() }
