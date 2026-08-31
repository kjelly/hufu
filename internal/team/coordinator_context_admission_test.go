package team

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"charm.land/fantasy"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/config"
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

func TestDownshiftTelemetryOccursOnlyAfterLanguageModelSuccess(t *testing.T) {
	strongID := "coordinator-downshift-strong"
	weakID := "coordinator-downshift-weak"
	GlobalModelSpecRegistry().RegisterSpec(ModelContextSpec{
		ModelID: strongID, ContextWindow: 256, MaxOutputTokens: 32, SafetyMarginTokens: 32,
	})
	GlobalModelSpecRegistry().RegisterSpec(ModelContextSpec{
		ModelID: weakID, ContextWindow: 32_768, MaxOutputTokens: 32, SafetyMarginTokens: 32,
	})

	providerCalls := 0
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/chat/completions" {
			t.Errorf("provider request = %s %s, want POST /v1/chat/completions", r.Method, r.URL.Path)
		}
		providerCalls++
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "data: {\"id\":\"downshift\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"weak\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"accepted\"},\"finish_reason\":null}]}\n\n")
		_, _ = fmt.Fprint(w, "data: {\"id\":\"downshift\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"weak\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	})
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Skipf("network listener unavailable in this environment: %v", err)
	}
	provider := httptest.NewUnstartedServer(handler)
	provider.Listener = listener
	provider.Start()
	defer provider.Close()

	providerManager, err := agent.NewProviderManager(provider.URL+"/v1", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	strongModel := &transientProjectionCaptureModel{modelID: strongID}
	ag := fantasy.NewAgent(
		strongModel,
		fantasy.WithSystemPrompt("system"),
		fantasy.WithMaxOutputTokens(32),
		fantasy.WithMaxRetries(0),
	)
	history := make([]fantasy.Message, 0, 20)
	for i := 0; i < 20; i++ {
		history = append(history, fantasy.NewUserMessage(fmt.Sprintf("historical message %d %s", i, strings.Repeat("evidence ", 100))))
	}
	c := &Coordinator{
		providerManager: providerManager,
		modelList:       []config.ModelEntry{{ID: weakID}, {ID: strongID}},
		session: &TeamSession{Workspace: t.TempDir(), Config: agent.TeamConfig{
			Generation: agent.GenerationParams{Model: strongID},
		}},
		taskTracker: NewTaskTracker(), reportStatus: func(StatusEvent) {},
	}
	preflight := newCoordinatorRequestPreflight(strongID, "incoming", "system", nil)
	ctx := context.WithValue(context.Background(), modelKey{}, strongID)
	ctx = withCoordinatorRequestPreflight(ctx, preflight)
	result, _, err := c.runAgentWithStatusAndHistory(ctx, ag, "coordinator", "incoming", history, &taskTiming{})
	if err != nil {
		t.Fatalf("downshifted coordinator run error = %v", err)
	}
	if result != "accepted" {
		t.Fatalf("downshifted coordinator result = %q, want accepted", result)
	}
	if len(strongModel.calls) != 0 {
		t.Fatalf("strong model calls = %d, want 0 after pre-provider CannotFit", len(strongModel.calls))
	}
	if providerCalls != 1 {
		t.Fatalf("provider calls = %d, want exactly one weak-model request", providerCalls)
	}
}

func TestCoordinatorModelContinuationPersistenceFailurePrecedesDownshiftTelemetry(t *testing.T) {
	strongID := "coordinator-downshift-persist-strong"
	weakID := "coordinator-downshift-persist-weak"
	GlobalModelSpecRegistry().RegisterSpec(ModelContextSpec{
		ModelID: strongID, ContextWindow: 256, MaxOutputTokens: 32, SafetyMarginTokens: 32,
	})
	GlobalModelSpecRegistry().RegisterSpec(ModelContextSpec{
		ModelID: weakID, ContextWindow: 32_768, MaxOutputTokens: 32, SafetyMarginTokens: 32,
	})

	providerCalls := 0
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		providerCalls++
		w.WriteHeader(http.StatusInternalServerError)
	})
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Skipf("network listener unavailable in this environment: %v", err)
	}
	provider := httptest.NewUnstartedServer(handler)
	provider.Listener = listener
	provider.Start()
	defer provider.Close()
	providerManager, err := agent.NewProviderManager(provider.URL+"/v1", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	workspace := t.TempDir()
	store, err := NewEventStore(workspace, "run-downshift-failure", "session-downshift-failure")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	syncCalls := 0
	store.syncFile = func() error {
		syncCalls++
		if syncCalls == 2 {
			return errors.New("injected continuation admission sync failure")
		}
		return nil
	}
	c := &Coordinator{
		providerManager: providerManager,
		modelList:       []config.ModelEntry{{ID: weakID}, {ID: strongID}},
		session: &TeamSession{Workspace: workspace, Config: agent.TeamConfig{
			Generation: agent.GenerationParams{Model: strongID},
		}},
		eventStore:   store,
		taskTracker:  NewTaskTracker(),
		reportStatus: func(StatusEvent) {},
	}
	preflight := newCoordinatorRequestPreflight(strongID, "incoming", "system", nil)
	continuation, err := c.admitCoordinatorEarlierModel(context.Background(), preflight, []fantasy.Message{fantasy.NewUserMessage("small")}, "incoming", 0, 32, strongID)
	if err == nil || !strings.Contains(err.Error(), "persist coordinator model continuation admission") {
		t.Fatalf("continuation persistence error = %v, want primary provenance error", err)
	}
	if continuation.Model != nil {
		t.Fatal("continuation model was returned after persistence failure")
	}
	if providerCalls != 0 {
		t.Fatalf("provider calls = %d, want 0 after continuation persistence failure", providerCalls)
	}
	data, readErr := os.ReadFile(filepath.Join(workspace, logsDir, eventStoreFile))
	if readErr != nil {
		t.Fatal(readErr)
	}
	if strings.Contains(string(data), string(EventContextWindowDownshift)) {
		t.Fatal("downshift telemetry was persisted after continuation persistence failure")
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
