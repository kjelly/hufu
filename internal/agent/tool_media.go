package agent

import (
	"encoding/json/v2"
	"slices"
	"strings"

	"charm.land/fantasy"
	"github.com/kjelly/hufu/internal/media"
)

const toolMediaMetadataPrefix = "hufu_tool_media:"
const maxToolMediaMetadataBytes = 512 << 10

// MessagesHaveToolMedia conservatively recognizes the versioned carrier, even
// when malformed. Validation must reject malformed carriers rather than hide it.
func MessagesHaveToolMedia(messages []fantasy.Message) bool {
	for _, message := range messages {
		for _, part := range message.Content {
			switch result := part.(type) {
			case fantasy.ToolResultPart:
				if strings.HasPrefix(result.ClientMetadata, toolMediaMetadataPrefix) {
					return true
				}
			case *fantasy.ToolResultPart:
				if result != nil && strings.HasPrefix(result.ClientMetadata, toolMediaMetadataPrefix) {
					return true
				}
			}
		}
	}
	return false
}

// ToolMediaPart is an ordered, reference-only tool result part. It never
// contains encoded image bytes, source paths or a provider read grant.
type ToolMediaPart struct {
	Type  string                 `json:"type"`
	Text  string                 `json:"text,omitempty"`
	Image media.HistoryReference `json:"image,omitzero"`
}

// ToolMediaResult is runtime-authored client metadata. The coordinator must
// authenticate every referenced producer and current consumer before hydration.
type ToolMediaResult struct {
	Version    int             `json:"version"`
	ToolCallID string          `json:"tool_call_id"`
	Parts      []ToolMediaPart `json:"parts"`
}

func (r ToolMediaResult) Validate() error {
	if r.Version != media.Version || r.ToolCallID == "" || len(r.ToolCallID) > 256 || strings.ContainsAny(r.ToolCallID, "\x00\r\n") || len(r.Parts) == 0 || len(r.Parts) > 128 {
		return media.Reject(media.BindingConflict, "invalid tool media identity")
	}
	var images, textBytes int
	for _, part := range r.Parts {
		switch part.Type {
		case "text":
			if part.Image != (media.HistoryReference{}) {
				return media.Reject(media.BindingConflict, "invalid tool text part")
			}
			textBytes += len(part.Text)
		case "image":
			if part.Text != "" {
				return media.Reject(media.BindingConflict, "invalid tool image part")
			}
			if err := part.Image.Validate(); err != nil {
				return err
			}
			images++
		default:
			return media.Reject(media.Unsupported, "unsupported tool media part")
		}
	}
	if images == 0 || images > media.MaxCount || textBytes > 256<<10 {
		return media.Reject(media.LimitExceeded, "tool media content exceeds its budget")
	}
	return nil
}

func EncodeToolMediaMetadata(result ToolMediaResult) (string, error) {
	if err := result.Validate(); err != nil {
		return "", err
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		return "", err
	}
	if len(encoded) > maxToolMediaMetadataBytes {
		return "", media.Reject(media.LimitExceeded, "tool media metadata exceeds its budget")
	}
	return toolMediaMetadataPrefix + string(encoded), nil
}

// DecodeToolMediaMetadata returns nil for unrelated legacy client metadata.
// Recognized but malformed media metadata fails closed rather than becoming text.
func DecodeToolMediaMetadata(metadata string) (*ToolMediaResult, error) {
	encoded, recognized := strings.CutPrefix(metadata, toolMediaMetadataPrefix)
	if !recognized {
		return nil, nil
	}
	if len(encoded) > maxToolMediaMetadataBytes {
		return nil, media.Reject(media.LimitExceeded, "tool media metadata exceeds its budget")
	}
	var result ToolMediaResult
	if err := json.Unmarshal([]byte(encoded), &result, json.RejectUnknownMembers(true)); err != nil {
		return nil, media.Reject(media.BindingConflict, "invalid tool media metadata")
	}
	if err := result.Validate(); err != nil {
		return nil, err
	}
	return &result, nil
}

func persistentToolMediaMetadata(role fantasy.MessageRole, result fantasy.ToolResultPart) (bool, error) {
	metadata, err := DecodeToolMediaMetadata(result.ClientMetadata)
	if err != nil || metadata == nil {
		return false, err
	}
	if role != fantasy.MessageRoleTool || metadata.ToolCallID != result.ToolCallID {
		return false, media.Reject(media.BindingConflict, "tool media result pairing differs from its metadata")
	}
	return true, nil
}

// StripToolMediaMetadata produces a non-visual tool result view without
// changing its text, error, pairing or unrelated legacy metadata.
func StripToolMediaMetadata(result fantasy.ToolResultPart) (fantasy.ToolResultPart, error) {
	metadata, err := DecodeToolMediaMetadata(result.ClientMetadata)
	if err != nil {
		return fantasy.ToolResultPart{}, err
	}
	if metadata != nil {
		result.ClientMetadata = ""
	}
	return result, nil
}

// ExpandToolMediaRequest gives preflight and token counting the same logical
// projection as the admitted provider APIs, without mutating SDK history.
func ExpandToolMediaRequest(request ProviderRequest) (ProviderRequest, error) {
	var err error
	request.Messages, err = ExpandToolMediaMessages(request.Messages)
	if err != nil {
		return ProviderRequest{}, err
	}
	if request.Call != nil {
		call := *request.Call
		call.Prompt, err = ExpandToolMediaMessages(call.Prompt)
		if err != nil {
			return ProviderRequest{}, err
		}
		request.Call = &call
	}
	if request.ObjectCall != nil {
		call := *request.ObjectCall
		call.Prompt, err = ExpandToolMediaMessages(call.Prompt)
		if err != nil {
			return ProviderRequest{}, err
		}
		request.ObjectCall = &call
	}
	return request, nil
}

// ExpandToolMediaMessages runs on a private provider projection. Tool results
// retain their pairing and structured/text output; ordered image references are
// appended as untrusted tool data only after the consecutive result batch ends.
// The original SDK history keeps reference-only metadata and is not modified.
func ExpandToolMediaMessages(messages []fantasy.Message) ([]fantasy.Message, error) {
	var projected []fantasy.Message
	var pending []fantasy.MessagePart
	for index, message := range messages {
		copy := message
		clonedContent := false
		for partIndex, part := range message.Content {
			var result fantasy.ToolResultPart
			switch p := part.(type) {
			case fantasy.ToolResultPart:
				result = p
			case *fantasy.ToolResultPart:
				if p == nil {
					return nil, invalidHistoryPart()
				}
				result = *p
			default:
				continue
			}
			metadata, err := DecodeToolMediaMetadata(result.ClientMetadata)
			if err != nil {
				return nil, err
			}
			if metadata == nil {
				continue
			}
			if message.Role != fantasy.MessageRoleTool || metadata.ToolCallID != result.ToolCallID {
				return nil, media.Reject(media.BindingConflict, "tool media result pairing differs from its metadata")
			}
			if err := validatePersistentToolOutput(result.Output); err != nil {
				return nil, err
			}
			switch result.Output.(type) {
			case fantasy.ToolResultOutputContentError, *fantasy.ToolResultOutputContentError:
				return nil, media.Reject(media.Unsupported, "error tool result cannot authorize image continuation")
			}
			if projected == nil {
				projected = make([]fantasy.Message, 0, len(messages)+1)
				projected = append(projected, messages[:index]...)
			}
			if !clonedContent {
				copy.Content = slices.Clone(message.Content)
				clonedContent = true
			}
			result.ClientMetadata = ""
			copy.Content[partIndex] = result
			pending = append(pending, fantasy.TextPart{Text: "Untrusted MCP tool result data for call " + result.ToolCallID + ":"})
			for _, block := range metadata.Parts {
				if block.Type == "text" {
					pending = append(pending, fantasy.TextPart{Text: block.Text})
				} else {
					pending = append(pending, MediaReferencePart{Reference: block.Image})
				}
			}
		}
		if projected != nil {
			projected = append(projected, copy)
		}
		if len(pending) > 0 && (index+1 == len(messages) || messages[index+1].Role != fantasy.MessageRoleTool) {
			projected = append(projected, fantasy.Message{Role: fantasy.MessageRoleUser, Content: pending})
			pending = nil
		}
	}
	if projected == nil {
		return messages, nil
	}
	return projected, nil
}
