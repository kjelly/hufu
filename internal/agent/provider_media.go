package agent

import (
	"reflect"
	"strings"

	"charm.land/fantasy"
)

func ProviderRequestHasMedia(request ProviderRequest) bool {
	return requestHasMessageParts(request, MessagesHaveMedia)
}

func providerRequestHasReferences(request ProviderRequest) bool {
	return requestHasMessageParts(request, func(messages []fantasy.Message) bool {
		for _, message := range messages {
			for _, part := range message.Content {
				switch part.(type) {
				case MediaReferencePart, *MediaReferencePart:
					return true
				}
				switch p := part.(type) {
				case fantasy.ToolResultPart:
					if strings.HasPrefix(p.ClientMetadata, toolMediaMetadataPrefix) {
						return true
					}
				case *fantasy.ToolResultPart:
					if p != nil && strings.HasPrefix(p.ClientMetadata, toolMediaMetadataPrefix) {
						return true
					}
				}
				if !nilContentPart(part) && part.GetType() == ContentTypeMediaReference {
					return true
				}
			}
		}
		return false
	})
}

func nilContentPart(part any) bool {
	if part == nil {
		return true
	}
	value := reflect.ValueOf(part)
	return value.Kind() == reflect.Pointer && value.IsNil()
}

func requestHasMessageParts(request ProviderRequest, match func([]fantasy.Message) bool) bool {
	if match(request.Messages) {
		return true
	}
	if request.Call != nil && match(request.Call.Prompt) {
		return true
	}
	return request.ObjectCall != nil && match(request.ObjectCall.Prompt)
}

func MessagesHaveMedia(messages []fantasy.Message) bool {
	for _, message := range messages {
		for _, part := range message.Content {
			switch p := part.(type) {
			case fantasy.FilePart, *fantasy.FilePart, MediaReferencePart, *MediaReferencePart:
				// MIME cannot route files through a text-only request counter.
				return true
			case fantasy.ToolResultPart:
				if strings.HasPrefix(p.ClientMetadata, toolMediaMetadataPrefix) || toolResultHasMedia(p.Output) {
					return true
				}
			case *fantasy.ToolResultPart:
				if p == nil || strings.HasPrefix(p.ClientMetadata, toolMediaMetadataPrefix) || toolResultHasMedia(p.Output) {
					return true
				}
			default:
				if nilContentPart(part) {
					return true
				}
				if part != nil && (part.GetType() == fantasy.ContentTypeFile || part.GetType() == ContentTypeMediaReference) {
					return true
				}
			}
		}
	}
	return false
}

func toolResultHasMedia(output fantasy.ToolResultOutputContent) bool {
	switch output.(type) {
	case fantasy.ToolResultOutputContentMedia, *fantasy.ToolResultOutputContentMedia:
		return true
	}
	if output != nil && nilContentPart(output) {
		return true
	}
	return output != nil && output.GetType() == fantasy.ToolResultContentTypeMedia
}
