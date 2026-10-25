package agent

import (
	"context"
	"net/http"
	"sync"

	"charm.land/fantasy"
)

type providerRejectionKey struct{}
type providerRejectionObservation struct {
	mu  sync.Mutex
	err *fantasy.ProviderError
}

func withProviderRejectionObservation(ctx context.Context) context.Context {
	return context.WithValue(ctx, providerRejectionKey{}, &providerRejectionObservation{})
}

// The OpenAI SDK delays HTTP errors until stream iteration. Capture the actual
// status at the trusted transport, before returning a stream to the caller.
// SSE error text and model output cannot populate this observation.
func observeProviderRejection(ctx context.Context, response *http.Response) {
	observation, _ := ctx.Value(providerRejectionKey{}).(*providerRejectionObservation)
	if observation == nil || response == nil || response.StatusCode != http.StatusTooManyRequests {
		return
	}
	observation.mu.Lock()
	defer observation.mu.Unlock()
	observation.err = &fantasy.ProviderError{StatusCode: http.StatusTooManyRequests, Message: "provider rejected request: HTTP 429", ResponseHeaders: map[string]string{"Retry-After": response.Header.Get("Retry-After")}}
}

func observedProviderRejection(ctx context.Context) error {
	observation, _ := ctx.Value(providerRejectionKey{}).(*providerRejectionObservation)
	if observation == nil {
		return nil
	}
	observation.mu.Lock()
	defer observation.mu.Unlock()
	if observation.err == nil {
		return nil
	}
	return observation.err
}

func (m *admittedLanguageModel) streamRejection(ctx context.Context, request ProviderRequest) error {
	recovery, _ := m.admission.(ProviderRequestRecovery)
	if recovery == nil || !recovery.CanRecoverProviderRequest(request) {
		return nil
	}
	return observedProviderRejection(ctx)
}
