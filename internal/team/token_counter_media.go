package team

import (
	"context"
	"slices"

	"charm.land/fantasy"
	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/media"
)

type nativeProviderCounterKey struct{}

func withNativeProviderCounter(ctx context.Context, c *Coordinator) context.Context {
	return context.WithValue(ctx, nativeProviderCounterKey{}, c)
}

func (tc *DefaultTokenCounter) countNativeProviderRequest(ctx context.Context, modelID string, request agent.ProviderRequest) (int, error) {
	c, _ := ctx.Value(nativeProviderCounterKey{}).(*Coordinator)
	if c == nil || !request.AdmissionContext.IsBound() || request.AdmissionContext.ModelID == "" {
		return 0, media.Reject(media.Unsupported, "native image counter requires current task admission")
	}
	actualModel := request.AdmissionContext.ModelID
	if modelID != actualModel {
		// PrepareStep receives the SDK's bare model name; the immutable bound
		// selector remains authoritative. Model names may themselves contain /.
		target, err := c.resolveCanonicalTaskTarget(actualModel, "")
		if err != nil || modelID != target.Model {
			return 0, media.Reject(media.EgressDenied, "image counter model differs from its bound target")
		}
	}
	request.ModelID = actualModel
	metadata, err := c.resolveProviderReferenceMetadata(ctx, request)
	if err != nil {
		return 0, err
	}
	if metadata {
		text, err := agent.ProjectProviderReferenceMetadata(request)
		if err != nil {
			return 0, err
		}
		return tc.CountProviderRequest(ctx, actualModel, text)
	}
	admission, err := c.resolveNativeProviderMedia(ctx, request)
	if err != nil {
		return 0, err
	}
	return tc.countAdmittedNativeRequest(ctx, request, admission.ImageTokens)
}

func (tc *DefaultTokenCounter) countAdmittedNativeRequest(ctx context.Context, request agent.ProviderRequest, imageTokens int64) (int, error) {
	if imageTokens <= 0 {
		return 0, media.Reject(media.Unsupported, "native image token upper bound is unavailable")
	}
	text := nativeRequestTextProjection(request)
	textTokens, err := tc.CountProviderRequest(ctx, request.ModelID, text)
	if err != nil {
		return 0, err
	}
	if imageTokens > int64(^uint(0)>>1)-int64(textTokens) {
		return 0, media.Reject(media.LimitExceeded, "native image estimate exceeds token counter capacity")
	}
	return textTokens + int(imageTokens), nil
}

// nativeRequestTextProjection is counting-only. It preserves complete text,
// roles, tool schemas and call options without serializing image bytes or
// treating inert reference metadata as text sent to the provider.
func nativeRequestTextProjection(request agent.ProviderRequest) agent.ProviderRequest {
	project := func(messages []fantasy.Message) []fantasy.Message {
		result := slices.Clone(messages)
		for index, message := range messages {
			result[index].Content = make([]fantasy.MessagePart, 0, len(message.Content))
			for _, part := range message.Content {
				switch part.(type) {
				case agent.MediaReferencePart, *agent.MediaReferencePart:
					continue
				}
				result[index].Content = append(result[index].Content, part)
			}
		}
		return result
	}
	request.Messages = project(request.Messages)
	if request.Call != nil {
		call := *request.Call
		call.Prompt = project(call.Prompt)
		request.Call = &call
	}
	if request.ObjectCall != nil {
		call := *request.ObjectCall
		call.Prompt = project(call.Prompt)
		request.ObjectCall = &call
	}
	return request
}
