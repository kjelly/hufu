package team

import (
	"bytes"
	"errors"
	"reflect"
	"testing"

	"charm.land/fantasy"
	"github.com/kjelly/hufu/internal/runtimestore"
)

func historyToolCall(id, input string) fantasy.Message {
	return fantasy.Message{Role: fantasy.MessageRoleAssistant, Content: []fantasy.MessagePart{fantasy.ToolCallPart{ToolCallID: id, ToolName: "inspect", Input: input}}}
}

func historyToolResult(id, text string) fantasy.Message {
	return fantasy.Message{Role: fantasy.MessageRoleTool, Content: []fantasy.MessagePart{fantasy.ToolResultPart{ToolCallID: id, Output: fantasy.ToolResultOutputContentText{Text: text}}}}
}

func TestHistoryToolPairProjectionPreservesCompletedExchanges(t *testing.T) {
	for _, name := range []string{"serial", "parallel", "alias_collision", "pending_collision", "orphan", "duplicate_result", "missing_result", "same_message"} {
		t.Run(name, func(t *testing.T) {
			history := []fantasy.Message{historyToolCall("recycled", `{"subject":"first"}`), historyToolResult("recycled", "original observation"), historyToolCall("recycled", `{"subject":"second"}`), historyToolResult("recycled", "second observation")}
			wantReason := ""
			switch name {
			case "parallel":
				history = []fantasy.Message{historyToolCall("recycled", "first"), historyToolCall("other", "other first"), historyToolResult("other", "other observation"), historyToolResult("recycled", "first observation"), historyToolCall("other", "other second"), historyToolCall("recycled", "second"), historyToolResult("recycled", "second observation"), historyToolResult("other", "other second observation")}
			case "alias_collision":
				history = append(history, historyToolCall("hufu_history_0", "reserved"), historyToolResult("hufu_history_0", "reserved observation"))
			case "pending_collision":
				history = []fantasy.Message{history[0], history[2], history[1]}
				wantReason = "duplicate_call_id"
			case "orphan":
				history = append(history, historyToolResult("unknown", "orphan"))
				wantReason = "result_without_call"
			case "duplicate_result":
				history = append(history, history[3])
				wantReason = "duplicate_result_id"
			case "missing_result":
				history = history[:3]
				wantReason = "missing_result"
			case "same_message":
				history = []fantasy.Message{{Role: fantasy.MessageRoleAssistant, Content: append(append(history[0].Content, history[1].Content...), history[2].Content...)}}
				wantReason = "duplicate_call_id"
			}
			before := digestMessages(history)
			projected, err := projectHistoryToolPairs(history)
			if digestMessages(history) != before {
				t.Fatal("projection changed source history")
			}
			if wantReason != "" {
				detail, ok := errors.AsType[*toolPairingError](err)
				if !ok || detail.Reason != wantReason || !reflect.DeepEqual(projected, history) {
					t.Fatalf("invalid exchange was repaired or misdiagnosed: %v", err)
				}
				return
			}
			if err != nil || !toolPairsIntact(projected) || len(projected) != len(history) {
				t.Fatalf("closed exchanges rejected: %v", err)
			}
			for i, message := range history {
				for j, part := range message.Content {
					if call, ok := fantasy.AsMessagePart[fantasy.ToolCallPart](part); ok {
						copy, _ := fantasy.AsMessagePart[fantasy.ToolCallPart](projected[i].Content[j])
						copy.ToolCallID = call.ToolCallID
						if !reflect.DeepEqual(call, copy) {
							t.Fatal("projection changed a tool or its original arguments")
						}
					}
					if result, ok := fantasy.AsMessagePart[fantasy.ToolResultPart](part); ok {
						copy, _ := fantasy.AsMessagePart[fantasy.ToolResultPart](projected[i].Content[j])
						copy.ToolCallID = result.ToolCallID
						if !reflect.DeepEqual(result, copy) {
							t.Fatal("projection changed a tool result")
						}
					}
				}
			}
			again, err := projectHistoryToolPairs(projected)
			if err != nil || !reflect.DeepEqual(again, projected) {
				t.Fatal("projection is not idempotent")
			}
		})
	}
}

func TestCoordinatorAdmitsCompletedHistoryWithRecycledToolIDs(t *testing.T) {
	c := newTestCoordinator(t)
	const modelID = "recycled-history-coordinator"
	GlobalModelSpecRegistry().RegisterSpec(ModelContextSpec{ModelID: modelID, ContextWindow: 8192, MaxOutputTokens: 128, SafetyMarginTokens: 32})
	model := &transientProjectionCaptureModel{modelID: modelID}
	ag := fantasy.NewAgent(model, fantasy.WithSystemPrompt("coordinator"), fantasy.WithMaxOutputTokens(128), fantasy.WithMaxRetries(0))
	history := []fantasy.Message{fantasy.NewUserMessage("original request"), historyToolCall("recycled", "first"), historyToolResult("recycled", "original observation"), historyToolCall("recycled", "second"), historyToolResult("recycled", "second observation")}
	before := digestMessages(history)
	ctx := withCoordinatorRequestPreflight(t.Context(), newCoordinatorRequestPreflight(modelID, "finish", "coordinator", nil))
	_, _, err := c.runAgentWithStatusAndHistory(ctx, ag, "coordinator", "finish", history, &taskTiming{})
	if err != nil || len(model.calls) != 1 {
		t.Fatalf("coordinator could not continue: %v, calls=%d", err, len(model.calls))
	}
	if !toolPairsIntact(model.calls[0].Prompt) || digestMessages(history) != before {
		t.Fatal("provider saw invalid pairs or projection rewrote the original history")
	}
}

func TestConversationCheckpointDisambiguatesNewStreamIDs(t *testing.T) {
	c := newTestCoordinator(t)
	c.conversationHistory = []fantasy.Message{fantasy.NewUserMessage("goal"), historyToolCall("recycled", "first"), historyToolResult("recycled", "original observation")}
	before := digestMessages(c.conversationHistory)
	steps := []fantasy.StepResult{{Messages: []fantasy.Message{historyToolCall("recycled", "second"), historyToolResult("recycled", "new observation")}}}
	stepBefore := digestMessages(steps[0].Messages)
	c.appendHistory(t.Context(), steps)
	if !toolPairsIntact(c.conversationHistory) || digestMessages(c.conversationHistory[:3]) != before || digestMessages(steps[0].Messages) != stepBefore {
		t.Fatal("checkpoint rewrote prior history, input steps or retained colliding IDs")
	}
	if err := SaveConversationHistory(c.session.Workspace, c.conversationHistory); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(LoadConversationHistory(c.session.Workspace), c.conversationHistory) {
		t.Fatal("new stream aliases did not survive restart")
	}
}

func TestLegacyHistoryProjectionPreservesStoredSource(t *testing.T) {
	workspace := t.TempDir()
	history := []fantasy.Message{fantasy.NewUserMessage("original request"), historyToolCall("recycled", "first"), historyToolResult("recycled", "original observation"), historyToolCall("recycled", "second"), historyToolResult("recycled", "second observation")}
	if err := SaveConversationHistory(workspace, history); err != nil {
		t.Fatal(err)
	}
	before, _ := getRuntimeDocument(t, workspace, runtimestore.DocumentSessionHistory)
	for range 2 {
		loaded := LoadConversationHistory(workspace)
		if !toolPairsIntact(loaded) || len(loaded) != len(history) {
			t.Fatal("legacy history could not be restored without deleting messages")
		}
		if err := validateResumeConversation(workspace, "main"); err != nil {
			t.Fatalf("resume preflight refused closed exchanges: %v", err)
		}
	}
	after, _ := getRuntimeDocument(t, workspace, runtimestore.DocumentSessionHistory)
	if !bytes.Equal(before, after) {
		t.Fatal("read-only history projection rewrote its stored source")
	}
}
