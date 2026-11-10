package agent

import (
	"encoding/json/jsontext"
	"encoding/json/v2"

	"charm.land/fantasy"
	"github.com/kjelly/hufu/internal/media"
)

const mediaHistoryVersion = 1
const toolMediaHistoryVersion = 2
const mediaHistoryKind = "native_image_history_projection"

// ValidatePersistentMessages permits inert media references and the SDK's
// known text/tool parts. It runs before JSON encoding, so a raw FilePart or
// tool-media payload cannot first allocate a Base64 copy at a durable sink.
func ValidatePersistentMessages(messages []fantasy.Message) (bool, error) {
	hasReferences := false
	for _, message := range messages {
		for _, part := range message.Content {
			var reference *media.HistoryReference
			switch p := part.(type) {
			case MediaReferencePart:
				reference = &p.Reference
			case *MediaReferencePart:
				if p == nil {
					return false, invalidHistoryPart()
				}
				reference = &p.Reference
			case fantasy.TextPart, fantasy.ReasoningPart, fantasy.ToolCallPart:
			case *fantasy.TextPart:
				if p == nil {
					return false, invalidHistoryPart()
				}
			case *fantasy.ReasoningPart:
				if p == nil {
					return false, invalidHistoryPart()
				}
			case *fantasy.ToolCallPart:
				if p == nil {
					return false, invalidHistoryPart()
				}
			case fantasy.ToolResultPart:
				if err := validatePersistentToolOutput(p.Output); err != nil {
					return false, err
				}
				toolReferences, err := persistentToolMediaMetadata(message.Role, p)
				if err != nil {
					return false, err
				}
				hasReferences = hasReferences || toolReferences
			case *fantasy.ToolResultPart:
				if p == nil {
					return false, invalidHistoryPart()
				}
				if err := validatePersistentToolOutput(p.Output); err != nil {
					return false, err
				}
				toolReferences, err := persistentToolMediaMetadata(message.Role, *p)
				if err != nil {
					return false, err
				}
				hasReferences = hasReferences || toolReferences
			default:
				return false, invalidHistoryPart()
			}
			if reference != nil {
				if message.Role != fantasy.MessageRoleUser {
					return false, media.Reject(media.BindingConflict, "input media reference has an invalid message role")
				}
				if err := reference.Validate(); err != nil {
					return false, err
				}
				hasReferences = true
			}
		}
	}
	return hasReferences, nil
}

func invalidHistoryPart() error {
	return media.Reject(media.Unsupported, "history contains raw or unrecognized media content")
}

func validatePersistentToolOutput(output fantasy.ToolResultOutputContent) error {
	switch p := output.(type) {
	case fantasy.ToolResultOutputContentText, fantasy.ToolResultOutputContentError:
		return nil
	case *fantasy.ToolResultOutputContentText:
		if p != nil {
			return nil
		}
	case *fantasy.ToolResultOutputContentError:
		if p != nil {
			return nil
		}
	}
	return invalidHistoryPart()
}

// EncodeMediaHistory is used only for media-bearing compatibility histories.
// Legacy text histories retain the caller's existing serialization and hashes.
func EncodeMediaHistory(messages []fantasy.Message, checkpointID string) ([]byte, error) {
	hasReferences, err := ValidatePersistentMessages(messages)
	if err != nil {
		return nil, err
	}
	if !hasReferences {
		return nil, media.Reject(media.BindingConflict, "media history has no references")
	}
	if checkpointID == "" {
		return nil, media.Reject(media.DurabilityRequired, "media projection has no canonical checkpoint")
	}
	version := mediaHistoryVersion
	if MessagesHaveToolMedia(messages) {
		version = toolMediaHistoryVersion
	}
	return json.Marshal(struct {
		Version      int               `json:"version"`
		Kind         string            `json:"kind"`
		Authority    string            `json:"authority"`
		CheckpointID string            `json:"checkpoint_id"`
		Messages     []fantasy.Message `json:"messages"`
	}{version, mediaHistoryKind, "compatibility", checkpointID, messages}, json.Deterministic(true))
}

func DecodeMediaHistory(data []byte) ([]fantasy.Message, string, error) {
	var envelope struct {
		Version      int            `json:"version"`
		Kind         string         `json:"kind"`
		Authority    string         `json:"authority"`
		CheckpointID string         `json:"checkpoint_id"`
		Messages     jsontext.Value `json:"messages"`
	}
	if err := json.Unmarshal(data, &envelope, json.RejectUnknownMembers(true)); err != nil {
		return nil, "", media.Reject(media.BindingConflict, "invalid persisted media history")
	}
	if envelope.Version != mediaHistoryVersion && envelope.Version != toolMediaHistoryVersion || envelope.Kind != mediaHistoryKind {
		return nil, "", media.Reject(media.Unsupported, "unsupported persisted media history version")
	}
	if envelope.Authority != "compatibility" || envelope.CheckpointID == "" {
		return nil, "", media.Reject(media.DurabilityRequired, "media projection lacks its canonical checkpoint identity")
	}
	messages, err := DecodePersistentMessages(envelope.Messages)
	if err != nil {
		return nil, "", err
	}
	if MessagesHaveToolMedia(messages) && envelope.Version != toolMediaHistoryVersion {
		return nil, "", media.Reject(media.Unsupported, "tool media history requires a tool-aware version")
	}
	hasReferences, err := ValidatePersistentMessages(messages)
	if err != nil {
		return nil, "", err
	}
	if !hasReferences {
		return nil, "", media.Reject(media.BindingConflict, "media history has no references")
	}
	return messages, envelope.CheckpointID, nil
}

// DecodePersistentMessages restores reference parts without reading any CAS
// bytes. Source authorization remains the owning runtime's responsibility.
func DecodePersistentMessages(data []byte) ([]fantasy.Message, error) {
	var rawMessages []jsontext.Value
	if err := json.Unmarshal(data, &rawMessages); err != nil {
		return nil, media.Reject(media.BindingConflict, "invalid persisted messages")
	}
	if rawMessages == nil {
		return nil, nil
	}
	messages := make([]fantasy.Message, 0, len(rawMessages))
	for _, raw := range rawMessages {
		message, err := decodePersistentMessage(raw)
		if err != nil {
			return nil, err
		}
		messages = append(messages, message)
	}
	if _, err := ValidatePersistentMessages(messages); err != nil {
		return nil, err
	}
	return messages, nil
}

func decodePersistentMessage(data []byte) (fantasy.Message, error) {
	var wire struct {
		Role            fantasy.MessageRole `json:"role"`
		Content         []jsontext.Value    `json:"content"`
		ProviderOptions jsontext.Value      `json:"provider_options"`
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		return fantasy.Message{}, media.Reject(media.BindingConflict, "invalid persisted message")
	}
	// Let the pinned SDK restore registered options. No content is supplied to
	// that decoder until its discriminator has been checked below.
	metadata, err := json.Marshal(struct {
		Role            fantasy.MessageRole `json:"role"`
		Content         []jsontext.Value    `json:"content"`
		ProviderOptions jsontext.Value      `json:"provider_options"`
	}{wire.Role, nil, wire.ProviderOptions})
	if err != nil {
		return fantasy.Message{}, err
	}
	var message fantasy.Message
	if err := message.UnmarshalJSON(metadata); err != nil {
		return fantasy.Message{}, media.Reject(media.BindingConflict, "invalid history provider options")
	}
	message.Content = make([]fantasy.MessagePart, 0, len(wire.Content))
	for _, rawPart := range wire.Content {
		part, err := decodePersistentPart(rawPart)
		if err != nil {
			return fantasy.Message{}, err
		}
		message.Content = append(message.Content, part)
	}
	return message, nil
}

func decodePersistentPart(data []byte) (fantasy.MessagePart, error) {
	var envelope struct {
		Type fantasy.ContentType `json:"type"`
		Data jsontext.Value      `json:"data"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		return nil, invalidHistoryPart()
	}
	switch envelope.Type {
	case ContentTypeMediaReference:
		return DecodeMediaReferencePart(data)
	case fantasy.ContentTypeText, fantasy.ContentTypeReasoning, fantasy.ContentTypeToolCall:
	case fantasy.ContentTypeToolResult:
		// Reject media before the SDK's output decoder can Base64-decode it.
		var result struct {
			Output struct {
				Type fantasy.ToolResultContentType `json:"type"`
			} `json:"output"`
		}
		if err := json.Unmarshal(envelope.Data, &result); err != nil {
			return nil, invalidHistoryPart()
		}
		if result.Output.Type != fantasy.ToolResultContentTypeText && result.Output.Type != fantasy.ToolResultContentTypeError {
			return nil, invalidHistoryPart()
		}
	default:
		return nil, invalidHistoryPart()
	}
	part, err := fantasy.UnmarshalMessagePart(data)
	if err != nil {
		return nil, media.Reject(media.BindingConflict, "invalid persisted message part")
	}
	return part, nil
}
