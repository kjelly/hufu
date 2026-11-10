package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"reflect"

	"charm.land/fantasy"
	"github.com/kjelly/hufu/internal/media"
)

func validateCompiledReferenceRequest(original, wire ProviderRequest) error {
	return validateCompiledReferenceRequestWithPart(original, wire, validateCompiledImage)
}

func validateCompiledReferenceRequestWithPart(original, wire ProviderRequest, validatePart func(media.HistoryReference, fantasy.MessagePart) error) error {
	if err := validateCompiledReferenceMessagesWithPart(original.Messages, wire.Messages, validatePart); err != nil {
		return err
	}
	if original.Call != nil {
		before, after := *original.Call, *wire.Call
		before.Prompt, after.Prompt = nil, nil
		if !reflect.DeepEqual(before, after) {
			return media.Reject(media.BindingConflict, "request compiler changed admitted call parameters")
		}
		return validateCompiledReferenceMessagesWithPart(original.Call.Prompt, wire.Call.Prompt, validatePart)
	}
	if original.ObjectCall != nil {
		before, after := *original.ObjectCall, *wire.ObjectCall
		before.Prompt, after.Prompt = nil, nil
		if !reflect.DeepEqual(before, after) {
			return media.Reject(media.BindingConflict, "request compiler changed admitted object parameters")
		}
		return validateCompiledReferenceMessagesWithPart(original.ObjectCall.Prompt, wire.ObjectCall.Prompt, validatePart)
	}
	return nil
}

func validateCompiledReferenceMessagesWithPart(original, wire []fantasy.Message, validatePart func(media.HistoryReference, fantasy.MessagePart) error) error {
	if len(original) != len(wire) {
		return media.Reject(media.BindingConflict, "request compiler changed message count")
	}
	for index, message := range original {
		compiled := wire[index]
		if message.Role != compiled.Role || !reflect.DeepEqual(message.ProviderOptions, compiled.ProviderOptions) || len(message.Content) != len(compiled.Content) {
			return media.Reject(media.BindingConflict, "request compiler changed message shape")
		}
		for partIndex, part := range message.Content {
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
				if !reflect.DeepEqual(part, compiled.Content[partIndex]) {
					return media.Reject(media.BindingConflict, "request compiler changed non-media content")
				}
				continue
			}
			if err := validatePart(*reference, compiled.Content[partIndex]); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateCompiledImage(reference media.HistoryReference, part fantasy.MessagePart) error {
	if err := reference.Validate(); err != nil {
		return err
	}
	var file fantasy.FilePart
	switch p := part.(type) {
	case fantasy.FilePart:
		file = p
	case *fantasy.FilePart:
		if p == nil {
			return media.Reject(media.IntegrityMismatch, "compiled image is missing")
		}
		file = *p
	default:
		return media.Reject(media.IntegrityMismatch, "compiled image is not a native file part")
	}
	if file.Filename != "" || len(file.ProviderOptions) != 0 || file.MediaType != reference.Ref.MediaType || int64(len(file.Data)) != reference.Ref.ByteSize {
		return media.Reject(media.IntegrityMismatch, "compiled image differs from its admitted reference")
	}
	digest := sha256.Sum256(file.Data)
	if hex.EncodeToString(digest[:]) != reference.Ref.SHA256 {
		return media.Reject(media.IntegrityMismatch, "compiled image content differs from its admitted digest")
	}
	return nil
}
