package team

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"strings"
	"syscall"

	"charm.land/fantasy"
)

// ProviderFailureClass classifies why a model call failed at the provider
// boundary. It is a fact about the call, distinct from TaskFailureClass,
// which judges the task: verification, semantic, and policy failures are
// never provider failures.
type ProviderFailureClass string

const (
	// ProviderRateLimited is HTTP 429.
	ProviderRateLimited ProviderFailureClass = "rate_limited"
	// ProviderUnavailable is HTTP 500/502/503/504, a refused or reset
	// connection, DNS failure, or a TLS handshake failure.
	ProviderUnavailable ProviderFailureClass = "provider_unavailable"
	// ProviderModelUnavailable is HTTP 404 or a documented "model not found".
	ProviderModelUnavailable ProviderFailureClass = "model_unavailable"
	// ProviderTransportTimeout is a provider transport timeout, not a task
	// timeout.
	ProviderTransportTimeout ProviderFailureClass = "transport_timeout"
	// ProviderAuthFailed is HTTP 401/403. It is never fallback-eligible.
	ProviderAuthFailed ProviderFailureClass = "auth_failed"
	// ProviderContextExceeded is a context-length rejection. It is never
	// fallback-eligible.
	ProviderContextExceeded ProviderFailureClass = "context_length_exceeded"
	ProviderOther           ProviderFailureClass = "other"
)

// fallbackEligibleProviderFailures are the only classes an execution route's
// fallback-on may name.
var fallbackEligibleProviderFailures = []ProviderFailureClass{
	ProviderRateLimited, ProviderUnavailable, ProviderModelUnavailable, ProviderTransportTimeout,
}

// knownProviderFailureClasses lists every class, so a route naming a class
// that can never trigger a fallback gets a precise error.
var knownProviderFailureClasses = []ProviderFailureClass{
	ProviderRateLimited, ProviderUnavailable, ProviderModelUnavailable, ProviderTransportTimeout,
	ProviderAuthFailed, ProviderContextExceeded, ProviderOther,
}

// modelNotFoundBodyMarkers are the documented "model not found" bodies of
// the OpenAI-compatible backends Hufu talks to, for servers that report a
// missing model with a status other than 404.
var modelNotFoundBodyMarkers = []string{
	"model_not_found",                 // OpenAI and gateways that keep its code
	"not found, try pulling it first", // Ollama
}

// ClassifyProviderError classifies a model-call error from its structure
// only: the provider's HTTP status, or the transport error it wraps. It
// never reads free text except the explicit model-not-found table above.
// A context deadline is classified as a transport timeout; callers must
// rule out a task timeout or cancellation from their own context first.
func ClassifyProviderError(err error) ProviderFailureClass {
	if err == nil {
		return ""
	}
	// Cost admission is a local policy decision even when its durable append
	// cause happens to look like a transport timeout. It must never enter the
	// provider-route fallback mechanism or select a cheaper candidate.
	if _, ok := errors.AsType[*CostAdmissionError](err); ok {
		return ProviderOther
	}
	var providerErr *fantasy.ProviderError
	if errors.As(err, &providerErr) {
		switch {
		case providerErr.IsContextTooLarge():
			return ProviderContextExceeded
		case providerErr.AuthError || providerErr.StatusCode == http.StatusUnauthorized || providerErr.StatusCode == http.StatusForbidden:
			return ProviderAuthFailed
		case providerErr.StatusCode == http.StatusTooManyRequests:
			return ProviderRateLimited
		case providerErr.StatusCode == http.StatusNotFound:
			return ProviderModelUnavailable
		case providerErr.StatusCode == http.StatusInternalServerError, providerErr.StatusCode == http.StatusBadGateway,
			providerErr.StatusCode == http.StatusServiceUnavailable, providerErr.StatusCode == http.StatusGatewayTimeout:
			return ProviderUnavailable
		case providerErr.StatusCode >= 400 && providerErr.StatusCode < 500:
			if modelNotFoundBody(providerErr) {
				return ProviderModelUnavailable
			}
			return ProviderOther
		}
	}
	return classifyProviderTransportError(err)
}

func modelNotFoundBody(providerErr *fantasy.ProviderError) bool {
	body := string(providerErr.ResponseBody) + "\n" + providerErr.Message
	for _, marker := range modelNotFoundBodyMarkers {
		if strings.Contains(body, marker) {
			return true
		}
	}
	return false
}

func classifyProviderTransportError(err error) ProviderFailureClass {
	var dnsErr *net.DNSError
	var tlsRecordErr tls.RecordHeaderError
	var tlsAlertErr tls.AlertError
	var certErr *tls.CertificateVerificationError
	switch {
	case errors.As(err, &dnsErr):
		return ProviderUnavailable
	case errors.Is(err, syscall.ECONNREFUSED), errors.Is(err, syscall.ECONNRESET):
		return ProviderUnavailable
	case errors.As(err, &tlsRecordErr), errors.As(err, &tlsAlertErr), errors.As(err, &certErr):
		return ProviderUnavailable
	case errors.Is(err, context.DeadlineExceeded):
		return ProviderTransportTimeout
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return ProviderTransportTimeout
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		return ProviderUnavailable
	}
	return ProviderOther
}
