package team

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
