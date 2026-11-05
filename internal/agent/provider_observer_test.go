package agent

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"charm.land/fantasy"
)

func TestProviderTransportObserverCopiesBodyAndIdentityWithoutChangingRequest(t *testing.T) {
	const body = `{"messages":[{"role":"user","content":"sentinel"}]}`
	var observed ProviderTransportObservation
	ctx := WithProviderTransportObserver(t.Context(), func(_ context.Context, observation ProviderTransportObservation) {
		observed = observation
		observation.RequestBody[0] = 'x'
	})
	ctx = withProviderTransportIdentity(ctx, ProviderRequest{InvocationID: "invocation-1", ModelID: "model-1"})
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://localhost/v1/chat/completions", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	observeProviderTransport(request)
	if observed.InvocationID != "invocation-1" || observed.ModelID != "model-1" || observed.UnavailableReason != "" {
		t.Fatalf("observation identity = %+v", observed)
	}
	actual, err := io.ReadAll(request.Body)
	if err != nil || string(actual) != body {
		t.Fatalf("observer changed request: %q, error %v", actual, err)
	}
}

func TestProviderTransportObserverOmitsMediaBody(t *testing.T) {
	var observed ProviderTransportObservation
	ctx := WithProviderTransportObserver(t.Context(), func(_ context.Context, observation ProviderTransportObservation) { observed = observation })
	ctx = withProviderTransportIdentity(ctx, ProviderRequest{InvocationID: "media-invocation", ModelID: "fixture", Messages: []fantasy.Message{{Role: fantasy.MessageRoleUser, Content: historyReferenceParts(t)}}})
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://localhost/v1/chat/completions", strings.NewReader("image wire sentinel"))
	if err != nil {
		t.Fatal(err)
	}
	request.GetBody = func() (io.ReadCloser, error) { t.Fatal("observer read a media body"); return nil, nil }
	observeProviderTransport(request)
	if observed.InvocationID != "media-invocation" || observed.ModelID != "fixture" || observed.RequestBody != nil || observed.UnavailableReason != "media_body_omitted" {
		t.Fatal("media body escaped metadata-only observation")
	}
}

func TestProviderTransportObserverBoundsDiagnosticsAndIgnoresMetadata(t *testing.T) {
	var observations []ProviderTransportObservation
	ctx := WithProviderTransportObserver(t.Context(), func(_ context.Context, observation ProviderTransportObservation) {
		observations = append(observations, observation)
	})
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://localhost/v1/models", nil)
	if err != nil {
		t.Fatal(err)
	}
	observeProviderTransport(request)
	if len(observations) != 0 {
		t.Fatal("model metadata was counted as a model invocation")
	}
	ctx = withProviderTransportIdentity(ctx, ProviderRequest{InvocationID: "invocation-1", ModelID: "model-1"})
	request, err = http.NewRequestWithContext(ctx, http.MethodPost, "http://localhost/v1/chat/completions", strings.NewReader(strings.Repeat("x", maxObservedProviderBody+1)))
	if err != nil {
		t.Fatal(err)
	}
	observeProviderTransport(request)
	if len(observations) != 1 || observations[0].UnavailableReason != "request_body_over_limit" || observations[0].RequestBody != nil {
		t.Fatal("oversized body escaped the diagnostic limit")
	}
}
