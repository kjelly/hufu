package team

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"charm.land/fantasy"
)

type transientProjectionTestTool struct{}

func (transientProjectionTestTool) Info() fantasy.ToolInfo {
	return fantasy.ToolInfo{
		Name:        "inspect",
		Description: "Inspect the requested subject.",
		Parameters:  map[string]any{"subject": map[string]any{"type": "string"}},
		Required:    []string{"subject"},
	}
}

func (transientProjectionTestTool) Run(context.Context, fantasy.ToolCall) (fantasy.ToolResponse, error) {
	return fantasy.NewTextResponse("unused"), nil
}

func (transientProjectionTestTool) ProviderOptions() fantasy.ProviderOptions {
	return nil
}

func (transientProjectionTestTool) SetProviderOptions(fantasy.ProviderOptions) {}

type transientProjectionCaptureModel struct {
	fantasy.LanguageModel
	modelID string
	calls   []fantasy.Call
}

func (m *transientProjectionCaptureModel) Model() string  { return m.modelID }
func (*transientProjectionCaptureModel) Provider() string { return "transient-projection-provider" }

func (m *transientProjectionCaptureModel) Stream(_ context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
	m.calls = append(m.calls, call)
	return func(yield func(fantasy.StreamPart) bool) {
		if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextStart, ID: "text-1"}) {
			return
		}
		if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextDelta, ID: "text-1", Delta: "accepted"}) {
			return
		}
		if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextEnd, ID: "text-1"}) {
			return
		}
		yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonStop})
	}, nil
}

func messageText(message fantasy.Message) string {
	var text strings.Builder
	for _, part := range message.Content {
		if content, ok := fantasy.AsMessagePart[fantasy.TextPart](part); ok {
			text.WriteString(content.Text)
		}
	}
	return text.String()
}

func TestCoordinatorContextAdmissionUsesTransientProjectionWithRealFantasyAgent(t *testing.T) {
	modelID := "context-window-transient-fantasy-agent"
	GlobalModelSpecRegistry().RegisterSpec(ModelContextSpec{
		ModelID:            modelID,
		ContextWindow:      8_192,
		MaxOutputTokens:    128,
		SafetyMarginTokens: 32,
	})
	workspace := t.TempDir()
	committedSummary := &StructuredSummary{Goal: "committed summary"}
	c := &Coordinator{
		session:      &TeamSession{Workspace: workspace},
		taskTracker:  NewTaskTracker(),
		reportStatus: func(StatusEvent) {},
		conversationHistory: []fantasy.Message{
			fantasy.NewUserMessage("committed history"),
		},
		conversationHistorySourceCounts: []int{7},
		conversationHistorySourceOffset: 13,
		lastCompactionSummary:           committedSummary,
		initialPrompt:                   "initial coordinator goal",
	}
	priorHistory := append([]fantasy.Message(nil), c.conversationHistory...)
	priorSourceCounts := append([]int(nil), c.conversationHistorySourceCounts...)
	priorSummary := cloneStructuredSummary(c.lastCompactionSummary)
	if err := SaveCompactionRecord(workspace, CompactionRecord{
		ID:          "committed-record",
		Summary:     *committedSummary,
		SourceRange: CompactionRange{StartIndex: 1, EndIndex: 1, MsgCount: 1},
	}); err != nil {
		t.Fatal(err)
	}
	priorRecords, err := LoadCompactionHistory(workspace)
	if err != nil {
		t.Fatal(err)
	}

	system := "coordinator system instructions"
	requiredTool := fantasy.NewAgentTool("finish", strings.Repeat("required finish guidance ", 10), func(context.Context, map[string]any, fantasy.ToolCall) (fantasy.ToolResponse, error) {
		return fantasy.NewTextResponse("unused"), nil
	})
	optionalTool := fantasy.NewAgentTool("inspect", strings.Repeat("optional inspection guidance ", 900), func(context.Context, map[string]any, fantasy.ToolCall) (fantasy.ToolResponse, error) {
		return fantasy.NewTextResponse("unused"), nil
	})
	tools := []fantasy.AgentTool{requiredTool, optionalTool}
	preflight := newCoordinatorRequestPreflight(modelID, "incoming request", system, tools)
	model := &transientProjectionCaptureModel{modelID: modelID}
	agent := fantasy.NewAgent(
		model,
		fantasy.WithSystemPrompt(system),
		fantasy.WithTools(tools...),
		fantasy.WithMaxOutputTokens(128),
		fantasy.WithMaxRetries(0),
	)
	history := []fantasy.Message{
		fantasy.NewUserMessage("historical goal"),
		{Role: fantasy.MessageRoleAssistant, Content: []fantasy.MessagePart{
			fantasy.ToolCallPart{ToolCallID: "call-1", ToolName: "inspect", Input: `{"subject":"history"}`},
		}},
		{Role: fantasy.MessageRoleTool, Content: []fantasy.MessagePart{
			fantasy.ToolResultPart{ToolCallID: "call-1", Output: fantasy.ToolResultOutputContentText{Text: strings.Repeat("historical observation ", 3_000)}},
		}},
	}
	ctx := withCoordinatorRequestPreflight(context.Background(), preflight)
	if _, _, err := c.runAgentWithStatusAndHistory(ctx, agent, "coordinator", "incoming request", history, &taskTiming{}); err != nil {
		t.Fatal(err)
	}

	if len(model.calls) != 1 {
		t.Fatalf("provider Stream calls = %d, want exactly one", len(model.calls))
	}
	call := model.calls[0]
	if len(call.Prompt) == 0 {
		t.Fatal("provider received an empty prompt")
	}
	if !toolPairsIntact(call.Prompt) {
		t.Fatal("provider received an invalid tool-call/result projection")
	}
	if got := countExactUserMessages(call.Prompt, "incoming request"); got != 1 {
		t.Fatalf("incoming prompt occurrences = %d, want exactly one", got)
	}
	if !strings.Contains(messageText(call.Prompt[0]), system) {
		t.Fatalf("provider system prompt does not contain full system instructions: %q", messageText(call.Prompt[0]))
	}
	if len(call.Tools) != 1 || call.Tools[0].GetName() != "finish" {
		t.Fatalf("provider tools = %#v, want only the required tool after optional projection", call.Tools)
	}
	if !containsVerifiedHistory(call.Prompt) {
		t.Fatal("provider prompt did not use the verified transient history projection")
	}
	actualToolSet := make([]fantasy.AgentTool, 0, len(call.Tools))
	for _, providerTool := range call.Tools {
		for _, availableTool := range tools {
			if providerTool.GetName() == availableTool.Info().Name {
				actualToolSet = append(actualToolSet, availableTool)
				break
			}
		}
	}
	messageTokens, err := defaultCounter.CountMessages(context.Background(), modelID, call.Prompt)
	if err != nil {
		t.Fatal(err)
	}
	toolTokens, err := defaultCounter.CountTools(context.Background(), modelID, actualToolSet)
	if err != nil {
		t.Fatal(err)
	}
	budget := CalculateContextBudget(GlobalModelSpecRegistry().GetSpec(modelID), 0, 0)
	if got := messageTokens + toolTokens; got > budget.Available {
		t.Fatalf("actual provider payload uses %d tokens, available budget is %d", got, budget.Available)
	}
	if !reflect.DeepEqual(c.conversationHistory, priorHistory) {
		t.Fatal("transient admission mutated coordinator conversation history")
	}
	if !reflect.DeepEqual(c.conversationHistorySourceCounts, priorSourceCounts) || c.conversationHistorySourceOffset != 13 {
		t.Fatal("transient admission mutated conversation source metadata")
	}
	if !reflect.DeepEqual(c.lastCompactionSummary, priorSummary) {
		t.Fatal("transient admission mutated the last durable compaction summary")
	}
	if got := c.Metrics().Compactions; got != 0 {
		t.Fatalf("transient admission compaction metric = %d, want 0", got)
	}
	gotRecords, err := LoadCompactionHistory(workspace)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(gotRecords, priorRecords) {
		t.Fatal("transient admission mutated durable compaction records")
	}
}

func countExactUserMessages(messages []fantasy.Message, want string) int {
	count := 0
	for _, message := range messages {
		if message.Role == fantasy.MessageRoleUser && messageText(message) == want {
			count++
		}
	}
	return count
}
