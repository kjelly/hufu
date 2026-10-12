package agent

import (
	"context"
	"io"
	"net/http"
)

const maxObservedProviderBody = 2 * 1024 * 1024

// ProviderTransportObservation describes an attempted transport request. Body
// bytes are transient: an observer must not persist credentials or prompt text.
// A receiving fake provider must confirm delivery before counting exposure.
type ProviderTransportObservation struct {
	InvocationID      string
	ModelID           string
	RequestBody       []byte
	UnavailableReason string
}

type providerObserverKey struct{}
type providerIdentityKey struct{}
type providerTransportIdentity struct{ invocationID, modelID string }

// WithProviderTransportObserver installs a request-scoped diagnostic observer.
// It cannot alter admission, routing, request bytes, or a provider result.
func WithProviderTransportObserver(ctx context.Context, observer func(context.Context, ProviderTransportObservation)) context.Context {
	return context.WithValue(ctx, providerObserverKey{}, observer)
}

// ProviderTransportObserverEnabled reports whether request diagnostics were
// explicitly installed. It does not imply admission or tool authorization.
func ProviderTransportObserverEnabled(ctx context.Context) bool {
	observer, ok := ctx.Value(providerObserverKey{}).(func(context.Context, ProviderTransportObservation))
	return ok && observer != nil
}

func withProviderTransportIdentity(ctx context.Context, request ProviderRequest) context.Context {
	if _, ok := ctx.Value(providerObserverKey{}).(func(context.Context, ProviderTransportObservation)); !ok {
		return ctx
	}
	return context.WithValue(ctx, providerIdentityKey{}, providerTransportIdentity{request.InvocationID, request.ModelID})
}

func observeProviderTransport(request *http.Request) {
	ctx := request.Context()
	observer, ok := ctx.Value(providerObserverKey{}).(func(context.Context, ProviderTransportObservation))
	if !ok || observer == nil {
		return
	}
	identity, ok := ctx.Value(providerIdentityKey{}).(providerTransportIdentity)
	if !ok {
		return // Metadata/model-list calls are not model invocations.
	}
	observation := ProviderTransportObservation{InvocationID: identity.invocationID, ModelID: identity.modelID}
	if request.GetBody == nil {
		observation.UnavailableReason = "request_body_unavailable"
		observer(ctx, observation)
		return
	}
	body, err := request.GetBody()
	if err != nil {
		observation.UnavailableReason = "request_body_unavailable"
		observer(ctx, observation)
		return
	}
	defer func() { _ = body.Close() }()
	data, err := io.ReadAll(io.LimitReader(body, maxObservedProviderBody+1))
	switch {
	case err != nil:
		observation.UnavailableReason = "request_body_unavailable"
	case len(data) > maxObservedProviderBody:
		observation.UnavailableReason = "request_body_over_limit"
	default:
		observation.RequestBody = data
	}
	observer(ctx, observation)
}
