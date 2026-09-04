package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"iter"
	"sync"

	"charm.land/fantasy"
	"charm.land/fantasy/providers/openaicompat"
	"charm.land/fantasy/schema"
)

// ProviderRequest is the provider-equivalent request shape used by Hufu's
// admission layer. Messages already include the system message and every
// multimodal/tool part that Fantasy will send for the step.
type ProviderRequest struct {
	ModelID    string
	Call       *fantasy.Call
	ObjectCall *fantasy.ObjectCall
	Messages   []fantasy.Message
	Tools      []fantasy.AgentTool
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
	modelID           string
	inner             fantasy.LanguageModel
	admission         RequestAdmission
	invocationLimiter InvocationLimiter
	admissionContext  ProviderAdmissionContext
}

// RequestAdmission is the shared pre-provider admission contract.
type RequestAdmission interface {
	AdmitProviderRequest(context.Context, ProviderRequest) error
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
	return &admittedLanguageModel{modelID: modelID, inner: inner, admission: admission, invocationLimiter: limiter, admissionContext: context}
}

func (m *admittedLanguageModel) acquireInvocation(ctx context.Context) (func(), error) {
	if m.invocationLimiter == nil {
		return nil, nil
	}
	return m.invocationLimiter.AcquireProviderInvocation(ctx, m.modelID)
}

func admittedStream[T any](stream iter.Seq[T], cancel context.CancelFunc, cleanup func(), stopCleanup func() bool) iter.Seq[T] {
	return func(yield func(T) bool) {
		defer func() {
			stopCleanup()
			cleanup()
		}()
		stream(func(part T) bool {
			keepGoing := yield(part)
			if !keepGoing {
				cancel()
			}
			return keepGoing
		})
	}
}

func (m *admittedLanguageModel) streamContext(ctx context.Context, release func()) (context.Context, context.CancelFunc, func(), func() bool) {
	streamCtx, cancel := context.WithCancel(ctx)
	cleanup := sync.OnceFunc(func() {
		cancel()
		if release != nil {
			release()
		}
	})
	stopCleanup := context.AfterFunc(ctx, cleanup)
	return streamCtx, cancel, cleanup, stopCleanup
}

func (m *admittedLanguageModel) request(request ProviderRequest) ProviderRequest {
	request.ModelID = m.modelID
	request.AdmissionContext = m.admissionContext
	return request
}

func (m *admittedLanguageModel) Generate(ctx context.Context, call fantasy.Call) (*fantasy.Response, error) {
	if err := m.admission.AdmitProviderRequest(ctx, m.request(ProviderRequest{Call: &call})); err != nil {
		return nil, err
	}
	release, err := m.acquireInvocation(ctx)
	if err != nil {
		return nil, err
	}
	defer func() {
		if release != nil {
			release()
		}
	}()
	return m.inner.Generate(ctx, call)
}

func (m *admittedLanguageModel) Stream(ctx context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
	if err := m.admission.AdmitProviderRequest(ctx, m.request(ProviderRequest{Call: &call})); err != nil {
		return nil, err
	}
	release, err := m.acquireInvocation(ctx)
	if err != nil {
		return nil, err
	}
	streamCtx, cancel, cleanup, stopCleanup := m.streamContext(ctx, release)
	// Keep the registered .Stream(ctx, chokepoint marker while passing the
	// derived context required to own the stream's transport lifetime.
	stream, err := m.inner.Stream(streamCtx, call)
	if err != nil {
		stopCleanup()
		cleanup()
		return nil, err
	}
	return admittedStream(stream, cancel, cleanup, stopCleanup), nil
}

func (m *admittedLanguageModel) GenerateObject(ctx context.Context, call fantasy.ObjectCall) (*fantasy.ObjectResponse, error) {
	if err := m.admission.AdmitProviderRequest(ctx, m.request(ProviderRequest{ObjectCall: &call})); err != nil {
		return nil, err
	}
	release, err := m.acquireInvocation(ctx)
	if err != nil {
		return nil, err
	}
	defer func() {
		if release != nil {
			release()
		}
	}()
	return m.inner.GenerateObject(ctx, call)
}

func (m *admittedLanguageModel) StreamObject(ctx context.Context, call fantasy.ObjectCall) (fantasy.ObjectStreamResponse, error) {
	if err := m.admission.AdmitProviderRequest(ctx, m.request(ProviderRequest{ObjectCall: &call})); err != nil {
		return nil, err
	}
	release, err := m.acquireInvocation(ctx)
	if err != nil {
		return nil, err
	}
	streamCtx, cancel, cleanup, stopCleanup := m.streamContext(ctx, release)
	stream, err := m.inner.StreamObject(streamCtx, call)
	if err != nil {
		stopCleanup()
		cleanup()
		return nil, err
	}
	return admittedStream(stream, cancel, cleanup, stopCleanup), nil
}

func (m *admittedLanguageModel) Provider() string { return m.inner.Provider() }

func (m *admittedLanguageModel) Model() string { return m.inner.Model() }
