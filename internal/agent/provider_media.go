package agent

import "charm.land/fantasy"

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
				if part != nil && part.GetType() == ContentTypeMediaReference {
					return true
				}
			}
		}
		return false
	})
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
				if toolResultHasMedia(p.Output) {
					return true
				}
			case *fantasy.ToolResultPart:
				if p == nil || toolResultHasMedia(p.Output) {
					return true
				}
			default:
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
	return output != nil && output.GetType() == fantasy.ToolResultContentTypeMedia
}
