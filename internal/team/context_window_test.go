package team

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"charm.land/fantasy"
)

func TestContextWindowManagerCompactsHugeRecentTailBeforePreflight(t *testing.T) {
	modelID := "context-window-under-100"
	GlobalModelSpecRegistry().RegisterSpec(ModelContextSpec{
		ModelID:            modelID,
		ContextWindow:      512,
		MaxOutputTokens:    64,
		SafetyMarginTokens: 32,
	})

	call := fantasy.ToolCallPart{ToolCallID: "call-1", ToolName: "bash", Input: `{"command":"go test ./..."}`}
	result := fantasy.ToolResultPart{ToolCallID: "call-1", Output: fantasy.ToolResultOutputContentText{Text: strings.Repeat("recent tool output ", 2_000)}}
	messages := []fantasy.Message{
		fantasy.NewSystemMessage("coordinator system"),
		fantasy.NewUserMessage("goal"),
		{Role: fantasy.MessageRoleAssistant, Content: []fantasy.MessagePart{call}},
		{Role: fantasy.MessageRoleTool, Content: []fantasy.MessagePart{result}},
	}
	compacted := false
	manager := NewContextWindowManager(defaultCounter, func(context.Context, []fantasy.Message) ([]fantasy.Message, error) {
		compacted = true
		return []fantasy.Message{fantasy.NewUserMessage(verifiedHistoryPrefix + "verified tool evidence")}, nil
	})

	admission, err := manager.Admit(context.Background(), ContextWindowRequest{
		ModelID:              modelID,
		System:               "coordinator system",
		Messages:             messages,
		Prompt:               "goal",
		ReservedOutputTokens: 64,
		SafetyMarginTokens:   32,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !compacted {
		t.Fatal("expected compaction for a four-message request with a huge recent tool result")
	}
	if admission.Decision != ContextWindowCompactPreTurn {
		t.Fatalf("decision = %q, want %q", admission.Decision, ContextWindowCompactPreTurn)
	}
	if admission.RequestTokens > admission.Budget.Available {
		t.Fatalf("request tokens = %d, budget = %d", admission.RequestTokens, admission.Budget.Available)
	}
	if !toolPairsIntact(admission.Messages) {
		t.Fatal("compacted request contains an orphan tool call/result")
	}
}

func TestContextWindowManagerExposesUnfitCompactedCandidateForPreflight(t *testing.T) {
	modelID := "context-window-unfit-candidate"
	GlobalModelSpecRegistry().RegisterSpec(ModelContextSpec{
		ModelID:            modelID,
		ContextWindow:      512,
		MaxOutputTokens:    64,
		SafetyMarginTokens: 32,
	})
	optional := fantasy.NewAgentTool("inspect", strings.Repeat("optional tool guidance ", 300), func(context.Context, map[string]any, fantasy.ToolCall) (fantasy.ToolResponse, error) {
		return fantasy.NewTextResponse("unused"), nil
	})
	manager := NewContextWindowManager(defaultCounter, func(context.Context, []fantasy.Message) ([]fantasy.Message, error) {
		return []fantasy.Message{fantasy.NewUserMessage(verifiedHistoryPrefix + "verified history")}, nil
	})
	admission, err := manager.Admit(context.Background(), ContextWindowRequest{
		ModelID:              modelID,
		System:               "system",
		Tools:                []fantasy.AgentTool{optional},
		Messages:             []fantasy.Message{fantasy.NewUserMessage(strings.Repeat("old history ", 500))},
		Prompt:               "incoming prompt",
		ReservedOutputTokens: 64,
		SafetyMarginTokens:   32,
	})
	if err != nil {
		t.Fatal(err)
	}
	if admission.Decision != ContextWindowCannotFit {
		t.Fatalf("decision = %q, want %q until preflight projects tools", admission.Decision, ContextWindowCannotFit)
	}
	if admission.Messages != nil {
		t.Fatal("unfit candidate was exposed as admitted Messages")
	}
	if admission.Candidate == nil {
		t.Fatal("missing typed compacted candidate")
	}
	if !containsVerifiedHistory(admission.Candidate.Messages) {
		t.Fatal("candidate did not retain verified compacted history")
	}
	if got := countExactUserMessages(admission.Candidate.Messages, "incoming prompt"); got != 1 {
		t.Fatalf("candidate incoming prompt occurrences = %d, want exactly one", got)
	}
}

func TestContextWindowManagerIncomingPromptOverflowCannotFit(t *testing.T) {
	modelID := "context-window-incoming-overflow"
	GlobalModelSpecRegistry().RegisterSpec(ModelContextSpec{
		ModelID:            modelID,
		ContextWindow:      256,
		MaxOutputTokens:    32,
		SafetyMarginTokens: 32,
	})
	original := []fantasy.Message{fantasy.NewSystemMessage("system")}
	compactionCalls := 0
	manager := NewContextWindowManager(defaultCounter, func(context.Context, []fantasy.Message) ([]fantasy.Message, error) {
		compactionCalls++
		return nil, nil
	})
	admission, err := manager.Admit(context.Background(), ContextWindowRequest{
		ModelID:              modelID,
		System:               "system",
		Messages:             original,
		Prompt:               strings.Repeat("incoming prompt ", 1_000),
		ReservedOutputTokens: 32,
		SafetyMarginTokens:   32,
	})
	if err != nil {
		t.Fatal(err)
	}
	if admission.Decision != ContextWindowCannotFit {
		t.Fatalf("decision = %q, want %q", admission.Decision, ContextWindowCannotFit)
	}
	if compactionCalls != 0 {
		t.Fatalf("incoming-only overflow invoked compaction %d times", compactionCalls)
	}
	if len(admission.Messages) != len(original) {
		t.Fatal("cannot-fit admission did not retain the original messages")
	}
}

func TestContextWindowManagerActualOptsPromptIsNeverCompacted(t *testing.T) {
	modelID := "context-window-actual-opts-prompt"
	GlobalModelSpecRegistry().RegisterSpec(ModelContextSpec{
		ModelID:            modelID,
		ContextWindow:      256,
		MaxOutputTokens:    32,
		SafetyMarginTokens: 32,
	})
	prompt := strings.Repeat("incoming prompt ", 1_000)
	messages := []fantasy.Message{fantasy.NewSystemMessage("system"), fantasy.NewUserMessage(prompt)}
	compactionCalls := 0
	manager := NewContextWindowManager(defaultCounter, func(context.Context, []fantasy.Message) ([]fantasy.Message, error) {
		compactionCalls++
		return []fantasy.Message{fantasy.NewUserMessage(verifiedHistoryPrefix + "must not be used")}, nil
	})
	admission, err := manager.Admit(context.Background(), ContextWindowRequest{
		ModelID:              modelID,
		System:               "system",
		Messages:             messages,
		Prompt:               prompt,
		ReservedOutputTokens: 32,
		SafetyMarginTokens:   32,
	})
	if err != nil {
		t.Fatal(err)
	}
	if admission.Decision != ContextWindowCannotFit {
		t.Fatalf("decision = %q, want %q", admission.Decision, ContextWindowCannotFit)
	}
	if compactionCalls != 0 {
		t.Fatalf("actual incoming prompt invoked compaction %d times", compactionCalls)
	}
}

func TestCapStepMessagesWithCounterReportsStillOverBudget(t *testing.T) {
	messages := []fantasy.Message{fantasy.NewSystemMessage("system"), fantasy.NewUserMessage("goal")}
	for range recentMessagesProtected {
		messages = append(messages, fantasy.NewUserMessage(strings.Repeat("recent ", 2_000)))
	}
	result := CapStepMessagesWithCounterResult(context.Background(), defaultCounter, "capper-still-over", messages, 10)
	if !result.StillOverBudget {
		t.Fatalf("StillOverBudget = false, tokens = %d", result.Tokens)
	}
	if result.Messages == nil || len(result.Messages) != len(messages) {
		t.Fatal("capper did not return the retained messages for admission to reject")
	}
}

func TestContextWindowManagerRejectsOrphanToolPair(t *testing.T) {
	modelID := "context-window-orphan-pair"
	GlobalModelSpecRegistry().RegisterSpec(ModelContextSpec{
		ModelID:            modelID,
		ContextWindow:      4_096,
		MaxOutputTokens:    64,
		SafetyMarginTokens: 32,
	})
	messages := []fantasy.Message{
		fantasy.NewSystemMessage("system"),
		{Role: fantasy.MessageRoleTool, Content: []fantasy.MessagePart{
			fantasy.ToolResultPart{ToolCallID: "missing-call", Output: fantasy.ToolResultOutputContentText{Text: "result"}},
		}},
	}
	manager := NewContextWindowManager(defaultCounter, func(context.Context, []fantasy.Message) ([]fantasy.Message, error) {
		return messages[1:], nil
	})
	admission, err := manager.Admit(context.Background(), ContextWindowRequest{ModelID: modelID, System: "system", Messages: messages})
	if err == nil || admission.Decision != ContextWindowCannotFit {
		t.Fatalf("admission = %#v, err = %v; want fail-closed orphan rejection", admission, err)
	}
}

func TestContextWindowManagerRejectsDuplicateToolPairIDs(t *testing.T) {
	modelID := "context-window-duplicate-pair"
	GlobalModelSpecRegistry().RegisterSpec(ModelContextSpec{ModelID: modelID, ContextWindow: 4_096})
	messages := []fantasy.Message{
		{Role: fantasy.MessageRoleAssistant, Content: []fantasy.MessagePart{
			fantasy.ToolCallPart{ToolCallID: "duplicate", ToolName: "view"},
		}},
		{Role: fantasy.MessageRoleAssistant, Content: []fantasy.MessagePart{
			fantasy.ToolCallPart{ToolCallID: "duplicate", ToolName: "view"},
		}},
		{Role: fantasy.MessageRoleTool, Content: []fantasy.MessagePart{
			fantasy.ToolResultPart{ToolCallID: "duplicate", Output: fantasy.ToolResultOutputContentText{Text: "ok"}},
		}},
	}
	compactionCalls := 0
	manager := NewContextWindowManager(defaultCounter, func(context.Context, []fantasy.Message) ([]fantasy.Message, error) {
		compactionCalls++
		return []fantasy.Message{fantasy.NewUserMessage(verifiedHistoryPrefix + "invalid input")}, nil
	})
	_, err := manager.Admit(context.Background(), ContextWindowRequest{ModelID: modelID, Messages: messages})
	if err == nil || compactionCalls != 0 {
		t.Fatalf("duplicate pair was not rejected before compaction: err=%v calls=%d", err, compactionCalls)
	}
}

func TestContextWindowManagerReusesStreamLocalCompactionProjection(t *testing.T) {
	modelID := "context-window-reuse-projection"
	GlobalModelSpecRegistry().RegisterSpec(ModelContextSpec{
		ModelID: modelID, ContextWindow: 512, MaxOutputTokens: 64, SafetyMarginTokens: 32,
	})
	original := []fantasy.Message{
		fantasy.NewSystemMessage("system"),
		fantasy.NewUserMessage(strings.Repeat("old history ", 2_000)),
	}
	compactionCalls := 0
	manager := NewContextWindowManager(defaultCounter, func(context.Context, []fantasy.Message) ([]fantasy.Message, error) {
		compactionCalls++
		return []fantasy.Message{fantasy.NewUserMessage(verifiedHistoryPrefix + "verified history")}, nil
	})
	request := ContextWindowRequest{ModelID: modelID, Messages: original, System: "system", Prompt: "goal"}
	first, err := manager.Admit(context.Background(), request)
	if err != nil || first.Decision == ContextWindowCannotFit {
		t.Fatalf("first admission = %#v, err=%v; want compacted fit", first, err)
	}
	second, err := manager.Admit(context.Background(), request)
	if err != nil || second.Decision == ContextWindowCannotFit {
		t.Fatalf("second admission = %#v, err=%v; want reused fit", second, err)
	}
	if compactionCalls != 1 {
		t.Fatalf("compaction calls = %d, want 1 for a repeated stream-local request", compactionCalls)
	}
}

func TestContextWindowManagerRecompactsVerifiedHistoryWithNewToolResult(t *testing.T) {
	modelID := "context-window-lineage-recompact"
	GlobalModelSpecRegistry().RegisterSpec(ModelContextSpec{
		ModelID: modelID, ContextWindow: 512, MaxOutputTokens: 64, SafetyMarginTokens: 32,
	})
	prompt := "current prompt must remain verbatim"
	original := []fantasy.Message{
		fantasy.NewSystemMessage("system"),
		fantasy.NewUserMessage(strings.Repeat("old history ", 2_000)),
		fantasy.NewUserMessage(prompt),
	}
	compactionCalls := 0
	manager := NewContextWindowManagerWithPredecessor(defaultCounter, func(_ context.Context, messages []fantasy.Message, predecessor *StructuredSummary) ([]fantasy.Message, *StructuredSummary, error) {
		compactionCalls++
		compactionInput := formatMessagesForCompaction(messages)
		if strings.Contains(compactionInput, prompt) {
			return nil, nil, fmt.Errorf("current prompt entered compactor")
		}
		if compactionCalls == 1 {
			if predecessor != nil {
				return nil, nil, fmt.Errorf("first compaction received a predecessor")
			}
			return []fantasy.Message{fantasy.NewUserMessage(verifiedHistoryPrefix + "verified prior fact")}, &StructuredSummary{Goal: "goal", KeyDecisions: []string{"verified prior fact"}}, nil
		}
		if !strings.Contains(compactionInput, "verified prior fact") || !strings.Contains(compactionInput, "new tool result fact") {
			return nil, nil, fmt.Errorf("second compaction did not receive complete prior history: %q", compactionInput)
		}
		if predecessor == nil || len(predecessor.KeyDecisions) != 1 || predecessor.KeyDecisions[0] != "verified prior fact" {
			return nil, nil, fmt.Errorf("second compaction predecessor = %#v", predecessor)
		}
		return []fantasy.Message{fantasy.NewUserMessage(verifiedHistoryPrefix + "verified prior fact; new tool result fact")}, &StructuredSummary{Goal: "goal", KeyDecisions: []string{"verified prior fact", "new tool result fact"}}, nil
	})

	first, err := manager.Admit(context.Background(), ContextWindowRequest{
		ModelID: modelID, System: "system", Messages: original, Prompt: prompt,
		ReservedOutputTokens: 64, SafetyMarginTokens: 32,
	})
	if err != nil || first.Decision == ContextWindowCannotFit {
		t.Fatalf("first admission = %#v, err=%v; want compacted fit", first, err)
	}

	call := fantasy.ToolCallPart{ToolCallID: "call-new", ToolName: "bash", Input: `{"command":"inspect"}`}
	result := fantasy.ToolResultPart{ToolCallID: "call-new", Output: fantasy.ToolResultOutputContentText{Text: strings.Repeat("new tool result fact ", 2_000)}}
	secondMessages := append([]fantasy.Message(nil), first.Messages[:len(first.Messages)-1]...)
	secondMessages = append(secondMessages,
		fantasy.Message{Role: fantasy.MessageRoleAssistant, Content: []fantasy.MessagePart{call}},
		fantasy.Message{Role: fantasy.MessageRoleTool, Content: []fantasy.MessagePart{result}},
		fantasy.NewUserMessage(prompt),
	)
	second, err := manager.Admit(context.Background(), ContextWindowRequest{
		ModelID: modelID, System: "system", Messages: secondMessages, Prompt: prompt,
		ReservedOutputTokens: 64, SafetyMarginTokens: 32, StepNumber: 1,
	})
	if err != nil || second.Decision == ContextWindowCannotFit {
		t.Fatalf("second admission = %#v, err=%v; want re-compacted fit", second, err)
	}
	if compactionCalls != 2 {
		t.Fatalf("compaction calls = %d, want 2 after adding compactable tool history", compactionCalls)
	}
	if second.RequestTokens > second.Budget.Available {
		t.Fatalf("second request tokens = %d, budget = %d", second.RequestTokens, second.Budget.Available)
	}
	if !toolPairsIntact(second.Messages) {
		t.Fatal("re-compacted request broke tool-call/result pairing")
	}
	if got := countExactUserMessages(second.Messages, prompt); got != 1 {
		t.Fatalf("current prompt occurrences = %d, want exactly one", got)
	}
	if !strings.Contains(messageTextSizeAsText(second.Messages), "verified prior fact") {
		t.Fatal("re-compaction dropped the verified prior fact")
	}
}

func TestTransientCompactionCarriesPredecessorThroughDeterministicFallback(t *testing.T) {
	c := &Coordinator{session: &TeamSession{Workspace: t.TempDir()}}
	predecessor := &StructuredSummary{Goal: "retain lineage", KeyDecisions: []string{"fact only in predecessor"}}
	firstMessages := []fantasy.Message{fantasy.NewUserMessage("retain lineage")}
	first := c.buildTransientCompactionProjection(context.Background(), firstMessages, 0, []int{1}, predecessor)
	if first.summary == nil || !containsSummaryString(first.summary.KeyDecisions, "fact only in predecessor") {
		t.Fatalf("first transient summary lost predecessor fact: %#v", first.summary)
	}

	call := fantasy.ToolCallPart{ToolCallID: "call-lineage", ToolName: "view", Input: `{"path":"x"}`}
	result := fantasy.ToolResultPart{ToolCallID: "call-lineage", Output: fantasy.ToolResultOutputContentText{Text: "new evidence"}}
	secondMessages := append(append([]fantasy.Message(nil), first.messages...),
		fantasy.Message{Role: fantasy.MessageRoleAssistant, Content: []fantasy.MessagePart{call}},
		fantasy.Message{Role: fantasy.MessageRoleTool, Content: []fantasy.MessagePart{result}},
	)
	second := c.buildTransientCompactionProjection(context.Background(), secondMessages, 0, []int{1, 1, 1}, first.summary)
	if second.summary == nil || !containsSummaryString(second.summary.KeyDecisions, "fact only in predecessor") {
		t.Fatalf("deterministic fallback lost predecessor fact: %#v", second.summary)
	}
	if !toolPairsIntact(second.messages) {
		t.Fatal("repeated transient compaction broke tool-call/result pairing")
	}
}

func containsSummaryString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func messageTextSizeAsText(messages []fantasy.Message) string {
	var text strings.Builder
	for _, message := range messages {
		for _, part := range message.Content {
			if value, ok := fantasy.AsMessagePart[fantasy.TextPart](part); ok {
				text.WriteString(value.Text)
			}
		}
	}
	return text.String()
}

func TestContextWindowManagerRetainsOriginalAfterCompactionFailure(t *testing.T) {
	modelID := "context-window-compaction-failure"
	GlobalModelSpecRegistry().RegisterSpec(ModelContextSpec{
		ModelID:            modelID,
		ContextWindow:      256,
		MaxOutputTokens:    32,
		SafetyMarginTokens: 32,
	})
	original := []fantasy.Message{fantasy.NewSystemMessage("system"), fantasy.NewUserMessage(strings.Repeat("history ", 1_000))}
	manager := NewContextWindowManager(defaultCounter, func(context.Context, []fantasy.Message) ([]fantasy.Message, error) {
		// An unverified/failed compactor is represented by retaining its input.
		return append([]fantasy.Message(nil), original[1:]...), nil
	})
	admission, err := manager.Admit(context.Background(), ContextWindowRequest{ModelID: modelID, System: "system", Messages: original})
	if err != nil {
		t.Fatal(err)
	}
	if admission.Decision != ContextWindowCannotFit {
		t.Fatalf("decision = %q, want %q", admission.Decision, ContextWindowCannotFit)
	}
	if len(admission.Messages) != len(original) {
		t.Fatal("failed compaction changed history")
	}
	if got, want := messageTextSize(admission.Messages[1]), messageTextSize(original[1]); got != want {
		t.Fatalf("history text size = %d, want original %d", got, want)
	}
}

type contextWindowTestModel struct {
	fantasy.LanguageModel
	modelID string
}

func (m contextWindowTestModel) Model() string  { return m.modelID }
func (contextWindowTestModel) Provider() string { return "context-window-provider" }

type contextWindowCountingAgent struct {
	model fantasy.LanguageModel
	calls *int
}

func (a contextWindowCountingAgent) Generate(context.Context, fantasy.AgentCall) (*fantasy.AgentResult, error) {
	return nil, nil
}

func (a contextWindowCountingAgent) Stream(ctx context.Context, call fantasy.AgentStreamCall) (*fantasy.AgentResult, error) {
	if call.PrepareStep != nil {
		if _, _, err := call.PrepareStep(ctx, fantasy.PrepareStepFunctionOptions{Model: a.model, Messages: call.Messages}); err != nil {
			return nil, err
		}
	}
	(*a.calls)++
	return &fantasy.AgentResult{Response: fantasy.Response{Content: fantasy.ResponseContent{fantasy.TextContent{Text: "unexpected provider response"}}}}, nil
}

type contextWindowOverflowAgent struct {
	model fantasy.LanguageModel
	calls *int
}

func (a contextWindowOverflowAgent) Generate(context.Context, fantasy.AgentCall) (*fantasy.AgentResult, error) {
	return nil, nil
}

func (a contextWindowOverflowAgent) Stream(ctx context.Context, call fantasy.AgentStreamCall) (*fantasy.AgentResult, error) {
	if call.PrepareStep != nil {
		if _, _, err := call.PrepareStep(ctx, fantasy.PrepareStepFunctionOptions{Model: a.model, Messages: call.Messages}); err != nil {
			return nil, err
		}
	}
	(*a.calls)++
	return nil, fmt.Errorf("context length exceeded: context window 4096")
}

func TestCoordinatorLearnsObservedContextWindowAfterTerminalOverflow(t *testing.T) {
	modelID := "context-window-observed-after-overflow"
	GlobalModelSpecRegistry().RegisterSpec(ModelContextSpec{
		ModelID:            modelID,
		ContextWindow:      8_192,
		MaxOutputTokens:    128,
		SafetyMarginTokens: 32,
	})
	calls := 0
	c := &Coordinator{
		session:      &TeamSession{Workspace: t.TempDir()},
		taskTracker:  NewTaskTracker(),
		reportStatus: func(StatusEvent) {},
	}
	preflight := newCoordinatorRequestPreflight(modelID, "incoming", "system", nil)
	ctx := context.WithValue(context.Background(), modelKey{}, modelID)
	ctx = withCoordinatorRequestPreflight(ctx, preflight)
	_, _, err := c.runAgentWithStatusAndHistory(ctx, contextWindowOverflowAgent{
		model: contextWindowTestModel{modelID: modelID},
		calls: &calls,
	}, "coordinator", "incoming", nil, &taskTiming{})
	if err == nil || !strings.Contains(err.Error(), "context window 4096") {
		t.Fatalf("run error = %v, want original provider overflow", err)
	}
	if calls != 1 {
		t.Fatalf("provider calls = %d, want exactly one terminal call", calls)
	}
	if got := GlobalModelSpecRegistry().GetSpec(modelID); got.ContextWindow != 4_096 {
		t.Fatalf("registered context spec = %+v, want observed window 4096", got)
	}
	if got := preflight.windowValue(); got != 4_096 {
		t.Fatalf("preflight window = %d, want 4096", got)
	}

	freshPreflight := newCoordinatorRequestPreflight(modelID, "fresh incoming", "system", nil)
	if got := freshPreflight.windowValue(); got != 4_096 {
		t.Fatalf("fresh preflight window = %d, want 4096", got)
	}
	manager := NewContextWindowManager(defaultCounter, nil)
	admission, err := manager.Admit(context.Background(), ContextWindowRequest{
		ModelID:              modelID,
		System:               "system",
		Prompt:               "fresh incoming",
		ReservedOutputTokens: 128,
		SafetyMarginTokens:   32,
	})
	if err != nil {
		t.Fatal(err)
	}
	if admission.Budget.Window != 4_096 {
		t.Fatalf("fresh admission window = %d, want 4096", admission.Budget.Window)
	}
}

func TestCoordinatorCannotFitDoesNotEmitProviderRequest(t *testing.T) {
	modelID := "context-window-no-provider-request"
	GlobalModelSpecRegistry().RegisterSpec(ModelContextSpec{
		ModelID:            modelID,
		ContextWindow:      256,
		MaxOutputTokens:    32,
		SafetyMarginTokens: 32,
	})
	calls := 0
	c := &Coordinator{
		session:      &TeamSession{Workspace: t.TempDir()},
		taskTracker:  NewTaskTracker(),
		reportStatus: func(StatusEvent) {},
	}
	preflight := newCoordinatorRequestPreflight(modelID, "incoming", "system", nil)
	ctx := withCoordinatorRequestPreflight(context.Background(), preflight)
	_, _, err := c.runAgentWithStatusAndHistory(ctx, contextWindowCountingAgent{model: contextWindowTestModel{modelID: modelID}, calls: &calls}, "coordinator", strings.Repeat("incoming ", 1_000), nil, &taskTiming{})
	if err == nil || !strings.Contains(err.Error(), "context window admission cannot fit") {
		t.Fatalf("run error = %v, want fail-closed context admission", err)
	}
	if calls != 0 {
		t.Fatalf("provider calls = %d, want 0 while request cannot fit", calls)
	}
}

func TestCoordinatorEmptySystemStillAdmitsBeforeProvider(t *testing.T) {
	modelID := "context-window-empty-system-no-provider"
	GlobalModelSpecRegistry().RegisterSpec(ModelContextSpec{ModelID: modelID, ContextWindow: 256, MaxOutputTokens: 32, SafetyMarginTokens: 32})
	calls := 0
	c := &Coordinator{session: &TeamSession{Workspace: t.TempDir()}, taskTracker: NewTaskTracker(), reportStatus: func(StatusEvent) {}}
	preflight := newCoordinatorRequestPreflight(modelID, "incoming", "", nil)
	if preflight == nil {
		t.Fatal("empty-system coordinator preflight must remain active")
	}
	ctx := withCoordinatorRequestPreflight(context.Background(), preflight)
	_, _, err := c.runAgentWithStatusAndHistory(ctx, contextWindowCountingAgent{model: contextWindowTestModel{modelID: modelID}, calls: &calls}, "coordinator", strings.Repeat("incoming ", 1_000), nil, &taskTiming{})
	if err == nil || !strings.Contains(err.Error(), "context window admission cannot fit") {
		t.Fatalf("run error = %v, want fail-closed context admission", err)
	}
	if calls != 0 {
		t.Fatalf("provider calls = %d, want 0 while empty-system request cannot fit", calls)
	}
}
