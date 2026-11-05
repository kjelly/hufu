package agent

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"

	"charm.land/fantasy"
	"charm.land/fantasy/object"
	"charm.land/fantasy/providers/openai"
	"charm.land/fantasy/providers/openaicompat"
	"github.com/kjelly/hufu/internal/config"
)

var (
	ErrResponsesSetting  = errors.New("setting cannot be sent by the Responses adapter")
	ErrResponsesContent  = errors.New("content is not supported by the Responses adapter")
	ErrProtocolBinding   = errors.New("provider API protocol binding mismatch")
	ErrResponsesTerminal = errors.New("responses response did not complete verifiably")
)

type protocolLanguageModel struct {
	fantasy.LanguageModel
	resolution APIProtocolResolution
}

// ModelAPIProtocol follows Hufu wrappers without inferring capabilities from
// Provider() or a model name. Third-party models retain legacy Chat semantics.
func ModelAPIProtocol(model fantasy.LanguageModel) APIProtocolResolution {
	if bound, ok := model.(interface{ APIProtocol() APIProtocolResolution }); ok {
		if resolution := bound.APIProtocol(); resolution.Effective != "" {
			return resolution
		}
	}
	resolution, _ := ResolveAPIProtocol("")
	return resolution
}

func (m *protocolLanguageModel) APIProtocol() APIProtocolResolution { return m.resolution }
func (m *admittedLanguageModel) APIProtocol() APIProtocolResolution { return ModelAPIProtocol(m.inner) }
func (m *structuredResponseModel) APIProtocol() APIProtocolResolution {
	return ModelAPIProtocol(m.LanguageModel)
}

// responsesSDKSendsReasoning pins the private getResponsesModelConfig behavior
// of Fantasy v0.45.5. It is an adapter encoding capability, not a statement
// about what the upstream accepts. Wire contract tests cover the branches.
func responsesSDKSendsReasoning(model string) bool {
	lower := strings.ToLower(model)
	if strings.Contains(lower, "gpt-5-chat") {
		return false
	}
	for _, prefix := range []string{"o1", "o3", "o4", "oss"} {
		if strings.HasPrefix(model, prefix) || strings.Contains(model, "-"+prefix) {
			return true
		}
	}
	return strings.Contains(lower, "gpt-5") || strings.Contains(model, "codex-") || strings.Contains(model, "computer-use")
}

func validateResponsesRequest(request ProviderRequest) error {
	if request.AdmissionContext.APIProtocol.Effective != config.APIProtocolResponses {
		return nil
	}
	call, err := requestCall(request)
	if err != nil {
		return err
	}
	if _, err := responsesInputEstimate(call.Prompt); err != nil {
		return err
	}
	for _, tool := range call.Tools {
		if _, ok := tool.(fantasy.FunctionTool); !ok {
			return fmt.Errorf("%w: only local function tools are supported", ErrResponsesContent)
		}
	}
	options := call.ProviderOptions
	if request.ObjectCall != nil {
		options = request.ObjectCall.ProviderOptions
	}
	for key, value := range options {
		switch typed := value.(type) {
		case *openaicompat.ProviderOptions:
			if typed != nil && (typed.ReasoningEffort != nil || len(typed.ExtraBody) > 0) {
				return fmt.Errorf("%w: Responses requires its own reasoning options", ErrResponsesSetting)
			}
		case *openai.ProviderOptions:
			if typed != nil && typed.ReasoningEffort != nil {
				return fmt.Errorf("%w: Chat reasoning options cannot be used for Responses", ErrResponsesSetting)
			}
		case *openai.ResponsesProviderOptions:
			if typed == nil {
				continue
			}
			if key != openai.Name {
				return fmt.Errorf("%w: Responses options must use the openai key", ErrResponsesSetting)
			}
			if typed.Store != nil && *typed.Store || typed.PreviousResponseID != nil && *typed.PreviousResponseID != "" {
				return fmt.Errorf("%w: server-side response history is disabled", ErrResponsesSetting)
			}
			if typed.ReasoningEffort != nil {
				effort := string(*typed.ReasoningEffort)
				if !ValidReasoningEfforts[effort] || !responsesSDKSendsReasoning(request.ModelID) {
					return fmt.Errorf("%w: reasoning effort %q for model %q", ErrResponsesSetting, effort, request.ModelID)
				}
			}
		}
	}
	return nil
}

func (m *protocolLanguageModel) validate(call *fantasy.Call, object *fantasy.ObjectCall) error {
	return validateResponsesRequest(ProviderRequest{ModelID: m.Model(), Call: call, ObjectCall: object, AdmissionContext: ProviderAdmissionContext{APIProtocol: m.resolution}})
}

func (m *protocolLanguageModel) warn(warnings []fantasy.CallWarning) {
	if m.resolution.Effective != config.APIProtocolResponses {
		return
	}
	for _, warning := range warnings {
		if warning.Type == fantasy.CallWarningTypeUnsupportedSetting && (warning.Setting == "temperature" || warning.Setting == "topP") {
			param := warning.Setting
			if param == "topP" {
				param = "top-p"
			}
			if _, loaded := unsupportedSamplerWarnSeen.LoadOrStore("responses-model:"+m.Model()+":"+param, struct{}{}); !loaded {
				log.Printf("warning: Responses model %q ignores %s in the pinned adapter", m.Model(), param)
			}
		}
	}
}

func (m *protocolLanguageModel) Generate(ctx context.Context, call fantasy.Call) (*fantasy.Response, error) {
	if err := m.validate(&call, nil); err != nil {
		return nil, err
	}
	ctx = m.responseContext(ctx)
	response, err := m.LanguageModel.Generate(ctx, call)
	if response != nil {
		m.warn(response.Warnings)
		if err == nil {
			err = m.terminalError(ctx, response.FinishReason)
		}
	}
	return response, err
}
func (m *protocolLanguageModel) Stream(ctx context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
	if err := m.validate(&call, nil); err != nil {
		return nil, err
	}
	ctx = m.responseContext(ctx)
	stream, err := m.LanguageModel.Stream(ctx, call)
	if err != nil {
		return nil, err
	}
	return func(yield func(fantasy.StreamPart) bool) {
		var pending []fantasy.StreamPart
		pendingBytes := 0
		for part := range stream {
			if m.resolution.Effective == config.APIProtocolResponses && part.Type == fantasy.StreamPartTypeToolCall {
				pendingBytes += responsesItemAllowance + len(part.ID) + len(part.ToolCallName) + len(part.ToolCallInput)
				if pendingBytes > maxObservedProviderBody {
					yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeError, Error: ErrResponsesContent})
					return
				}
				pending = append(pending, part)
				continue
			}
			if part.Type == fantasy.StreamPartTypeFinish {
				if terminalErr := m.terminalError(ctx, part.FinishReason); terminalErr != nil {
					yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeError, Error: terminalErr})
					return
				}
				for _, call := range pending {
					if !yield(call) {
						return
					}
				}
				pending = nil
			}
			m.warn(part.Warnings)
			if !yield(part) {
				return
			}
		}
	}, nil
}
func (m *protocolLanguageModel) GenerateObject(ctx context.Context, call fantasy.ObjectCall) (*fantasy.ObjectResponse, error) {
	if err := m.validate(nil, &call); err != nil {
		return nil, err
	}
	ctx = m.responseContext(ctx)
	response, err := m.LanguageModel.GenerateObject(ctx, call)
	if response != nil {
		m.warn(response.Warnings)
		if err == nil {
			err = m.terminalError(ctx, response.FinishReason)
		}
	}
	return response, err
}
func (m *protocolLanguageModel) StreamObject(ctx context.Context, call fantasy.ObjectCall) (fantasy.ObjectStreamResponse, error) {
	if err := m.validate(nil, &call); err != nil {
		return nil, err
	}
	ctx = m.responseContext(ctx)
	var stream fantasy.ObjectStreamResponse
	var err error
	if m.resolution.Effective == config.APIProtocolResponses {
		// openaicompat's pinned object mode is tool-based. Use Fantasy's
		// helper with the guarded Stream entry point, so a tool result is
		// never finalized before the response's terminal event arrives.
		stream, err = object.StreamWithTool(ctx, m, call)
	} else {
		stream, err = m.LanguageModel.StreamObject(ctx, call)
	}
	if err != nil {
		return nil, err
	}
	return func(yield func(fantasy.ObjectStreamPart) bool) {
		for part := range stream {
			status := responsesTerminalStatus(ctx)
			if part.Type == fantasy.ObjectStreamPartTypeFinish || part.Type == fantasy.ObjectStreamPartTypeObject && status != "" && status != "completed" {
				reason := part.FinishReason
				if part.Type == fantasy.ObjectStreamPartTypeObject {
					reason = fantasy.FinishReasonToolCalls
				}
				if terminalErr := m.terminalError(ctx, reason); terminalErr != nil {
					yield(fantasy.ObjectStreamPart{Type: fantasy.ObjectStreamPartTypeError, Error: terminalErr})
					return
				}
			}
			m.warn(part.Warnings)
			if !yield(part) {
				return
			}
		}
	}, nil
}

func (m *protocolLanguageModel) responseContext(ctx context.Context) context.Context {
	if m.resolution.Effective == config.APIProtocolResponses {
		if _, exists := ctx.Value(responsesTerminalKey{}).(*responsesTerminalObservation); exists {
			return ctx
		}
		return withResponsesTerminal(ctx)
	}
	return ctx
}

func (m *protocolLanguageModel) terminalError(ctx context.Context, reason fantasy.FinishReason) error {
	if m.resolution.Effective != config.APIProtocolResponses {
		return nil
	}
	status := responsesTerminalStatus(ctx)
	if status == "completed" && (reason == fantasy.FinishReasonStop || reason == fantasy.FinishReasonToolCalls) {
		return nil
	}
	if status == "" {
		status = "unobserved"
	}
	return fmt.Errorf("%w: status=%s finish_reason=%s", ErrResponsesTerminal, status, reason)
}
