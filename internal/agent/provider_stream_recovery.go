package agent

import (
	"context"
	"errors"
	"iter"
	"net/http"

	"charm.land/fantasy"
)

// ProviderTransientRetryLimit bounds resends within one model step. Quota
// cooldowns retain their separate policy; neither path repeats worker tools.
const ProviderTransientRetryLimit = 2

type providerRetryState struct{ transientRetries int }

// IsTransientProviderError uses the SDK's typed retry signal, including SSE
// errors with no HTTP failure status. Error messages never choose this policy.
func IsTransientProviderError(err error) bool {
	failure, ok := errors.AsType[*fantasy.ProviderError](err)
	if !ok || failure == nil || errors.Is(err, context.Canceled) || failure.AuthError || failure.IsContextTooLarge() {
		return false
	}
	if failure.StatusCode >= 400 && failure.StatusCode < 500 &&
		failure.StatusCode != http.StatusRequestTimeout && failure.StatusCode != http.StatusConflict {
		return false
	}
	return failure.IsRetryable()
}

func isRecoverableProviderRejection(err error) bool {
	failure, ok := errors.AsType[*fantasy.ProviderError](err)
	return ok && (IsTransientProviderError(err) || failure.StatusCode == http.StatusTooManyRequests && !failure.AuthError)
}

func (m *admittedLanguageModel) recoverRejectedRequest(ctx context.Context, request ProviderRequest, state *providerRetryState, cause error) (bool, error) {
	recovery, ok := m.admission.(ProviderRequestRecovery)
	if !ok || !recovery.CanRecoverProviderRequest(request) || !isRecoverableProviderRejection(cause) {
		return false, nil
	}
	transient := IsTransientProviderError(cause)
	if transient && state.transientRetries >= ProviderTransientRetryLimit {
		return false, nil
	}
	request.TransientRetries = state.transientRetries
	retry, err := recovery.RecoverProviderRequest(ctx, request, cause)
	if err == nil && retry && transient {
		state.transientRetries++
	}
	return retry, err
}

// A deferred SDK rejection can arrive only when the iterator is consumed.
// Let admittedStream settle/release that invocation before considering a resend.
// Once ANY part reaches the consumer, replay is forbidden: even a partial tool
// call or provider-executed event may have started downstream work.
func (m *admittedLanguageModel) recoverRejectedStream[T any](ctx context.Context, request ProviderRequest, state *providerRetryState, initial iter.Seq[T], open func() (iter.Seq[T], error), partError func(T) error, errorPart func(error) T) iter.Seq[T] {
	recovery, ok := m.admission.(ProviderRequestRecovery)
	if !ok || !recovery.CanRecoverProviderRequest(request) {
		return initial
	}
	return func(yield func(T) bool) {
		stream := initial
		for {
			forwarded, consuming := false, true
			var rejection error
			var rejectedPart T
			stream(func(part T) bool {
				if cause := partError(part); !forwarded && isRecoverableProviderRejection(cause) {
					rejection, rejectedPart = cause, part
					return false
				}
				forwarded = true
				consuming = yield(part)
				return consuming
			})
			if !consuming || rejection == nil {
				return
			}
			retry, err := m.recoverRejectedRequest(ctx, request, state, rejection)
			if err != nil {
				yield(errorPart(err))
				return
			}
			if !retry {
				yield(rejectedPart)
				return
			}
			stream, err = m.recover(ctx, request, state, func() (iter.Seq[T], bool, error) {
				next, err := open()
				return next, next != nil, err
			})
			if err != nil {
				yield(errorPart(err))
				return
			}
		}
	}
}
