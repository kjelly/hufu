package agent

import (
	"encoding/json/v2"
	"slices"

	"charm.land/fantasy"
	"github.com/kjelly/hufu/internal/media"
)

// ReferenceMetadataText describes an admitted image without its bytes, CAS
// identifier, digest or source filename. Rendering grants no image access.
func ReferenceMetadataText(reference media.HistoryReference) (string, error) {
	if err := reference.Validate(); err != nil {
		return "", err
	}
	data, err := json.Marshal(struct {
		TurnID    string `json:"turn_id"`
		PartIndex int    `json:"part_index"`
		MediaType string `json:"media_type"`
		Width     int    `json:"width"`
		Height    int    `json:"height"`
		ByteSize  int64  `json:"byte_size"`
	}{reference.Source.TurnID, reference.Source.PartIndex, reference.Ref.MediaType, reference.Ref.Width, reference.Ref.Height, reference.Ref.ByteSize})
	if err != nil {
		return "", err
	}
	return "Image input metadata (image content is not provided): " + string(data), nil
}

// ProjectProviderReferenceMetadata creates a private, byte-free wire view.
// Callers still need explicit metadata admission before a provider can use it.
func ProjectProviderReferenceMetadata(request ProviderRequest) (ProviderRequest, error) {
	project := func(messages []fantasy.Message) ([]fantasy.Message, error) {
		messages = slices.Clone(messages)
		for index := range messages {
			messages[index].Content = slices.Clone(messages[index].Content)
			for partIndex, part := range messages[index].Content {
				var reference *media.HistoryReference
				switch p := part.(type) {
				case MediaReferencePart:
					reference = &p.Reference
				case *MediaReferencePart:
					if p != nil {
						reference = &p.Reference
					}
				}
				if reference == nil {
					continue
				}
				text, err := ReferenceMetadataText(*reference)
				if err != nil {
					return nil, err
				}
				messages[index].Content[partIndex] = fantasy.TextPart{Text: text}
			}
		}
		return messages, nil
	}
	var err error
	request.Messages, err = project(request.Messages)
	if err != nil {
		return ProviderRequest{}, err
	}
	if request.Call != nil {
		call := *request.Call
		call.Prompt, err = project(call.Prompt)
		if err != nil {
			return ProviderRequest{}, err
		}
		request.Call = &call
	}
	if request.ObjectCall != nil {
		call := *request.ObjectCall
		call.Prompt, err = project(call.Prompt)
		if err != nil {
			return ProviderRequest{}, err
		}
		request.ObjectCall = &call
	}
	return request, nil
}

func validateCompiledMetadata(reference media.HistoryReference, part fantasy.MessagePart) error {
	text, err := ReferenceMetadataText(reference)
	if err != nil {
		return err
	}
	if nilContentPart(part) {
		return media.Reject(media.BindingConflict, "compiled metadata is missing")
	}
	compiled, ok := fantasy.AsMessagePart[fantasy.TextPart](part)
	if !ok || compiled.Text != text || len(compiled.ProviderOptions) != 0 {
		return media.Reject(media.BindingConflict, "compiled metadata differs from its canonical reference")
	}
	return nil
}
