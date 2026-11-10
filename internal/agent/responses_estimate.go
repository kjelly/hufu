package agent

import (
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"fmt"

	"charm.land/fantasy"
	"charm.land/fantasy/providers/openai"
	"github.com/kjelly/hufu/internal/config"
)

// Version 1 allowances cover role/item framing and SDK-added schema defaults.
// They are conservative policy margins, not a claim about every tokenizer.
// Wire-capture tests independently check coverage of the encoded request.
const (
	responsesEstimateVersion = 1
	responsesBaseAllowance   = 256
	responsesItemAllowance   = 64
	responsesToolAllowance   = 64
)

type responsesEstimateItem struct {
	Role      fantasy.MessageRole `json:"role"`
	Type      string              `json:"type"`
	Text      string              `json:"text,omitempty"`
	CallID    string              `json:"call_id,omitempty"`
	Name      string              `json:"name,omitempty"`
	Arguments string              `json:"arguments,omitempty"`
	Output    string              `json:"output,omitempty"`
}

type responsesEstimateTool struct {
	Type        string         `json:"type"`
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
	Strict      bool           `json:"strict"`
}

// responsesInputEstimate is a content inventory, not a replacement SDK wire
// encoder. Only content which the pinned adapter preserves is admitted.
func responsesInputEstimate(prompt fantasy.Prompt) ([]responsesEstimateItem, error) {
	var items []responsesEstimateItem
	for _, message := range prompt {
		switch message.Role {
		case fantasy.MessageRoleSystem, fantasy.MessageRoleUser, fantasy.MessageRoleAssistant, fantasy.MessageRoleTool:
		default:
			return nil, fmt.Errorf("%w: unknown message role", ErrResponsesContent)
		}
		for _, part := range message.Content {
			item := responsesEstimateItem{Role: message.Role}
			switch value := part.(type) {
			case fantasy.TextPart:
				if message.Role == fantasy.MessageRoleTool {
					return nil, fmt.Errorf("%w: tool messages require a result", ErrResponsesContent)
				}
				item.Type, item.Text = "text", value.Text
			case fantasy.ReasoningPart:
				if message.Role != fantasy.MessageRoleAssistant {
					return nil, fmt.Errorf("%w: reasoning in a non-assistant role", ErrResponsesContent)
				}
				continue // The pinned SDK does not replay reasoning items.
			case fantasy.ToolCallPart:
				if message.Role != fantasy.MessageRoleAssistant || value.ProviderExecuted {
					return nil, fmt.Errorf("%w: only local assistant function calls are supported", ErrResponsesContent)
				}
				item.Type, item.CallID, item.Name, item.Arguments = "function_call", value.ToolCallID, value.ToolName, value.Input
			case fantasy.ToolResultPart:
				if message.Role != fantasy.MessageRoleTool || value.ProviderExecuted {
					return nil, fmt.Errorf("%w: only local tool results are supported", ErrResponsesContent)
				}
				item.Type, item.CallID = "function_call_output", value.ToolCallID
				switch output := value.Output.(type) {
				case fantasy.ToolResultOutputContentText:
					item.Output = output.Text
				case fantasy.ToolResultOutputContentError:
					if output.Error == nil {
						return nil, fmt.Errorf("%w: nil tool error", ErrResponsesContent)
					}
					item.Output = output.Error.Error()
				default:
					return nil, fmt.Errorf("%w: only text tool results are supported", ErrResponsesContent)
				}
			default:
				return nil, fmt.Errorf("%w: only text and function call/result input is supported", ErrResponsesContent)
			}
			items = append(items, item)
		}
	}
	return items, nil
}

func serializeResponsesAdmissionEstimate(request ProviderRequest) ([]byte, error) {
	if err := validateResponsesRequest(request); err != nil {
		return nil, err
	}
	call, err := requestCall(request)
	if err != nil {
		return nil, err
	}
	items, err := responsesInputEstimate(call.Prompt)
	if err != nil {
		return nil, err
	}
	tools := make([]responsesEstimateTool, 0, len(call.Tools))
	for _, tool := range call.Tools {
		function, ok := tool.(fantasy.FunctionTool)
		if !ok {
			return nil, fmt.Errorf("%w: unsupported function tool representation", ErrResponsesContent)
		}
		tools = append(tools, responsesEstimateTool{Type: "function", Name: function.Name, Description: function.Description, Parameters: function.InputSchema})
	}
	// openaicompat uses Fantasy's tool-based ObjectMode. Account for the
	// fallback names and descriptions inserted by that adapter too.
	if request.ObjectCall != nil && len(tools) > 0 {
		if tools[0].Name == "" {
			tools[0].Name = "generate_object"
		}
		if tools[0].Description == "" {
			tools[0].Description = "Generate a structured object matching the schema"
		}
	}
	estimate := map[string]any{"estimate_version": responsesEstimateVersion, "model": request.ModelID, "input": items, "tools": tools}
	if request.ObjectCall != nil && len(tools) > 0 {
		estimate["tool_choice"] = map[string]string{"type": "function", "name": tools[0].Name}
	}
	if call.ToolChoice != nil {
		switch *call.ToolChoice {
		case fantasy.ToolChoiceAuto, fantasy.ToolChoiceNone, fantasy.ToolChoiceRequired:
			estimate["tool_choice"] = string(*call.ToolChoice)
		default:
			estimate["tool_choice"] = map[string]string{"type": "function", "name": string(*call.ToolChoice)}
		}
	}
	options := call.ProviderOptions
	if request.ObjectCall != nil {
		options = request.ObjectCall.ProviderOptions
	}
	if value, ok := options[openai.Name].(*openai.ResponsesProviderOptions); ok && value != nil {
		// Account for any additional provider instructions and option strings.
		// Store/history restrictions are enforced by validateResponsesRequest.
		estimate["provider_options"] = value
	}
	// The pinned SDK uses encoding/json v1 string escaping. Preserve its HTML
	// and JavaScript expansion in this inventory so long strings cannot make
	// the conservative byte floor smaller than the actual request body.
	return json.Marshal(estimate, jsontext.EscapeForHTML(true), jsontext.EscapeForJS(true))
}

// ProviderRequestTokenFloor is a versioned conservative byte-based floor for
// Responses admission. serialized must be the successful complete estimate
// from SerializeProviderRequest. Chat keeps its existing estimator unchanged.
func ProviderRequestTokenFloor(request ProviderRequest, serialized []byte) int {
	if request.AdmissionContext.APIProtocol.Effective != config.APIProtocolResponses {
		return 0
	}
	messages := request.Messages
	tools := len(request.Tools)
	if request.Call != nil {
		messages = request.Call.Prompt
		tools += len(request.Call.Tools)
	}
	if request.ObjectCall != nil {
		messages = request.ObjectCall.Prompt
		tools++
	}
	items := 0
	for _, message := range messages {
		items += max(1, len(message.Content))
	}
	return len(serialized) + responsesBaseAllowance + responsesItemAllowance*items + responsesToolAllowance*tools
}
