package team

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"

	"charm.land/fantasy"
)

// CannotFitError is emitted only before the provider boundary. ProvenNoSend
// is intentionally explicit so callers cannot turn an arbitrary provider
// overflow into a replay or model fallback.
type CannotFitError struct {
	ModelID       string
	RequestTokens int
	Available     int
	ProvenNoSend  bool
}

func (e *CannotFitError) Error() string {
	if e == nil {
		return "context request cannot fit"
	}
	return fmt.Sprintf("context window admission cannot fit request for model %q: %d tokens exceeds available budget %d", e.ModelID, e.RequestTokens, e.Available)
}

func isProvenPreProviderCannotFit(err error) (*CannotFitError, bool) {
	var fit *CannotFitError
	if !errors.As(err, &fit) {
		return nil, false
	}
	return fit, fit.ProvenNoSend
}

// ContextWindowMetadataUnavailableError is returned before counting or
// invoking a provider when the registry has no usable capacity at all.
// A family-fallback estimate with a positive window is admissible: the
// estimator multiplier and safety margin cover the uncertainty, and the
// runtime overflow-recovery path replaces the estimate with an observed
// window after the first provider refusal. Only a spec that is both
// estimated and windowless leaves admission with no capacity to enforce.
type ContextWindowMetadataUnavailableError struct {
	ModelID string
}

func (e *ContextWindowMetadataUnavailableError) Error() string {
	if e == nil {
		return "context window metadata unavailable"
	}
	return fmt.Sprintf("context window metadata unavailable for model %q: registry capacity is estimated", e.ModelID)
}

// ContextWindowDecision is the result of admitting one complete model request.
type ContextWindowDecision string

const (
	ContextWindowNoop           ContextWindowDecision = "noop"
	ContextWindowCompactPreTurn ContextWindowDecision = "compact_pre_turn"
	ContextWindowCompactMidTurn ContextWindowDecision = "compact_mid_turn"
	ContextWindowCannotFit      ContextWindowDecision = "cannot_fit"
)

// ContextWindowRequest is the complete request presented to the context
// admission owner. Messages are the messages Fantasy will send for the step;
// Prompt is counted only when it is not already present in Messages.
type ContextWindowRequest struct {
	ModelID              string
	System               string
	Tools                []fantasy.AgentTool
	Messages             []fantasy.Message
	Prompt               string
	ReservedOutputTokens int
	SafetyMarginTokens   int
	Window               int
	StepNumber           int
}

// ContextWindowCandidate is a verified transient message projection that still
// needs complete-request admission after system and tool projection.
type ContextWindowCandidate struct {
	Messages []fantasy.Message
}

// ContextWindowAdmission is an immutable result candidate. Messages is the
// admitted request when Decision is not ContextWindowCannotFit. Candidate is
// set when history compaction produced a verified request candidate that still
// needs system/tool projection before final admission.
type ContextWindowAdmission struct {
	Decision  ContextWindowDecision
	Messages  []fantasy.Message
	Candidate *ContextWindowCandidate

	RequestTokens int
	Budget        ContextBudget
}

// ContextWindowManager owns request-token admission for one stream. It
// deliberately accepts a compactor callback instead of implementing another
// compaction algorithm; the coordinator supplies the existing verified
// structured compactor.
type ContextWindowManager struct {
	counter                TokenCounter
	compact                func(context.Context, []fantasy.Message) ([]fantasy.Message, error)
	compactWithPredecessor func(context.Context, []fantasy.Message, *StructuredSummary) ([]fantasy.Message, *StructuredSummary, error)

	mu                  sync.Mutex
	compactedSource     []fantasy.Message
	compactedProjection []fantasy.Message
	compactedSummary    *StructuredSummary
	hasProjection       bool
}

func NewContextWindowManager(counter TokenCounter, compact func(context.Context, []fantasy.Message) ([]fantasy.Message, error)) *ContextWindowManager {
	if counter == nil {
		counter = defaultCounter
	}
	return &ContextWindowManager{counter: counter, compact: compact}
}

// NewContextWindowManagerWithPredecessor constructs a stream-local admission
// manager whose compactor receives the previous transient summary and returns
// the new summary alongside its verified message projection.
func NewContextWindowManagerWithPredecessor(counter TokenCounter, compact func(context.Context, []fantasy.Message, *StructuredSummary) ([]fantasy.Message, *StructuredSummary, error)) *ContextWindowManager {
	if counter == nil {
		counter = defaultCounter
	}
	return &ContextWindowManager{counter: counter, compactWithPredecessor: compact}
}

// Admit evaluates the full request, optionally replacing all conversational
// messages with one verified replacement. No candidate is accepted unless it
// satisfies request tokens + reserved output + safety margin <= model window.
func (m *ContextWindowManager) Admit(ctx context.Context, request ContextWindowRequest) (ContextWindowAdmission, error) {
	if m == nil {
		return ContextWindowAdmission{Decision: ContextWindowCannotFit}, fmt.Errorf("context window manager is nil")
	}
	// A manager is stream-local. Serializing admission also makes a verified
	// projection idempotent if Fantasy or a test seam asks to prepare the same
	// step more than once.
	m.mu.Lock()
	defer m.mu.Unlock()

	if !toolPairsIntact(request.Messages) {
		return ContextWindowAdmission{Decision: ContextWindowCannotFit}, fmt.Errorf("context window request has invalid tool-call/result pairing")
	}
	spec := globalRegistry.GetSpec(request.ModelID)
	if request.Window > 0 {
		spec.ContextWindow = request.Window
		// A caller-provided window is authoritative only when the caller also
		// supplies an exact admission capacity. Preserve fail-closed behavior
		// for estimated registry metadata; team setup registers operator values
		// with IsEstimated=false before admission reaches this path.
	}
	if request.ReservedOutputTokens > 0 {
		spec.MaxOutputTokens = request.ReservedOutputTokens
	}
	if request.SafetyMarginTokens > 0 {
		spec.SafetyMarginTokens = request.SafetyMarginTokens
	}
	budget := CalculateContextBudget(spec, 0, 0)
	admission := ContextWindowAdmission{Decision: ContextWindowCannotFit, Budget: budget}
	if spec.IsEstimated && spec.ContextWindow <= 0 {
		return admission, &ContextWindowMetadataUnavailableError{ModelID: request.ModelID}
	}

	originalMessages := cloneMessages(request.Messages)
	requestTokens, err := m.countRequest(ctx, request)
	if err != nil {
		return admission, fmt.Errorf("count context window request: %w", err)
	}
	admission.RequestTokens = requestTokens
	if requestTokens <= budget.Available {
		admission.Decision = ContextWindowNoop
		admission.Messages = request.effectiveMessages()
		return admission, nil
	}

	// The incoming prompt is part of Fantasy's opts.Messages in the real
	// stream. Remove that one current message before handing history to the
	// compactor; otherwise prompt-only overflow can invoke compaction and the
	// compactor can summarize the request that must remain verbatim.
	prior, fixed, currentPrompt := splitRequestForCompaction(request)
	candidateHistory := prior
	compacted := false
	if len(prior) > 0 {
		switch {
		case m.hasProjection && reflect.DeepEqual(prior, m.compactedProjection):
			// This is the exact projection produced for this stream. It can be
			// reused without another compaction, including the second admission
			// of one PrepareStep after system/tool preflight.
			compacted = true
		case m.hasProjection && reflect.DeepEqual(prior, m.compactedSource):
			candidateHistory = cloneMessages(m.compactedProjection)
			compacted = true
		case hasCompactableHistory(prior) && m.compactWithPredecessor != nil:
			compactedHistory, summary, compactErr := m.compactWithPredecessor(ctx, prior, cloneStructuredSummary(m.compactedSummary))
			if compactErr != nil {
				return admission, fmt.Errorf("compact context history: %w", compactErr)
			}
			if validCompactedHistory(compactedHistory, prior) {
				candidateHistory = cloneMessages(compactedHistory)
				m.compactedSource = cloneMessages(prior)
				m.compactedProjection = cloneMessages(compactedHistory)
				m.compactedSummary = cloneStructuredSummary(summary)
				m.hasProjection = true
				compacted = true
			}
		case hasCompactableHistory(prior) && m.compact != nil:
			compactedHistory, compactErr := m.compact(ctx, prior)
			if compactErr != nil {
				return admission, fmt.Errorf("compact context history: %w", compactErr)
			}
			if validCompactedHistory(compactedHistory, prior) {
				candidateHistory = cloneMessages(compactedHistory)
				m.compactedSource = cloneMessages(prior)
				m.compactedProjection = cloneMessages(compactedHistory)
				m.hasProjection = true
				compacted = true
			}
		}
	}

	// Do not expose a CannotFit candidate to the caller. A caller may use the
	// original request to produce a system/tool projection, but must re-enter
	// this manager for final admission before sending anything to the provider.
	if !compacted {
		admission.Messages = originalMessages
		return admission, nil
	}
	candidate := append(cloneMessages(fixed), candidateHistory...)
	if currentPrompt != "" {
		candidate = append(candidate, fantasy.NewUserMessage(currentPrompt))
	}
	if !toolPairsIntact(candidate) {
		return admission, fmt.Errorf("context window compacted request has invalid tool-call/result pairing")
	}
	request.Messages = candidate
	requestTokens, err = m.countRequest(ctx, request)
	if err != nil {
		return admission, fmt.Errorf("count compacted context window request: %w", err)
	}
	admission.RequestTokens = requestTokens
	if requestTokens <= budget.Available {
		admission.Messages = candidate
		if request.StepNumber > 0 {
			admission.Decision = ContextWindowCompactMidTurn
		} else {
			admission.Decision = ContextWindowCompactPreTurn
		}
		return admission, nil
	}
	admission.Candidate = &ContextWindowCandidate{Messages: cloneMessages(candidate)}
	return admission, nil
}

func validCompactedHistory(compactedHistory, prior []fantasy.Message) bool {
	return len(compactedHistory) > 0 && !reflect.DeepEqual(compactedHistory, prior) &&
		containsVerifiedHistory(compactedHistory) && toolPairsIntact(compactedHistory)
}

func hasCompactableHistory(messages []fantasy.Message) bool {
	for _, message := range messages {
		if !isVerifiedHistoryMessage(message) {
			return true
		}
	}
	return false
}

func (m *ContextWindowManager) countRequest(ctx context.Context, request ContextWindowRequest) (int, error) {
	messages := request.effectiveMessages()
	messageTokens, err := m.counter.CountMessages(ctx, request.ModelID, messages)
	if err != nil {
		return 0, err
	}
	toolTokens, err := m.counter.CountTools(ctx, request.ModelID, request.Tools)
	if err != nil {
		return 0, err
	}
	return messageTokens + toolTokens, nil
}

func splitRequestForCompaction(request ContextWindowRequest) (prior, fixed []fantasy.Message, currentPrompt string) {
	messages := request.effectiveMessages()
	if request.Prompt != "" {
		for i := len(messages) - 1; i >= 0; i-- {
			if messages[i].Role != fantasy.MessageRoleUser || len(messages[i].Content) != 1 {
				continue
			}
			part, ok := fantasy.AsMessagePart[fantasy.TextPart](messages[i].Content[0])
			if !ok || part.Text != request.Prompt {
				continue
			}
			currentPrompt = part.Text
			messages = append(messages[:i], messages[i+1:]...)
			break
		}
		if currentPrompt == "" {
			currentPrompt = request.Prompt
		}
	}
	prior, fixed = splitSystemMessage(messages)
	return prior, fixed, currentPrompt
}

func (r ContextWindowRequest) effectiveMessages() []fantasy.Message {
	messages := cloneMessages(r.Messages)
	if r.System != "" {
		if _, ok := firstMessageWithRole(messages, fantasy.MessageRoleSystem); ok {
			for i := range messages {
				if messages[i].Role == fantasy.MessageRoleSystem {
					messages[i] = fantasy.NewSystemMessage(r.System)
					break
				}
			}
		} else {
			messages = append([]fantasy.Message{fantasy.NewSystemMessage(r.System)}, messages...)
		}
	}
	if r.Prompt != "" && !hasExactUserMessage(messages, r.Prompt) {
		messages = append(messages, fantasy.NewUserMessage(r.Prompt))
	}
	return messages
}

func splitSystemMessage(messages []fantasy.Message) ([]fantasy.Message, []fantasy.Message) {
	for i, message := range messages {
		if message.Role == fantasy.MessageRoleSystem {
			fixed := []fantasy.Message{message}
			history := make([]fantasy.Message, 0, len(messages)-1)
			history = append(history, messages[:i]...)
			history = append(history, messages[i+1:]...)
			return history, fixed
		}
	}
	return cloneMessages(messages), nil
}

func cloneMessages(messages []fantasy.Message) []fantasy.Message {
	if messages == nil {
		return nil
	}
	cloned := make([]fantasy.Message, len(messages))
	copy(cloned, messages)
	return cloned
}

func toolPairsIntact(messages []fantasy.Message) bool {
	calls := make(map[string]struct{})
	results := make(map[string]struct{})
	for _, message := range messages {
		for _, part := range message.Content {
			if call, ok := fantasy.AsMessagePart[fantasy.ToolCallPart](part); ok {
				if call.ToolCallID == "" {
					return false
				}
				if _, duplicate := calls[call.ToolCallID]; duplicate {
					return false
				}
				calls[call.ToolCallID] = struct{}{}
			}
			if result, ok := fantasy.AsMessagePart[fantasy.ToolResultPart](part); ok {
				if result.ToolCallID == "" {
					return false
				}
				if _, exists := calls[result.ToolCallID]; !exists {
					return false
				}
				if _, duplicate := results[result.ToolCallID]; duplicate {
					return false
				}
				results[result.ToolCallID] = struct{}{}
			}
		}
	}
	return len(calls) == len(results)
}

func containsVerifiedHistory(messages []fantasy.Message) bool {
	for _, message := range messages {
		if isVerifiedHistoryMessage(message) {
			return true
		}
	}
	return false
}
