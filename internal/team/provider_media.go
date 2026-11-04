package team

import (
	"charm.land/fantasy"

	"github.com/kjelly/hufu/internal/agent"
)

func providerRequestHasMedia(request agent.ProviderRequest) bool {
	if messagesHaveMedia(request.Messages) {
		return true
	}
	if request.Call != nil && messagesHaveMedia(request.Call.Prompt) {
		return true
	}
	return request.ObjectCall != nil && messagesHaveMedia(request.ObjectCall.Prompt)
}

func messagesHaveMedia(messages []fantasy.Message) bool {
	for _, message := range messages {
		for _, part := range message.Content {
			switch p := part.(type) {
			case fantasy.FilePart, *fantasy.FilePart:
				// Reject files regardless of claimed MIME. An untrusted MIME
				// must not route raw bytes through the text-only counter.
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
				if part != nil && part.GetType() == fantasy.ContentTypeFile {
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
