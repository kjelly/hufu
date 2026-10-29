package team

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"strings"
	"testing"

	"charm.land/fantasy"
)

func TestHistoricalResultDispatchRequiresCanonicalCallEvidence(t *testing.T) {
	c, item, input := historicalContextFixture(t)
	_, manifest := historicalContextManifest(t, c, item, input)
	if err := c.persistContextManifest(&manifest); err != nil {
		t.Fatal(err)
	}
	if err := c.validateHistoricalResultDelivery(item.ID, 1, manifest.RunID); err == nil {
		t.Fatal("prepared historical request was counted as delivery")
	}
	forged := *cloneContextInjectionManifest(&manifest)
	forged.ModelCalled, forged.Outcome = true, "model_call"
	forged.Fingerprint = historicalResultManifestFingerprint(forged)
	if err := c.taskTracker.TodoList().SetContextManifest(item.ID, &forged); err != nil {
		t.Fatal(err)
	}
	if err := c.validateHistoricalResultDelivery(item.ID, 1, manifest.RunID); err == nil {
		t.Fatal("checkpoint-only historical dispatch was accepted")
	}
	if _, err := markDependencyViewModelCall(c.bindDependencyViewCall(t.Context(), &manifest)); err != nil {
		t.Fatal(err)
	}
	if !manifest.ModelCalled || c.validateHistoricalResultDelivery(item.ID, 1, manifest.RunID) != nil {
		t.Fatal("actual runtime dispatch did not establish canonical delivery")
	}
	if err := c.validateSelectedRepairContext(t.Context(), c.todoItemByID(item.ID), &manifest); err != nil {
		t.Fatalf("result-only repair lost the original dispatch proof: %v", err)
	}
}

func TestHistoricalResultDispatchCancellationAndAuxiliaryIsolation(t *testing.T) {
	c, _, input := historicalContextFixture(t)
	item := c.todoItemByID(input.Request.TaskID)
	_, manifest := historicalContextManifest(t, c, item, input)
	ctx, cancel := context.WithCancel(c.bindDependencyViewCall(t.Context(), &manifest))
	cancel()
	if _, err := markDependencyViewModelCall(ctx); err == nil || manifest.ModelCalled {
		t.Fatal("cancelled request became delivery")
	}
	ctx = c.bindDependencyViewCall(t.Context(), &manifest)
	ctx = withInvocationMetadata(ctx, InvocationMetadata{ParentRequestID: "auxiliary-request", Purpose: "result_repair"})
	if _, err := markDependencyViewModelCall(ctx); err != nil || manifest.ModelCalled {
		t.Fatal("auxiliary call marked its parent as delivered")
	}
	messages := []fantasy.Message{fantasy.NewUserMessage("auxiliary evidence only")}
	prepared, protected, err := protectHistoricalResultMessages(ctx, messages)
	if err != nil || len(protected) != 0 || messageTextSizeAsText(prepared) != messageTextSizeAsText(messages) {
		t.Fatal("auxiliary call inherited historical presentations")
	}
}

func TestHistoricalResultDispatchRevalidatesRequiredSources(t *testing.T) {
	c, item, input := historicalContextFixture(t)
	_, manifest := historicalContextManifest(t, c, item, input)
	reader := c.HistoricalTaskReader()
	c.SetHistoricalTaskReader(historicalTaskReaderFunc(func(ctx context.Context, scope HistoricalTaskScope) ([]HistoricalTaskResult, error) {
		rows, err := reader.Load(ctx, scope)
		if len(rows) > 0 {
			rows = append([]HistoricalTaskResult(nil), rows...)
			rows[0].Validity = "invalidated"
		}
		return rows, err
	}))
	if _, err := markDependencyViewModelCall(c.bindDependencyViewCall(t.Context(), &manifest)); err == nil || manifest.ModelCalled {
		t.Fatal("source invalidated after compilation was dispatched")
	}
}

func TestHistoricalResultWorkerReceivesCompleteContext(t *testing.T) {
	c, item, _ := historicalContextFixture(t)
	worker := scriptWorker(scriptedTurn{stream: func(ctx context.Context, call fantasy.AgentStreamCall) (*fantasy.AgentResult, error) {
		if !strings.Contains(call.Prompt, "complete deliverable") || !strings.Contains(call.Prompt, "9007199254740993") {
			return nil, fmt.Errorf("worker did not receive full typed historical data")
		}
		current := c.todoItemByID(item.ID)
		if len(current.ContextManifests) != 1 || !current.ContextManifests[0].ModelCalled || current.ContextManifests[0].SchemaVersion != HistoricalResultManifestSchemaVersion {
			return nil, fmt.Errorf("actual worker dispatch has no historical manifest")
		}
		response, err := (&submitResultTool{coordinator: c, todoID: item.ID}).Run(ctx, fantasy.ToolCall{Input: `{"status":"success","summary":"historical evidence assessed"}`})
		if err != nil {
			return nil, err
		}
		if response.IsError {
			return nil, fmt.Errorf("historical submission: %s", response.Content)
		}
		return textAgentResult("submitted"), nil
	}})
	c.workerAgentOverride = worker
	if _, err := c.executeTask(t.Context(), taskDefFromTodoItem(item), item.ID); err != nil {
		t.Fatal(err)
	}
	if worker.Calls() != 1 || c.todoItemByID(item.ID).Status != TaskDone {
		t.Fatal("historical dispatch did not complete through the normal acceptance gate")
	}
	raw, err := json.Marshal(SummarizeContextManifests(c.todoItemByID(item.ID).ContextManifests))
	if err != nil || strings.Contains(string(raw), "complete deliverable") {
		t.Fatal("content-free historical summary leaked source bodies")
	}
}

func TestHistoricalResultProviderMessagesSurviveCompaction(t *testing.T) {
	c, item, input := historicalContextFixture(t)
	compiled, manifest := historicalContextManifest(t, c, item, input)
	ctx := c.bindDependencyViewCall(t.Context(), &manifest)
	messages := []fantasy.Message{fantasy.NewUserMessage(strings.Repeat("old history ", 20_000)), fantasy.NewUserMessage(compiled.Prompt)}
	prepared, protected, err := protectHistoricalResultMessages(ctx, messages)
	if err != nil || len(protected) != 1 {
		t.Fatalf("historical protection = %d, %v", len(protected), err)
	}
	modelID := "historical-protected-compaction"
	GlobalModelSpecRegistry().RegisterSpec(ModelContextSpec{ModelID: modelID, ContextWindow: 8192, MaxOutputTokens: 256, SafetyMarginTokens: 64})
	var compacted string
	manager := NewContextWindowManager(defaultCounter, func(_ context.Context, history []fantasy.Message) ([]fantasy.Message, error) {
		compacted = formatMessagesForCompaction(history)
		return []fantasy.Message{fantasy.NewUserMessage(verifiedHistoryPrefix + "verified old history")}, nil
	})
	admitted, err := manager.Admit(ctx, ContextWindowRequest{ModelID: modelID, Messages: prepared, Prompt: compiled.Prompt,
		ProtectedMessages: protected, ReservedOutputTokens: 256, SafetyMarginTokens: 64})
	if err != nil || admitted.Decision == ContextWindowCannotFit {
		t.Fatalf("required historical content was not admitted: %v", err)
	}
	if strings.Contains(compacted, "complete deliverable") || countExactUserMessages(admitted.Messages, compiled.Prompt) != 1 {
		t.Fatal("compaction summarized or duplicated the required historical prompt")
	}
	continuation, continuationProtected, err := protectHistoricalResultMessages(ctx, []fantasy.Message{fantasy.NewUserMessage("continue with the accepted evidence")})
	if err != nil || len(continuationProtected) != 1 || !strings.Contains(messageTextSizeAsText(continuation), "complete deliverable") {
		t.Fatal("continuation lost its required historical presentation")
	}
	if err := validateHistoricalResultOutbound(ctx, "missing context", nil); err == nil {
		t.Fatal("external provider request could omit historical context")
	}
}
