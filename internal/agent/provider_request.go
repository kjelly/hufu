package agent

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"sync"
	"time"

	"charm.land/fantasy"
	"charm.land/fantasy/providers/openaicompat"
	"charm.land/fantasy/schema"
)

// ProviderRequest is the provider-equivalent request shape used by Hufu's
// admission layer. Messages already include the system message and every
// multimodal/tool part that Fantasy will send for the step.
type ProviderRequest struct {
	ModelID      string
	InvocationID string
	Call         *fantasy.Call
	ObjectCall   *fantasy.ObjectCall
	Messages     []fantasy.Message
	Tools        []fantasy.AgentTool
	// AdmissionContext is copied into each provider request by the admitted
	// language-model wrapper. A zero context preserves legacy direct
	// constructors, which use the compatibility registry in the consumer.
	AdmissionContext ProviderAdmissionContext
}

// ProviderAdmissionContext is the immutable, provider-bound capacity used by
// pre-provider admission. It contains no credential material.
type ProviderAdmissionContext struct {
	ModelID          string
	ProviderIdentity string
	ProviderBaseURL  string
	// Bound distinguishes a provider-specific context from the legacy
	// registry-compatible zero value. Provider identity fields also imply a
	// bound context for compatibility with older callers.
	Bound               bool
	ContextWindow       int
	MaxOutputTokens     int
	SafetyMarginTokens  int
	Estimator           string
	ContextWindowSource string
	IsEstimated         bool
	// ProfileTelemetryJSON is a secret-free, immutable profile projection
	// carried opaquely to the team-owned durable commit boundary.
	ProfileTelemetryJSON string
}

// IsBound reports whether this context is tied to one provider invocation.
// A bound context must not be replaced with global model metadata when its
// capacity is unavailable.
func (c ProviderAdmissionContext) IsBound() bool {
	return c.Bound || c.ProviderIdentity != "" || c.ProviderBaseURL != ""
}

// SerializeProviderRequest returns the compact OpenAI-compatible request body
// used for admission. Fantasy's exported prompt conversion is the source of
// truth for message and multimodal wire representation.
func SerializeProviderRequest(request ProviderRequest) ([]byte, error) {
	call, err := requestCall(request)
	if err != nil {
		return nil, err
	}
	messages, _ := openaicompat.ToPromptFunc(call.Prompt, "", "")
	wire := map[string]any{"model": request.ModelID, "messages": messages}
	if tools := serializeProviderTools(call.Tools); len(tools) > 0 {
		wire["tools"] = tools
	}
	if call.ToolChoice != nil {
		wire["tool_choice"] = serializeToolChoice(*call.ToolChoice)
	}
	return json.Marshal(wire)
}

func requestCall(request ProviderRequest) (*fantasy.Call, error) {
	if request.Call != nil && request.ObjectCall != nil {
		return nil, fmt.Errorf("provider request has multiple calls")
	}
	call := request.Call
	if request.ObjectCall != nil {
		objectCall := request.ObjectCall
		call = &fantasy.Call{
			Prompt:          objectCall.Prompt,
			MaxOutputTokens: objectCall.MaxOutputTokens,
			Tools: []fantasy.Tool{fantasy.FunctionTool{
				Name:        objectCall.SchemaName,
				Description: objectCall.SchemaDescription,
				InputSchema: schema.ToMap(objectCall.Schema),
			}},
		}
	}
	if call == nil {
		call = &fantasy.Call{Prompt: request.Messages}
	}
	if len(request.Tools) > 0 {
		for _, tool := range request.Tools {
			if tool == nil {
				continue
			}
			info := tool.Info()
			inputSchema := map[string]any{"type": "object", "properties": info.Parameters, "required": info.Required}
			inputSchema = cloneProviderSchemaValue(inputSchema).(map[string]any)
			schema.Normalize(inputSchema)
			call.Tools = append(call.Tools, fantasy.FunctionTool{
				Name:        info.Name,
				Description: info.Description,
				InputSchema: inputSchema,
			})
		}
	}
	return call, nil
}

type providerWireTool struct {
	Type     string               `json:"type"`
	Function providerWireFunction `json:"function"`
}

type providerWireFunction struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
	Strict      bool           `json:"strict"`
}

func serializeProviderTools(tools []fantasy.Tool) []providerWireTool {
	serialized := make([]providerWireTool, 0, len(tools))
	for _, tool := range tools {
		function, ok := tool.(fantasy.FunctionTool)
		if !ok || function.GetType() != fantasy.ToolTypeFunction {
			continue
		}
		serialized = append(serialized, providerWireTool{
			Type: "function",
			Function: providerWireFunction{
				Name: function.Name, Description: function.Description,
				Parameters: function.InputSchema, Strict: false,
			},
		})
	}
	return serialized
}

func serializeToolChoice(choice fantasy.ToolChoice) any {
	switch choice {
	case fantasy.ToolChoiceAuto, fantasy.ToolChoiceNone, fantasy.ToolChoiceRequired:
		return string(choice)
	default:
		return map[string]any{"type": "function", "function": map[string]string{"name": string(choice)}}
	}
}

// CountProviderRequestTokens is retained as a small compatibility helper for
// callers that need a provider-independent conservative estimate.
func CountProviderRequestTokens(request ProviderRequest) (int, error) {
	data, err := SerializeProviderRequest(request)
	if err != nil {
		return 0, fmt.Errorf("serialize provider request: %w", err)
	}
	return (len(data) + 3) / 4, nil
}

type admittedLanguageModel struct {
	modelID             string
	inner               fantasy.LanguageModel
	admission           RequestAdmission
	invocationLimiter   InvocationLimiter
	admissionContext    ProviderAdmissionContext
	invocationCommitter ProviderInvocationCommitter
	invocationSettler   ProviderInvocationSettler
}

// RequestAdmission is the shared pre-provider admission contract.
type RequestAdmission interface {
	AdmitProviderRequest(context.Context, ProviderRequest) error
}

// ProviderInvocationCommitter is the optional durable commit boundary that
// runs after admission and provider-slot acquisition, but before transport.
// Implementations must fail closed: a commit error prevents the provider call.
type ProviderInvocationCommitter interface {
	CommitProviderInvocation(context.Context, ProviderRequest) error
}

// ProviderInvocationOutcome is a bounded, provider-neutral terminal state.
// It intentionally carries no raw provider error or response content.
type ProviderInvocationOutcome string

const (
	ProviderInvocationSuccess         ProviderInvocationOutcome = "success"
	ProviderInvocationProviderError   ProviderInvocationOutcome = "provider_error"
	ProviderInvocationCancelled       ProviderInvocationOutcome = "cancelled"
	ProviderInvocationStreamAbandoned ProviderInvocationOutcome = "stream_abandoned"
)

// ProviderInvocationUsage is the provider-neutral usage needed to settle a
// committed invocation. A nil usage in ProviderInvocationResult means the
// provider did not report complete enough data for an observed-cost charge.
type ProviderInvocationUsage struct {
	InputTokens         int64
	CacheReadTokens     int64
	CacheCreationTokens int64
	OutputTokens        int64
	TotalTokens         int64
}

// ProviderInvocationResult is emitted exactly once after a committed
// provider invocation reaches a terminal state. ResponseObserved records
// whether any response object or stream part crossed the transport boundary.
type ProviderInvocationResult struct {
	Usage *ProviderInvocationUsage
	// UsageReported preserves explicit presence from a trusted transport,
	// including all-zero usage. SDK zero-value structs alone do not set it.
	UsageReported    bool
	Outcome          ProviderInvocationOutcome
	ResponseObserved bool
}

// ProviderInvocationSettler is the optional terminal accounting boundary.
// Settlement is observational: implementations must latch their own failures
// and must not replace or retry the provider result already produced.
type ProviderInvocationSettler interface {
	SettleProviderInvocation(context.Context, ProviderRequest, ProviderInvocationResult)
}

// InvocationLimiter is an optional coordinator-owned boundary around the
// actual provider call. The model ID is supplied by the wrapper that is about
// to invoke the provider, so retries and model continuations use their final
// selected model rather than an earlier dispatch model.
type InvocationLimiter interface {
	AcquireProviderInvocation(context.Context, string) (release func(), err error)
}

func NewAdmittedLanguageModel(modelID string, inner fantasy.LanguageModel, admission RequestAdmission) fantasy.LanguageModel {
	return NewAdmittedLanguageModelWithContext(modelID, inner, admission, ProviderAdmissionContext{})
}

// NewAdmittedLanguageModelWithContext binds one copied admission context to
// all four Fantasy language-model entry points. The old constructor remains
// compatible for tests and integrations that intentionally use legacy lookup.
func NewAdmittedLanguageModelWithContext(modelID string, inner fantasy.LanguageModel, admission RequestAdmission, context ProviderAdmissionContext) fantasy.LanguageModel {
	if inner == nil || admission == nil {
		return inner
	}
	if context.ModelID == "" {
		context.ModelID = modelID
	}
	limiter, _ := admission.(InvocationLimiter)
	committer, _ := admission.(ProviderInvocationCommitter)
	settler, _ := admission.(ProviderInvocationSettler)
	return &admittedLanguageModel{
		modelID: modelID, inner: inner, admission: admission,
		invocationLimiter: limiter, admissionContext: context,
		invocationCommitter: committer, invocationSettler: settler,
	}
}

func (m *admittedLanguageModel) acquireInvocation(ctx context.Context) (func(), error) {
	if m.invocationLimiter == nil {
		return nil, nil
	}
	return m.invocationLimiter.AcquireProviderInvocation(ctx, m.modelID)
}

type providerStreamState struct {
	mu               sync.Mutex
	usage            *ProviderInvocationUsage
	responseObserved bool
	providerError    bool
	abandoned        bool
}

func (s *providerStreamState) observe(usage *ProviderInvocationUsage, providerError bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.responseObserved = true
	if usage != nil {
		s.usage = usage
	}
	s.providerError = s.providerError || providerError
}

func (s *providerStreamState) abandon() {
	s.mu.Lock()
	s.abandoned = true
	s.mu.Unlock()
}

func (s *providerStreamState) result(ctx context.Context) ProviderInvocationResult {
	s.mu.Lock()
	defer s.mu.Unlock()
	outcome := ProviderInvocationSuccess
	switch {
	case ctx.Err() != nil:
		outcome = ProviderInvocationCancelled
	case s.abandoned:
		outcome = ProviderInvocationStreamAbandoned
	case s.providerError:
		outcome = ProviderInvocationProviderError
	}
	return ProviderInvocationResult{Usage: s.usage, Outcome: outcome, ResponseObserved: s.responseObserved}
}

func admittedStream[T any](ctx context.Context, stream iter.Seq[T], state *providerStreamState, cancel context.CancelFunc, stopCancellation func() bool, finalize func(ProviderInvocationResult), observe func(T) (*ProviderInvocationUsage, bool)) iter.Seq[T] {
	return func(yield func(T) bool) {
		defer func() {
			stopCancellation()
			finalize(state.result(ctx))
		}()
		stream(func(part T) bool {
			state.observe(observe(part))
			keepGoing := yield(part)
			if !keepGoing {
				state.abandon()
				cancel()
			}
			return keepGoing
		})
	}
}

func (m *admittedLanguageModel) streamContext(ctx context.Context, request ProviderRequest, state *providerStreamState, release func()) (context.Context, context.CancelFunc, func(ProviderInvocationResult), func() bool) {
	ctx = withProviderRejectionObservation(ctx)
	ctx = withProviderTransportIdentity(ctx, request)
	ctx = withProviderUsageObservation(ctx)
	streamCtx, cancel := context.WithCancel(ctx)
	var once sync.Once
	finalize := func(result ProviderInvocationResult) {
		once.Do(func() {
			cancel()
			m.settleInvocation(ctx, request, result)
			if release != nil {
				release()
			}
		})
	}
	stopCancellation := context.AfterFunc(ctx, func() {
		finalize(state.result(ctx))
	})
	return streamCtx, cancel, finalize, stopCancellation
}

func (m *admittedLanguageModel) request(request ProviderRequest) ProviderRequest {
	request.ModelID = m.modelID
	request.AdmissionContext = m.admissionContext
	return request
}

// NewProviderInvocationID returns the shared durable identity used by both
// Fantasy provider calls and external execution backends.
func NewProviderInvocationID() (string, error) {
	bytes := make([]byte, 16)
	if _, err := rand.Read(bytes); err != nil {
		return "", fmt.Errorf("generate provider invocation ID: %w", err)
	}
	return fmt.Sprintf("pinv-%s-%x", time.Now().UTC().Format("20060102150405.000000000"), bytes), nil
}

func (m *admittedLanguageModel) commitInvocation(ctx context.Context, request *ProviderRequest) error {
	if m.invocationCommitter == nil {
		return nil
	}
	id, err := NewProviderInvocationID()
	if err != nil {
		return err
	}
	request.InvocationID = id
	return m.invocationCommitter.CommitProviderInvocation(ctx, *request)
}

func (m *admittedLanguageModel) settleInvocation(ctx context.Context, request ProviderRequest, result ProviderInvocationResult) {
	if m.invocationSettler != nil {
		m.invocationSettler.SettleProviderInvocation(ctx, request, observedProviderUsage(ctx, result))
	}
}

func providerInvocationOutcome(ctx context.Context, err error) ProviderInvocationOutcome {
	if err == nil {
		return ProviderInvocationSuccess
	}
	if ctx.Err() != nil || errors.Is(err, context.Canceled) {
		return ProviderInvocationCancelled
	}
	return ProviderInvocationProviderError
}

func providerInvocationUsage(usage fantasy.Usage) *ProviderInvocationUsage {
	return &ProviderInvocationUsage{
		InputTokens: usage.InputTokens, CacheReadTokens: usage.CacheReadTokens,
		CacheCreationTokens: usage.CacheCreationTokens, OutputTokens: usage.OutputTokens,
		TotalTokens: usage.TotalTokens,
	}
}

func reportedProviderInvocationUsage(usage fantasy.Usage) *ProviderInvocationUsage {
	if usage.InputTokens == 0 && usage.CacheReadTokens == 0 && usage.CacheCreationTokens == 0 && usage.OutputTokens == 0 && usage.TotalTokens == 0 {
		return nil
	}
	return providerInvocationUsage(usage)
}

func (m *admittedLanguageModel) generateOnce(ctx context.Context, call fantasy.Call) (*fantasy.Response, error) {
	request := m.request(ProviderRequest{Call: &call})
	release, err := m.acquireInvocation(ctx)
	if err != nil {
		return nil, err
	}
	defer func() {
		if release != nil {
			release()
		}
	}()
	if err := m.commitInvocation(ctx, &request); err != nil {
		return nil, err
	}
	ctx = withProviderTransportIdentity(ctx, request)
	ctx = withProviderUsageObservation(ctx)
	response, providerErr := m.inner.Generate(ctx, call)
	result := ProviderInvocationResult{Outcome: providerInvocationOutcome(ctx, providerErr), ResponseObserved: response != nil}
	if response != nil {
		result.Usage = providerInvocationUsage(response.Usage)
	}
	m.settleInvocation(ctx, request, result)
	return response, wrapProviderRejection(providerErr, response != nil)
}

func (m *admittedLanguageModel) streamOnce(ctx context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
	request := m.request(ProviderRequest{Call: &call})
	release, err := m.acquireInvocation(ctx)
	if err != nil {
		return nil, err
	}
	if err := m.commitInvocation(ctx, &request); err != nil {
		if release != nil {
			release()
		}
		return nil, err
	}
	state := &providerStreamState{}
	streamCtx, cancel, finalize, stopCancellation := m.streamContext(ctx, request, state, release)
	// Keep the registered .Stream(ctx, chokepoint marker while passing the
	// derived context required to own the stream's transport lifetime.
	stream, err := m.inner.Stream(streamCtx, call)
	if err == nil {
		if rejection := m.streamRejection(streamCtx, request); rejection != nil {
			err = rejection
			// Drain the SDK's rejected HTTP result to preserve its typed error
			// and close its response. No model response is exposed or replayed.
			if stream != nil {
				stream(func(part fantasy.StreamPart) bool {
					if part.Error != nil {
						err = part.Error
						return false
					}
					return true
				})
			}
		}
	}
	if err != nil {
		stopCancellation()
		finalize(ProviderInvocationResult{Outcome: providerInvocationOutcome(ctx, err)})
		return nil, wrapProviderRejection(err, false)
	}
	return admittedStream(ctx, stream, state, cancel, stopCancellation, finalize, func(part fantasy.StreamPart) (*ProviderInvocationUsage, bool) {
		return reportedProviderInvocationUsage(part.Usage), part.Error != nil || part.Type == fantasy.StreamPartTypeError
	}), nil
}

func (m *admittedLanguageModel) generateObjectOnce(ctx context.Context, call fantasy.ObjectCall) (*fantasy.ObjectResponse, error) {
	request := m.request(ProviderRequest{ObjectCall: &call})
	release, err := m.acquireInvocation(ctx)
	if err != nil {
		return nil, err
	}
	defer func() {
		if release != nil {
			release()
		}
	}()
	if err := m.commitInvocation(ctx, &request); err != nil {
		return nil, err
	}
	ctx = withProviderTransportIdentity(ctx, request)
	ctx = withProviderUsageObservation(ctx)
	response, providerErr := m.inner.GenerateObject(ctx, call)
	result := ProviderInvocationResult{Outcome: providerInvocationOutcome(ctx, providerErr), ResponseObserved: response != nil}
	if response != nil {
		result.Usage = providerInvocationUsage(response.Usage)
	}
	m.settleInvocation(ctx, request, result)
	return response, wrapProviderRejection(providerErr, response != nil)
}

func (m *admittedLanguageModel) streamObjectOnce(ctx context.Context, call fantasy.ObjectCall) (fantasy.ObjectStreamResponse, error) {
	request := m.request(ProviderRequest{ObjectCall: &call})
	release, err := m.acquireInvocation(ctx)
	if err != nil {
		return nil, err
	}
	if err := m.commitInvocation(ctx, &request); err != nil {
		if release != nil {
			release()
		}
		return nil, err
	}
	state := &providerStreamState{}
	streamCtx, cancel, finalize, stopCancellation := m.streamContext(ctx, request, state, release)
	stream, err := m.inner.StreamObject(streamCtx, call)
	if err == nil {
		if rejection := m.streamRejection(streamCtx, request); rejection != nil {
			err = rejection
			if stream != nil {
				stream(func(part fantasy.ObjectStreamPart) bool {
					if part.Error != nil {
						err = part.Error
						return false
					}
					return true
				})
			}
		}
	}
	if err != nil {
		stopCancellation()
		finalize(ProviderInvocationResult{Outcome: providerInvocationOutcome(ctx, err)})
		return nil, wrapProviderRejection(err, false)
	}
	return admittedStream(ctx, stream, state, cancel, stopCancellation, finalize, func(part fantasy.ObjectStreamPart) (*ProviderInvocationUsage, bool) {
		return reportedProviderInvocationUsage(part.Usage), part.Error != nil || part.Type == fantasy.ObjectStreamPartTypeError
	}), nil
}

func (m *admittedLanguageModel) Provider() string { return m.inner.Provider() }

func (m *admittedLanguageModel) Model() string { return m.inner.Model() }

// recover retries only a rejected provider request. The single-call methods
// settle and release every invocation before this policy can wait or resend.
func (m *admittedLanguageModel) recover[T any](ctx context.Context, request ProviderRequest, once func() (T, bool, error)) (T, error) {
	recovery, _ := m.admission.(ProviderRequestRecovery)
	for {
		var zero T
		if err := ctx.Err(); err != nil {
			return zero, err
		}
		if err := m.admission.AdmitProviderRequest(ctx, request); err != nil {
			return zero, err
		}
		var finish func(bool) error
		if recovery != nil {
			var err error
			finish, err = recovery.WaitProviderRequest(ctx, request)
			if err != nil {
				return zero, err
			}
		}
		var response T
		var observed bool
		var err, finishErr error
		func() {
			completed := false
			defer func() {
				if finish != nil {
					finishErr = finish(completed && (err == nil || observed))
				}
			}()
			response, observed, err = once()
			completed = true
		}()
		if finishErr != nil {
			return zero, finishErr
		}

		if err == nil || observed {
			return response, err
		}
		if recovery == nil {
			if rejection, ok := errors.AsType[*providerRejection](err); ok {
				err = rejection.cause
			}
			return response, err
		}
		rejection, rejected := errors.AsType[*providerRejection](err)
		if !rejected {
			return response, err
		}
		retry, recoveryErr := recovery.RecoverProviderRequest(ctx, request, rejection.cause)
		if recoveryErr != nil {
			return zero, recoveryErr
		}
		if !retry {
			return response, rejection.cause
		}
	}
}

func (m *admittedLanguageModel) Generate(ctx context.Context, call fantasy.Call) (*fantasy.Response, error) {
	call.Prompt = sanitizeToolCallHistory(call.Prompt)
	return m.recover(ctx, m.request(ProviderRequest{Call: &call}), func() (*fantasy.Response, bool, error) {
		response, err := m.generateOnce(ctx, call)
		return response, response != nil, err
	})
}
func (m *admittedLanguageModel) Stream(ctx context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
	call.Prompt = sanitizeToolCallHistory(call.Prompt)
	return m.recover(ctx, m.request(ProviderRequest{Call: &call}), func() (fantasy.StreamResponse, bool, error) {
		response, err := m.streamOnce(ctx, call)
		return response, response != nil, err
	})
}
func (m *admittedLanguageModel) GenerateObject(ctx context.Context, call fantasy.ObjectCall) (*fantasy.ObjectResponse, error) {
	call.Prompt = sanitizeToolCallHistory(call.Prompt)
	return m.recover(ctx, m.request(ProviderRequest{ObjectCall: &call}), func() (*fantasy.ObjectResponse, bool, error) {
		response, err := m.generateObjectOnce(ctx, call)
		return response, response != nil, err
	})
}
func (m *admittedLanguageModel) StreamObject(ctx context.Context, call fantasy.ObjectCall) (fantasy.ObjectStreamResponse, error) {
	call.Prompt = sanitizeToolCallHistory(call.Prompt)
	return m.recover(ctx, m.request(ProviderRequest{ObjectCall: &call}), func() (fantasy.ObjectStreamResponse, bool, error) {
		response, err := m.streamObjectOnce(ctx, call)
		return response, response != nil, err
	})
}

// providerRejection marks only an actual rejected transport call. A reservation
// or admission error wrapping an HTTP error must never trigger quota retries.
type providerRejection struct{ cause error }

func (e *providerRejection) Error() string { return e.cause.Error() }
func (e *providerRejection) Unwrap() error { return e.cause }
func wrapProviderRejection(err error, observed bool) error {
	if failure, ok := errors.AsType[*fantasy.ProviderError](err); ok && failure.StatusCode == 429 && !observed {
		return &providerRejection{cause: err}
	}
	return err
}
