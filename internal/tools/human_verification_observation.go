package tools

import "context"

// HumanVerificationObservation contains metadata only. Never persist the
// confirmation code, the operator's answer, page text, or the prompt here.
type HumanVerificationObservation struct {
	RequestID string `json:"request_id"`
	URLSHA256 string `json:"url_sha256,omitempty"`
	Attempt   int    `json:"request_attempt"`
	Trigger   string `json:"trigger"`
	Purpose   string `json:"purpose,omitempty"`
	Status    string `json:"status"`
}

type humanVerificationRequestKey struct{}
type humanVerificationObserverKey struct{}

// HumanVerificationRequiresAuthentication is set only by the native browser's
// typed page classification, never by model-supplied prompt text.
func HumanVerificationRequiresAuthentication(ctx context.Context) bool {
	request, _ := ctx.Value(humanVerificationRequestKey{}).(HumanVerificationObservation)
	return request.Purpose == "authentication"
}

func WithHumanVerificationRequest(ctx context.Context, request HumanVerificationObservation) context.Context {
	return context.WithValue(ctx, humanVerificationRequestKey{}, request)
}

func WithHumanVerificationObserver(ctx context.Context, observe func(context.Context, HumanVerificationObservation) error) context.Context {
	return context.WithValue(ctx, humanVerificationObserverKey{}, observe)
}

// ObserveHumanVerification aborts the operation if a configured observer fails.
func ObserveHumanVerification(ctx context.Context, status string) error {
	observe, _ := ctx.Value(humanVerificationObserverKey{}).(func(context.Context, HumanVerificationObservation) error)
	if observe == nil {
		return nil
	}
	request, _ := ctx.Value(humanVerificationRequestKey{}).(HumanVerificationObservation)
	request.Status = status
	return observe(ctx, request)
}
