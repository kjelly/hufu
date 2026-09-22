package decisionrt_test

import (
	"context"
	"errors"
	"math"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/kjelly/hufu/internal/decisionrt"
)

type fakeBackend struct {
	name   string
	decide func(context.Context, decisionrt.Request) (decisionrt.BackendResult, error)
}

func (b fakeBackend) Name() string { return b.name }

func (b fakeBackend) Decide(ctx context.Context, request decisionrt.Request) (decisionrt.BackendResult, error) {
	return b.decide(ctx, request)
}

func TestRuntimePrimaryDecisionAndReceipt(t *testing.T) {
	backend := fixedBackend("primary", decidedChoice("small"))
	runtime := mustRuntime(t, decisionrt.RuntimeConfig{Primary: backend})
	request := validChoiceRequest()

	result, receipt, err := runtime.Decide(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != decisionrt.StatusDecided || result.Value.Choice != "small" || result.Backend != "primary" || result.FallbackUsed {
		t.Fatalf("result = %#v", result)
	}
	if receipt.SchemaVersion != 1 || receipt.Status != result.Status || receipt.Backend != result.Backend || receipt.RequestDigest == "" || receipt.FallbackUsed {
		t.Fatalf("receipt = %#v", receipt)
	}
}

func TestRuntimeFallbackMatrix(t *testing.T) {
	backendFailure := errors.New("provider failed")
	tests := map[string]fakeBackend{
		"technical error": {
			name: "primary",
			decide: func(context.Context, decisionrt.Request) (decisionrt.BackendResult, error) {
				return decisionrt.BackendResult{}, backendFailure
			},
		},
		"invalid output": fixedBackend("primary", decidedChoice("outside")),
		"abstained": fixedBackend("primary", decisionrt.BackendResult{
			Status: decisionrt.StatusAbstained, ConfidenceSemantics: decisionrt.ConfidenceNone,
		}),
		"policy rejected": fixedBackend("primary", decisionrt.BackendResult{
			Status: decisionrt.StatusDecided, Value: decisionrt.Value{Choice: "small"}, ConfidenceSemantics: decisionrt.ConfidenceNone,
		}),
	}
	minimum := 0.5
	for name, primary := range tests {
		t.Run(name, func(t *testing.T) {
			fallbackResult := decidedChoice("large")
			config := decisionrt.RuntimeConfig{
				Primary:  primary,
				Fallback: fixedBackend("fallback", fallbackResult),
			}
			if name == "policy rejected" {
				config.Policy.MinConfidence = &minimum
				fallbackResult.Confidence = 0.9
				fallbackResult.ConfidenceSemantics = decisionrt.ConfidenceRaw
				config.Fallback = fixedBackend("fallback", fallbackResult)
			}
			runtime := mustRuntime(t, config)
			result, receipt, err := runtime.Decide(t.Context(), validChoiceRequest())
			if err != nil {
				t.Fatal(err)
			}
			if !result.FallbackUsed || result.Backend != "fallback" || result.Value.Choice != "large" || !receipt.FallbackUsed {
				t.Fatalf("fallback result = %#v, receipt = %#v", result, receipt)
			}
		})
	}
}

func TestRuntimeFinalAbstention(t *testing.T) {
	runtime := mustRuntime(t, decisionrt.RuntimeConfig{Primary: fixedBackend("primary", decisionrt.BackendResult{
		Status: decisionrt.StatusAbstained, ConfidenceSemantics: decisionrt.ConfidenceNone, Model: "model",
	})})
	result, receipt, err := runtime.Decide(t.Context(), validChoiceRequest())
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != decisionrt.StatusAbstained || result.ReasonCode != "backend_abstained" || result.Backend != "primary" || result.Model != "model" {
		t.Fatalf("result = %#v", result)
	}
	if receipt.ReasonCode != result.ReasonCode || receipt.Model != result.Model {
		t.Fatalf("receipt = %#v", receipt)
	}
}

func TestRuntimeFallbackAbstains(t *testing.T) {
	runtime := mustRuntime(t, decisionrt.RuntimeConfig{
		Primary: fixedBackend("primary", decisionrt.BackendResult{
			Status: decisionrt.StatusAbstained, ConfidenceSemantics: decisionrt.ConfidenceNone,
		}),
		Fallback: fixedBackend("fallback", decisionrt.BackendResult{
			Status: decisionrt.StatusAbstained, ConfidenceSemantics: decisionrt.ConfidenceNone,
		}),
	})
	result, receipt, err := runtime.Decide(t.Context(), validChoiceRequest())
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != decisionrt.StatusAbstained || result.Backend != "fallback" || !result.FallbackUsed || result.ReasonCode != "backend_abstained" {
		t.Fatalf("result = %#v", result)
	}
	if receipt.Backend != result.Backend || !receipt.FallbackUsed {
		t.Fatalf("receipt = %#v", receipt)
	}
}

func TestRuntimeFallbackErrorReturnsNoPartialOutput(t *testing.T) {
	sentinel := errors.New("fallback failed")
	runtime := mustRuntime(t, decisionrt.RuntimeConfig{
		Primary: fixedBackend("primary", decisionrt.BackendResult{
			Status: decisionrt.StatusAbstained, ConfidenceSemantics: decisionrt.ConfidenceNone,
		}),
		Fallback: fakeBackend{name: "fallback", decide: func(context.Context, decisionrt.Request) (decisionrt.BackendResult, error) {
			return decisionrt.BackendResult{}, sentinel
		}},
	})
	result, receipt, err := runtime.Decide(t.Context(), validChoiceRequest())
	assertErrorKind(t, err, decisionrt.ErrorBackendFailure)
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want wrapped sentinel", err)
	}
	if !reflect.DeepEqual(result, decisionrt.Result{}) || receipt != (decisionrt.Receipt{}) {
		t.Fatalf("error returned partial output: %#v %#v", result, receipt)
	}
}

func TestRuntimeAcceptancePolicy(t *testing.T) {
	minimum := 0.8
	raw := decisionrt.BackendResult{
		Status:              decisionrt.StatusDecided,
		Value:               decisionrt.Value{Choice: "small"},
		Confidence:          0.7,
		ConfidenceSemantics: decisionrt.ConfidenceRaw,
	}
	runtime := mustRuntime(t, decisionrt.RuntimeConfig{Primary: fixedBackend("primary", raw), Policy: decisionrt.AcceptancePolicy{MinConfidence: &minimum}})
	result, _, err := runtime.Decide(t.Context(), validChoiceRequest())
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != decisionrt.StatusAbstained || result.ReasonCode != "low_confidence" {
		t.Fatalf("result = %#v", result)
	}

	calibrated := raw
	calibrated.ConfidenceSemantics = decisionrt.ConfidenceCalibrated
	calibrated.Confidence = 0.9
	runtime = mustRuntime(t, decisionrt.RuntimeConfig{
		Primary: fixedBackend("primary", calibrated),
		Policy:  decisionrt.AcceptancePolicy{RequireCalibratedConfidence: true},
	})
	result, _, err = runtime.Decide(t.Context(), validChoiceRequest())
	if err != nil || result.Status != decisionrt.StatusDecided {
		t.Fatalf("calibrated result = %#v, err = %v", result, err)
	}
}

func TestRuntimeRejectsInvalidCandidateDistribution(t *testing.T) {
	result := decidedChoice("small")
	result.Confidence = 0.7
	result.ConfidenceSemantics = decisionrt.ConfidenceRaw
	result.Candidates = []decisionrt.Candidate{
		{Value: "small", Probability: 0.7},
		{Value: "large", Probability: 0.2},
	}
	runtime := mustRuntime(t, decisionrt.RuntimeConfig{Primary: fixedBackend("primary", result)})
	got, receipt, err := runtime.Decide(t.Context(), validChoiceRequest())
	assertErrorKind(t, err, decisionrt.ErrorInvalidBackendOutput)
	if !reflect.DeepEqual(got, decisionrt.Result{}) || receipt != (decisionrt.Receipt{}) {
		t.Fatalf("error returned partial output: %#v %#v", got, receipt)
	}
}

func TestRuntimeRejectsInvalidBackendOutputs(t *testing.T) {
	trueValue := true
	tests := map[string]decisionrt.BackendResult{
		"abstained missing confidence semantics": {
			Status: decisionrt.StatusAbstained,
		},
		"abstained value": {
			Status: decisionrt.StatusAbstained, Value: decisionrt.Value{Choice: "small"}, ConfidenceSemantics: decisionrt.ConfidenceNone,
		},
		"wrong value kind": {
			Status: decisionrt.StatusDecided, Value: decisionrt.Value{Boolean: &trueValue}, ConfidenceSemantics: decisionrt.ConfidenceNone,
		},
		"multiple value fields": {
			Status: decisionrt.StatusDecided, Value: decisionrt.Value{Choice: "small", Boolean: &trueValue}, ConfidenceSemantics: decisionrt.ConfidenceNone,
		},
		"outside domain": {
			Status: decisionrt.StatusDecided, Value: decisionrt.Value{Choice: "outside"}, ConfidenceSemantics: decisionrt.ConfidenceNone,
		},
		"non-finite confidence": {
			Status: decisionrt.StatusDecided, Value: decisionrt.Value{Choice: "small"}, Confidence: math.NaN(), ConfidenceSemantics: decisionrt.ConfidenceRaw,
		},
		"infinite confidence": {
			Status: decisionrt.StatusDecided, Value: decisionrt.Value{Choice: "small"}, Confidence: math.Inf(1), ConfidenceSemantics: decisionrt.ConfidenceRaw,
		},
		"probability outside range": {
			Status: decisionrt.StatusDecided, Value: decisionrt.Value{Choice: "small"}, ConfidenceSemantics: decisionrt.ConfidenceNone,
			Candidates: []decisionrt.Candidate{{Value: "small", Probability: 1.1}, {Value: "large", Probability: -0.1}},
		},
		"incomplete distribution": {
			Status: decisionrt.StatusDecided, Value: decisionrt.Value{Choice: "small"}, ConfidenceSemantics: decisionrt.ConfidenceNone,
			Candidates: []decisionrt.Candidate{{Value: "small", Probability: 1}},
		},
		"distribution outside domain": {
			Status: decisionrt.StatusDecided, Value: decisionrt.Value{Choice: "small"}, ConfidenceSemantics: decisionrt.ConfidenceNone,
			Candidates: []decisionrt.Candidate{{Value: "small", Probability: 0.5}, {Value: "outside", Probability: 0.5}},
		},
		"duplicate candidate": {
			Status: decisionrt.StatusDecided, Value: decisionrt.Value{Choice: "small"}, ConfidenceSemantics: decisionrt.ConfidenceNone,
			Candidates: []decisionrt.Candidate{{Value: "small", Probability: 0.5}, {Value: "small", Probability: 0.5}},
		},
		"confidence mismatch": {
			Status: decisionrt.StatusDecided, Value: decisionrt.Value{Choice: "small"}, Confidence: 0.8, ConfidenceSemantics: decisionrt.ConfidenceRaw,
			Candidates: []decisionrt.Candidate{{Value: "small", Probability: 0.7}, {Value: "large", Probability: 0.3}},
		},
	}
	for name, backendResult := range tests {
		t.Run(name, func(t *testing.T) {
			runtime := mustRuntime(t, decisionrt.RuntimeConfig{Primary: fixedBackend("primary", backendResult)})
			_, _, err := runtime.Decide(t.Context(), validChoiceRequest())
			assertErrorKind(t, err, decisionrt.ErrorInvalidBackendOutput)
		})
	}
}

func TestRuntimeRejectsIntegerOutsideRange(t *testing.T) {
	runtime := mustRuntime(t, decisionrt.RuntimeConfig{Primary: fixedBackend("primary", decisionrt.BackendResult{
		Status:              decisionrt.StatusDecided,
		Value:               decisionrt.Value{Integer: new(int64(2))},
		ConfidenceSemantics: decisionrt.ConfidenceNone,
	})})
	request := decisionrt.Request{
		Purpose: "score@v1",
		Spec: decisionrt.Spec{
			ID: "score", Version: "v1", Kind: decisionrt.KindIntegerRange, Question: "Score?",
			Range: &decisionrt.IntegerRange{Min: -1, Max: 1},
		},
	}

	result, receipt, err := runtime.Decide(t.Context(), request)
	assertErrorKind(t, err, decisionrt.ErrorInvalidBackendOutput)
	if !reflect.DeepEqual(result, decisionrt.Result{}) || receipt != (decisionrt.Receipt{}) {
		t.Fatalf("error returned partial output: %#v %#v", result, receipt)
	}
}

func TestRuntimeConfigurationValidation(t *testing.T) {
	valid := fixedBackend("primary", decidedChoice("small"))
	minimum := 2.0
	tests := []decisionrt.RuntimeConfig{
		{},
		{Primary: fixedBackend("Not Valid", decidedChoice("small"))},
		{Primary: valid, Fallback: fixedBackend("primary", decidedChoice("large"))},
		{Primary: valid, Timeout: -time.Second},
		{Primary: valid, Timeout: 11 * time.Second},
		{Primary: valid, Policy: decisionrt.AcceptancePolicy{MinConfidence: &minimum}},
	}
	for _, config := range tests {
		_, err := decisionrt.NewRuntime(config)
		assertErrorKind(t, err, decisionrt.ErrorConfiguration)
	}
}

func TestRuntimeCallerCancellationDoesNotFallback(t *testing.T) {
	var primaryCalls, fallbackCalls int
	primary := fakeBackend{name: "primary", decide: func(ctx context.Context, _ decisionrt.Request) (decisionrt.BackendResult, error) {
		primaryCalls++
		<-ctx.Done()
		return decisionrt.BackendResult{}, ctx.Err()
	}}
	fallback := fakeBackend{name: "fallback", decide: func(context.Context, decisionrt.Request) (decisionrt.BackendResult, error) {
		fallbackCalls++
		return decidedChoice("large"), nil
	}}
	runtime := mustRuntime(t, decisionrt.RuntimeConfig{Primary: primary, Fallback: fallback, Timeout: time.Second})
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, _, err := runtime.Decide(ctx, validChoiceRequest())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context canceled", err)
	}
	if fallbackCalls != 0 {
		t.Fatalf("fallback calls = %d", fallbackCalls)
	}
	if primaryCalls != 0 {
		t.Fatalf("primary calls = %d", primaryCalls)
	}
}

func TestRuntimeCancellationAfterPrimaryDoesNotInvokeFallback(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	var fallbackCalls int
	primary := fakeBackend{name: "primary", decide: func(context.Context, decisionrt.Request) (decisionrt.BackendResult, error) {
		cancel()
		return decisionrt.BackendResult{
			Status: decisionrt.StatusAbstained, ConfidenceSemantics: decisionrt.ConfidenceNone,
		}, nil
	}}
	fallback := fakeBackend{name: "fallback", decide: func(context.Context, decisionrt.Request) (decisionrt.BackendResult, error) {
		fallbackCalls++
		return decidedChoice("large"), nil
	}}
	runtime := mustRuntime(t, decisionrt.RuntimeConfig{Primary: primary, Fallback: fallback})
	result, receipt, err := runtime.Decide(ctx, validChoiceRequest())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context canceled", err)
	}
	if fallbackCalls != 0 {
		t.Fatalf("fallback calls = %d", fallbackCalls)
	}
	if !reflect.DeepEqual(result, decisionrt.Result{}) || receipt != (decisionrt.Receipt{}) {
		t.Fatalf("error returned partial output: %#v %#v", result, receipt)
	}
}

func TestRuntimeCancellationFromFallbackMetricDoesNotInvokeFallback(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	var fallbackCalls int
	metrics := &cancelOnFallbackMetrics{cancel: cancel}
	runtime := mustRuntime(t, decisionrt.RuntimeConfig{
		Primary: fixedBackend("primary", decisionrt.BackendResult{
			Status: decisionrt.StatusAbstained, ConfidenceSemantics: decisionrt.ConfidenceNone,
		}),
		Fallback: fakeBackend{name: "fallback", decide: func(context.Context, decisionrt.Request) (decisionrt.BackendResult, error) {
			fallbackCalls++
			return decidedChoice("large"), nil
		}},
		Metrics: metrics,
	})

	result, receipt, err := runtime.Decide(ctx, validChoiceRequest())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context canceled", err)
	}
	if fallbackCalls != 0 {
		t.Fatalf("fallback calls = %d", fallbackCalls)
	}
	if !reflect.DeepEqual(result, decisionrt.Result{}) || receipt != (decisionrt.Receipt{}) {
		t.Fatalf("error returned partial output: %#v %#v", result, receipt)
	}
}

func TestRuntimeConfigurationAndUnavailableErrorsDoNotFallback(t *testing.T) {
	for _, kind := range []decisionrt.ErrorKind{decisionrt.ErrorConfiguration, decisionrt.ErrorBackendUnavailable} {
		t.Run(string(kind), func(t *testing.T) {
			var fallbackCalls int
			primaryError := &decisionrt.RuntimeError{Kind: kind, Err: errors.New("cause")}
			runtime := mustRuntime(t, decisionrt.RuntimeConfig{
				Primary: fakeBackend{name: "primary", decide: func(context.Context, decisionrt.Request) (decisionrt.BackendResult, error) {
					return decisionrt.BackendResult{}, primaryError
				}},
				Fallback: fakeBackend{name: "fallback", decide: func(context.Context, decisionrt.Request) (decisionrt.BackendResult, error) {
					fallbackCalls++
					return decidedChoice("large"), nil
				}},
			})
			result, receipt, err := runtime.Decide(t.Context(), validChoiceRequest())
			assertErrorKind(t, err, kind)
			if !errors.Is(err, primaryError) {
				t.Fatalf("err = %v, want wrapped backend error", err)
			}
			if fallbackCalls != 0 {
				t.Fatalf("fallback calls = %d", fallbackCalls)
			}
			if !reflect.DeepEqual(result, decisionrt.Result{}) || receipt != (decisionrt.Receipt{}) {
				t.Fatalf("error returned partial output: %#v %#v", result, receipt)
			}
		})
	}
}

func TestRuntimeAttemptTimeoutFallsBack(t *testing.T) {
	primary := fakeBackend{name: "primary", decide: func(ctx context.Context, _ decisionrt.Request) (decisionrt.BackendResult, error) {
		<-ctx.Done()
		return decisionrt.BackendResult{}, ctx.Err()
	}}
	runtime := mustRuntime(t, decisionrt.RuntimeConfig{
		Primary: primary, Fallback: fixedBackend("fallback", decidedChoice("large")), Timeout: time.Millisecond,
	})
	result, _, err := runtime.Decide(t.Context(), validChoiceRequest())
	if err != nil || result.Backend != "fallback" {
		t.Fatalf("result = %#v, err = %v", result, err)
	}
}

func TestRuntimeMetrics(t *testing.T) {
	metrics := &recordingMetrics{}
	runtime := mustRuntime(t, decisionrt.RuntimeConfig{
		Primary:  fixedBackend("primary", decisionrt.BackendResult{Status: decisionrt.StatusAbstained, ConfidenceSemantics: decisionrt.ConfidenceNone}),
		Fallback: fixedBackend("fallback", decidedChoice("large")),
		Metrics:  metrics,
	})
	if _, _, err := runtime.Decide(t.Context(), validChoiceRequest()); err != nil {
		t.Fatal(err)
	}
	metrics.mu.Lock()
	defer metrics.mu.Unlock()
	if metrics.calls != 2 || metrics.fallbacks != 1 || metrics.decided != 1 || metrics.abstained != 0 || metrics.durations != 2 {
		t.Fatalf("metrics = %#v", metrics)
	}
}

func TestRuntimeMetricsRecordErrorsAndFinalAbstention(t *testing.T) {
	metrics := &recordingMetrics{}
	runtime := mustRuntime(t, decisionrt.RuntimeConfig{
		Primary: fakeBackend{name: "primary", decide: func(context.Context, decisionrt.Request) (decisionrt.BackendResult, error) {
			return decisionrt.BackendResult{}, errors.New("provider failed")
		}},
		Fallback: fixedBackend("fallback", decisionrt.BackendResult{
			Status: decisionrt.StatusAbstained, ConfidenceSemantics: decisionrt.ConfidenceNone,
		}),
		Metrics: metrics,
	})
	if _, _, err := runtime.Decide(t.Context(), validChoiceRequest()); err != nil {
		t.Fatal(err)
	}
	metrics.mu.Lock()
	defer metrics.mu.Unlock()
	if metrics.calls != 2 || metrics.errors != 1 || metrics.fallbacks != 1 || metrics.decided != 0 || metrics.abstained != 1 || metrics.durations != 2 {
		t.Fatalf("metrics = %#v", metrics)
	}
}

func TestRuntimeNoopMetricsMatchesExplicitMetrics(t *testing.T) {
	request := validChoiceRequest()
	backend := fixedBackend("primary", decidedChoice("small"))
	withoutMetrics := mustRuntime(t, decisionrt.RuntimeConfig{Primary: backend})
	withMetrics := mustRuntime(t, decisionrt.RuntimeConfig{Primary: backend, Metrics: &recordingMetrics{}})

	wantResult, wantReceipt, err := withoutMetrics.Decide(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	gotResult, gotReceipt, err := withMetrics.Decide(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	wantReceipt.DurationMS = 0
	gotReceipt.DurationMS = 0
	if !reflect.DeepEqual(gotResult, wantResult) || gotReceipt != wantReceipt {
		t.Fatalf("with metrics = %#v %#v, without metrics = %#v %#v", gotResult, gotReceipt, wantResult, wantReceipt)
	}
}

func TestRuntimeConcurrentUse(t *testing.T) {
	runtime := mustRuntime(t, decisionrt.RuntimeConfig{
		Primary: fixedBackend("primary", decidedChoice("small")),
		Metrics: &recordingMetrics{},
	})
	errorsCh := make(chan error, 32)
	ctx := t.Context()
	var wait sync.WaitGroup
	for range 32 {
		wait.Go(func() {
			result, _, err := runtime.Decide(ctx, validChoiceRequest())
			if err == nil && result.Value.Choice != "small" {
				err = errors.New("unexpected decision")
			}
			errorsCh <- err
		})
	}
	wait.Wait()
	close(errorsCh)
	for err := range errorsCh {
		if err != nil {
			t.Fatal(err)
		}
	}
}

func fixedBackend(name string, result decisionrt.BackendResult) fakeBackend {
	return fakeBackend{name: name, decide: func(context.Context, decisionrt.Request) (decisionrt.BackendResult, error) {
		return result, nil
	}}
}

func decidedChoice(choice string) decisionrt.BackendResult {
	return decisionrt.BackendResult{
		Status:              decisionrt.StatusDecided,
		Value:               decisionrt.Value{Choice: choice},
		ConfidenceSemantics: decisionrt.ConfidenceNone,
		Model:               "model",
	}
}

func mustRuntime(t *testing.T, config decisionrt.RuntimeConfig) decisionrt.Runtime {
	t.Helper()
	runtime, err := decisionrt.NewRuntime(config)
	if err != nil {
		t.Fatal(err)
	}
	return runtime
}

type recordingMetrics struct {
	mu        sync.Mutex
	calls     int
	decided   int
	abstained int
	errors    int
	fallbacks int
	durations int
}

type cancelOnFallbackMetrics struct {
	recordingMetrics
	cancel context.CancelFunc
}

func (m *cancelOnFallbackMetrics) IncFallback(purpose, backend string) {
	m.cancel()
	m.recordingMetrics.IncFallback(purpose, backend)
}

func (m *recordingMetrics) IncCalls(string, string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls++
}

func (m *recordingMetrics) IncDecided(string, string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.decided++
}

func (m *recordingMetrics) IncAbstained(string, string, string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.abstained++
}

func (m *recordingMetrics) IncErrors(string, string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.errors++
}

func (m *recordingMetrics) IncFallback(string, string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.fallbacks++
}

func (m *recordingMetrics) ObserveDurationMS(string, string, uint64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.durations++
}
