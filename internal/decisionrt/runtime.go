package decisionrt

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"math"
	"reflect"
	"slices"
	"time"
)

const (
	defaultDecisionTimeout = 2 * time.Second
	maximumDecisionTimeout = 10 * time.Second
	receiptSchemaVersion   = 1
	lowConfidenceReason    = "low_confidence"
	backendAbstainedReason = "backend_abstained"
)

type namedBackend struct {
	backend Backend
	name    string
}

type runtimeImpl struct {
	primary  namedBackend
	fallback *namedBackend
	policy   AcceptancePolicy
	timeout  time.Duration
	metrics  Metrics
}

func NewRuntime(config RuntimeConfig) (Runtime, error) {
	if isNilInterface(config.Primary) {
		return nil, runtimeError(ErrorConfiguration, "", fmt.Errorf("primary backend is required"))
	}
	timeout := config.Timeout
	if timeout == 0 {
		timeout = defaultDecisionTimeout
	}
	if timeout < 0 || timeout > maximumDecisionTimeout {
		return nil, runtimeError(ErrorConfiguration, "", fmt.Errorf("invalid decision timeout"))
	}
	if config.Policy.MinConfidence != nil {
		minimum := *config.Policy.MinConfidence
		if math.IsNaN(minimum) || math.IsInf(minimum, 0) || minimum < 0 || minimum > 1 {
			return nil, runtimeError(ErrorConfiguration, "", fmt.Errorf("invalid minimum confidence"))
		}
		config.Policy.MinConfidence = new(minimum)
	}

	primaryName := config.Primary.Name()
	if !backendNamePattern.MatchString(primaryName) {
		return nil, runtimeError(ErrorConfiguration, "", fmt.Errorf("invalid primary backend name"))
	}

	var fallback *namedBackend
	if !isNilInterface(config.Fallback) {
		fallbackName := config.Fallback.Name()
		if !backendNamePattern.MatchString(fallbackName) || fallbackName == primaryName {
			return nil, runtimeError(ErrorConfiguration, "", fmt.Errorf("invalid fallback backend"))
		}
		fallback = &namedBackend{backend: config.Fallback, name: fallbackName}
	}

	metrics := config.Metrics
	if isNilInterface(metrics) {
		metrics = noopMetrics{}
	}
	return &runtimeImpl{
		primary:  namedBackend{backend: config.Primary, name: primaryName},
		fallback: fallback,
		policy:   config.Policy,
		timeout:  timeout,
		metrics:  metrics,
	}, nil
}

func (r *runtimeImpl) Decide(ctx context.Context, request Request) (Result, Receipt, error) {
	if ctx == nil {
		return Result{}, Receipt{}, runtimeError(ErrorInvalidRequest, "", fmt.Errorf("context is nil"))
	}
	if err := ctx.Err(); err != nil {
		return Result{}, Receipt{}, runtimeError(ErrorBackendFailure, "", err)
	}
	if err := request.Validate(); err != nil {
		return Result{}, Receipt{}, err
	}
	started := time.Now()
	digest, err := Digest(request)
	if err != nil {
		return Result{}, Receipt{}, err
	}

	backendResult, reason, err := r.attempt(ctx, request, r.primary)
	if err == nil && reason == "" {
		return r.finishDecided(request, digest, started, r.primary, backendResult, false)
	}
	if err != nil && (!shouldFallback(err) || ctx.Err() != nil) {
		return Result{}, Receipt{}, err
	}
	if r.fallback == nil {
		if err != nil {
			return Result{}, Receipt{}, err
		}
		return r.finishAbstained(request, digest, started, r.primary, backendResult.Model, reason, false)
	}
	if err := ctx.Err(); err != nil {
		return Result{}, Receipt{}, runtimeError(ErrorBackendFailure, "", err)
	}

	r.metrics.IncFallback(request.Purpose, r.fallback.name)
	fallbackResult, fallbackReason, fallbackErr := r.attempt(ctx, request, *r.fallback)
	if fallbackErr != nil {
		return Result{}, Receipt{}, fallbackErr
	}
	if fallbackReason != "" {
		return r.finishAbstained(request, digest, started, *r.fallback, fallbackResult.Model, fallbackReason, true)
	}
	return r.finishDecided(request, digest, started, *r.fallback, fallbackResult, true)
}

func (r *runtimeImpl) attempt(ctx context.Context, request Request, backend namedBackend) (BackendResult, string, error) {
	if err := ctx.Err(); err != nil {
		return BackendResult{}, "", runtimeError(ErrorBackendFailure, backend.name, err)
	}

	r.metrics.IncCalls(request.Purpose, backend.name)
	started := time.Now()
	attemptCtx, cancel := context.WithTimeout(ctx, r.timeout)
	if err := attemptCtx.Err(); err != nil {
		cancel()
		r.metrics.ObserveDurationMS(request.Purpose, backend.name, elapsedMilliseconds(started))
		r.metrics.IncErrors(request.Purpose, backend.name)
		return BackendResult{}, "", runtimeError(ErrorBackendFailure, backend.name, err)
	}
	result, err := backend.backend.Decide(attemptCtx, cloneRequest(request))
	attemptContextErr := attemptCtx.Err()
	callerContextErr := ctx.Err()
	cancel()
	r.metrics.ObserveDurationMS(request.Purpose, backend.name, elapsedMilliseconds(started))

	if callerContextErr != nil {
		err = runtimeError(ErrorBackendFailure, backend.name, callerContextErr)
	} else if attemptContextErr != nil {
		err = runtimeError(ErrorBackendFailure, backend.name, attemptContextErr)
	} else if err != nil {
		err = normalizeBackendError(backend.name, err)
	}
	if err != nil {
		r.metrics.IncErrors(request.Purpose, backend.name)
		return BackendResult{}, "", err
	}
	if err := validateBackendResult(request.Spec, result); err != nil {
		err = normalizeBackendError(backend.name, err)
		r.metrics.IncErrors(request.Purpose, backend.name)
		return BackendResult{}, "", err
	}
	if result.Status == StatusAbstained {
		return result, backendAbstainedReason, nil
	}
	if !r.accepts(result) {
		return result, lowConfidenceReason, nil
	}
	return result, "", nil
}

func (r *runtimeImpl) accepts(result BackendResult) bool {
	if r.policy.RequireCalibratedConfidence && result.ConfidenceSemantics != ConfidenceCalibrated {
		return false
	}
	if r.policy.MinConfidence == nil {
		return true
	}
	if result.ConfidenceSemantics == ConfidenceNone {
		return false
	}
	return result.Confidence >= *r.policy.MinConfidence
}

func (r *runtimeImpl) finishDecided(request Request, digest string, started time.Time, backend namedBackend, backendResult BackendResult, fallbackUsed bool) (Result, Receipt, error) {
	result := Result{
		Status:              StatusDecided,
		Value:               backendResult.Value,
		Candidates:          slices.Clone(backendResult.Candidates),
		Confidence:          backendResult.Confidence,
		ConfidenceSemantics: backendResult.ConfidenceSemantics,
		Backend:             backend.name,
		Model:               backendResult.Model,
		FallbackUsed:        fallbackUsed,
	}
	receipt := buildReceipt(request, digest, started, result)
	r.metrics.IncDecided(request.Purpose, backend.name)
	return result, receipt, nil
}

func (r *runtimeImpl) finishAbstained(request Request, digest string, started time.Time, backend namedBackend, model, reason string, fallbackUsed bool) (Result, Receipt, error) {
	result := Result{
		Status:              StatusAbstained,
		ConfidenceSemantics: ConfidenceNone,
		Backend:             backend.name,
		Model:               model,
		FallbackUsed:        fallbackUsed,
		ReasonCode:          reason,
	}
	receipt := buildReceipt(request, digest, started, result)
	r.metrics.IncAbstained(request.Purpose, backend.name, reason)
	return result, receipt, nil
}

func buildReceipt(request Request, digest string, started time.Time, result Result) Receipt {
	return Receipt{
		SchemaVersion: receiptSchemaVersion,
		Purpose:       request.Purpose,
		RequestDigest: digest,
		SpecID:        request.Spec.ID,
		SpecVersion:   request.Spec.Version,
		Backend:       result.Backend,
		Model:         result.Model,
		Status:        result.Status,
		ReasonCode:    result.ReasonCode,
		FallbackUsed:  result.FallbackUsed,
		DurationMS:    elapsedMilliseconds(started),
	}
}

func elapsedMilliseconds(started time.Time) uint64 {
	elapsed := time.Since(started)
	if elapsed <= 0 {
		return 0
	}
	return uint64(elapsed / time.Millisecond)
}

func normalizeBackendError(backend string, err error) error {
	if typed, ok := errors.AsType[*RuntimeError](err); ok {
		return &RuntimeError{Kind: typed.Kind, Backend: backend, Err: err}
	}
	return runtimeError(ErrorBackendFailure, backend, err)
}

func shouldFallback(err error) bool {
	typed, ok := errors.AsType[*RuntimeError](err)
	return ok && (typed.Kind == ErrorBackendFailure || typed.Kind == ErrorInvalidBackendOutput)
}

func isNilInterface(value any) bool {
	if value == nil {
		return true
	}
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return reflected.IsNil()
	default:
		return false
	}
}

func cloneRequest(request Request) Request {
	cloned := request
	cloned.Context = maps.Clone(request.Context)
	cloned.Spec.Options = slices.Clone(request.Spec.Options)
	if request.Spec.Range != nil {
		cloned.Spec.Range = new(*request.Spec.Range)
	}
	return cloned
}
