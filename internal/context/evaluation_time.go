package context

import (
	"context"
	"time"
)

type evaluationTimeKey struct{}

// WithEvaluationTime fixes the eligibility instant for trusted offline replay.
// Runtime tools never accept this value from model arguments. It is scoped to
// the supplied context and does not replace the clock for process lifecycles.
func WithEvaluationTime(ctx context.Context, instant time.Time) context.Context {
	return context.WithValue(ctx, evaluationTimeKey{}, instant.UTC())
}

// EvaluationTime returns the replay instant, or the current runtime instant.
func EvaluationTime(ctx context.Context) time.Time {
	if instant, ok := ctx.Value(evaluationTimeKey{}).(time.Time); ok && !instant.IsZero() {
		return instant
	}
	return time.Now().UTC()
}
