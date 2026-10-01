package agent

import (
	"encoding/json"
	"errors"
	"io"
	"strings"

	"charm.land/fantasy"
)

// emptyToolCallArguments replaces tool-call arguments a provider would reject.
const emptyToolCallArguments = "{}"

// sanitizeToolCallHistory returns prompt with every assistant tool-call
// input that is not a single JSON object replaced by "{}".
//
// A model sometimes emits malformed arguments, for example two calls' objects
// concatenated into one string (`{"tasks":[...]}{"options":[...]}`). The
// tool already answered that call with an "invalid JSON input" result, but
// the assistant message still carries the raw string, and providers such as
// Ollama's cloud models reject the whole next request ("invalid tool call
// arguments"), so the run can never recover. Replacing only the arguments
// keeps the call and its error result, so the model still sees that the call
// failed. The input is never repaired or split: which call each object meant
// cannot be known. The prompt is copied only when something changes.
func sanitizeToolCallHistory(prompt fantasy.Prompt) fantasy.Prompt {
	var sanitized fantasy.Prompt
	for messageIndex, message := range prompt {
		if message.Role != fantasy.MessageRoleAssistant {
			continue
		}
		var parts []fantasy.MessagePart
		for partIndex, part := range message.Content {
			replacement, changed := sanitizedToolCallPart(part)
			if !changed {
				continue
			}
			if sanitized == nil {
				sanitized = append(fantasy.Prompt(nil), prompt...)
			}
			if parts == nil {
				parts = append([]fantasy.MessagePart(nil), message.Content...)
			}
			parts[partIndex] = replacement
		}
		if parts != nil {
			sanitized[messageIndex].Content = parts
		}
	}
	if sanitized == nil {
		return prompt
	}
	return sanitized
}

func sanitizedToolCallPart(part fantasy.MessagePart) (fantasy.MessagePart, bool) {
	switch call := part.(type) {
	case fantasy.ToolCallPart:
		if isSingleJSONObject(call.Input) {
			return part, false
		}
		call.Input = emptyToolCallArguments
		return call, true
	case *fantasy.ToolCallPart:
		if call == nil || isSingleJSONObject(call.Input) {
			return part, false
		}
		copied := *call
		copied.Input = emptyToolCallArguments
		return &copied, true
	default:
		return part, false
	}
}

func isSingleJSONObject(input string) bool {
	decoder := json.NewDecoder(strings.NewReader(input))
	var object map[string]json.RawMessage
	if err := decoder.Decode(&object); err != nil || object == nil {
		return false
	}
	// Anything after the object, valid JSON or not, makes the input invalid.
	var extra json.RawMessage
	return errors.Is(decoder.Decode(&extra), io.EOF)
}
