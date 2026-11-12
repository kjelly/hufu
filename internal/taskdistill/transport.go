package taskdistill

import (
	"context"
	"errors"
	"fmt"
	"time"
)

var ErrTransport = errors.New("distillation provider transport interrupted")

// TransportError deliberately omits the URL, which may contain a private relay
// capability. Its original typed cause remains available through errors.Is/As.
type TransportError struct{ cause error }

// NewTransportError marks an EOF observed at the trusted provider HTTP boundary,
// rather than an admission, checkpoint, or model JSON parsing failure.
func NewTransportError(cause error) error { return &TransportError{cause: cause} }

func (e *TransportError) Error() string { return ErrTransport.Error() + " (eof)" }
func (e *TransportError) Unwrap() []error {
	if e.cause == nil {
		return []error{ErrTransport}
	}
	return []error{ErrTransport, e.cause}
}

func (r *draftRunner) generateText(ctx context.Context, prompt string) (string, error) {
	for attempt := 0; ; attempt++ {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		if r.remaining <= 0 {
			return "", ErrCallBudget
		}
		r.remaining--
		raw, err := r.generator.GenerateText(ctx, prompt)
		if err == nil {
			return raw, nil
		}
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		if !errors.Is(err, ErrTransport) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return "", err
		}
		if r.remaining <= 0 {
			return "", fmt.Errorf("%w: %w", ErrCallBudget, err)
		}
		// This generator only produces a draft, never tools or cleanup effects.
		// Resend the identical frozen prompt once; never split evidence because
		// of a transport failure, or reuse any accompanying partial response.
		if attempt >= 1 || raw != "" {
			return "", err
		}
		timer := time.NewTimer(time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return "", ctx.Err()
		case <-timer.C:
		}
	}
}
