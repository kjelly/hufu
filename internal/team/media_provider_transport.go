package team

import (
	"context"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/media"
)

var _ agent.ProviderTransportAuthorizer = providerRequestAdmission{}

// AuthorizeProviderTransport rechecks the live lease and exact durable
// permission after encoding, immediately before the HTTP transport. It only
// reads canonical metadata; the private compiler already verified wire bytes.
func (a providerRequestAdmission) AuthorizeProviderTransport(ctx context.Context, request agent.ProviderRequest) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !providerRequestHasMedia(request) {
		return nil
	}
	if a.c == nil || request.InvocationID == "" {
		return media.Reject(media.DurabilityRequired, "image transport has no committed invocation")
	}
	admission, expected, err := a.c.mediaProviderAdmissionPayload(ctx, request)
	if err != nil {
		return err
	}
	view, err := a.c.mediaLineage(ctx)
	if err != nil {
		return err
	}
	found := false
	for index, event := range view.events {
		if event.Type != string(EventMediaProviderAdmitted) {
			continue
		}
		payload, err := decodeMediaProviderEvent(event)
		if err != nil {
			return err
		}
		if payload.ProviderInvocationID != request.InvocationID {
			continue
		}
		if payload != expected || event.RunID != admission.Identity.RunID || event.TaskID != admission.Identity.TaskID ||
			event.Actor != admission.Identity.Agent || event.BranchID != view.branch {
			return media.Reject(media.AccessDenied, "image transport differs from its committed permission")
		}
		if err := verifyMediaProviderAdmission(event, payload, view.events[:index]); err != nil {
			return err
		}
		found = true
	}
	if !found {
		return media.Reject(media.DurabilityRequired, "image transport admission evidence is unavailable")
	}
	// Reading the journal can wait behind another writer. Recheck the live
	// authority after that wait rather than accepting its earlier projection.
	_, err = a.c.resolveNativeProviderMedia(ctx, request)
	return err
}
