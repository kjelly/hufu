package team

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"testing"

	"charm.land/fantasy"
	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/media"
)

func TestNativeWorkerInputPreservesOneOrderedCurrentTurn(t *testing.T) {
	for _, scenario := range []string{"empty history", "existing text", "existing refs", "retry", "source refs"} {
		t.Run(scenario, func(t *testing.T) {
			c, ctx, item, request := nativeMediaProviderFixture(t)
			var history []fantasy.Message
			prompt := item.Goal
			switch scenario {
			case "existing text":
				history = []fantasy.Message{fantasy.NewUserMessage(prompt)}
			case "existing refs", "retry":
				history = request.Call.Prompt
				if scenario == "retry" {
					prompt = "Continue from the recorded failure evidence. " + prompt
				}
			case "source refs":
				history = cloneMessages(c.conversationHistory)
			}
			before := cloneMessages(history)
			for index := range before {
				before[index].Content = slices.Clone(before[index].Content)
			}
			messages, err := c.nativeWorkerMessages(ctx, item.ID, prompt, history)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(history, before) {
				t.Fatal("native input overwrote caller history or upgraded its source owner in place")
			}
			locations := historyReferences(messages)
			if len(locations) != 2 || nativeWorkerPromptIndex(messages, prompt) < 0 {
				t.Fatal("native input lost ordered images or current user text")
			}
			for index, location := range locations {
				ref := location.Reference
				if ref.TaskID != item.ID || ref.BindingHash != item.MediaBinding.Hash || ref.Source != item.MediaBinding.Selections[index] {
					t.Fatal("native worker used an unbound or reordered image reference")
				}
			}
			wantMessages := 1
			if scenario == "retry" {
				wantMessages = 2
			}
			if len(messages) != wantMessages {
				t.Fatal("native input duplicated the current user turn", len(messages))
			}
			again, err := c.nativeWorkerMessages(ctx, item.ID, prompt, messages)
			if err != nil || !reflect.DeepEqual(again, messages) {
				t.Fatal("rebuilding a native input duplicated its prompt or image occurrences", err)
			}
		})
	}
}

func TestNativeWorkerRetryPromptIsProtectedWithoutMovingImageTurn(t *testing.T) {
	c, ctx, item, request := nativeMediaProviderFixture(t)
	initial := request.Call.Prompt
	if nativeWorkerAdmissionPrompt(item.Goal, initial) != "" {
		t.Fatal("initial image-bearing prompt would be duplicated or relocated")
	}
	history := append(cloneMessages(initial), fantasy.Message{Role: fantasy.MessageRoleAssistant,
		Content: []fantasy.MessagePart{fantasy.TextPart{Text: "Prior failed attempt."}}})
	retry := "CURRENT_NATIVE_RETRY_INSTRUCTION"
	messages, err := c.nativeWorkerMessages(ctx, item.ID, retry, history)
	if err != nil {
		t.Fatal(err)
	}
	prior, _, current, _ := splitRequestForCompaction(ContextWindowRequest{
		Messages: messages, Prompt: nativeWorkerAdmissionPrompt(retry, messages),
	})
	if current != retry {
		t.Fatal("the current native retry instruction was left in compactable history")
	}
	if len(prior) != len(history) || !reflect.DeepEqual(prior[0], initial[0]) || len(historyReferences(prior)) != len(item.MediaBinding.Refs) {
		t.Fatal("protecting retry text moved or duplicated the original image turn")
	}
}

func TestNativeWorkerInputRejectsInvalidHistoryOrLease(t *testing.T) {
	for _, scenario := range []string{"stale lease", "cancelled", "wrong owner", "missing image", "raw media"} {
		t.Run(scenario, func(t *testing.T) {
			c, ctx, item, request := nativeMediaProviderFixture(t)
			history := request.Call.Prompt
			switch scenario {
			case "stale lease":
				identity := ctx.Value(submitResultRuntimeIdentityKey{}).(submitResultRuntimeIdentity)
				identity.OccurrenceRevision++
				ctx = withSubmitResultRuntimeIdentity(ctx, identity)
			case "cancelled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			case "wrong owner":
				part := history[0].Content[1].(agent.MediaReferencePart)
				part.Reference.TaskID = "other"
				history[0].Content[1] = part
			case "missing image":
				history[0].Content = history[0].Content[:2]
			case "raw media":
				history[0].Content[1] = fantasy.FilePart{MediaType: "image/png", Data: []byte("untrusted")}
			}
			_, err := c.nativeWorkerMessages(ctx, item.ID, item.Goal, history)
			if scenario == "cancelled" {
				if !errors.Is(err, context.Canceled) {
					t.Fatal("native input lost its cancellation", err)
				}
			} else if _, ok := errors.AsType[*media.Error](err); !ok {
				t.Fatal("invalid native input was accepted", err)
			}
		})
	}
}

func TestNativeWorkerStreamUsesCompleteMessagesAndImageBudget(t *testing.T) {
	c, ctx, item, request := nativeMediaProviderFixture(t)
	messages, err := c.nativeWorkerMessages(ctx, item.ID, item.Goal, nil)
	if err != nil {
		t.Fatal(err)
	}
	descriptor := contextWindowRequestDescriptor{ModelID: request.ModelID, System: "Complete worker system instructions.",
		AdmissionContext: request.AdmissionContext, ModelContext: modelContextSpecForProviderRequest(request), Window: request.AdmissionContext.ContextWindow,
		ReservedOutputTokens: request.AdmissionContext.MaxOutputTokens, SafetyMarginTokens: request.AdmissionContext.SafetyMarginTokens, Owner: item.Agent, Scope: "worker"}
	ctx = context.WithValue(withNativeProviderCounter(ctx, c), nativeWorkerInputKey{}, true)
	ctx = context.WithValue(ctx, todoIDKey{}, item.ID)
	ctx = withContextWindowRequestDescriptor(ctx, descriptor)
	worker := scriptWorker(scriptedTurn{stream: func(callCtx context.Context, call fantasy.AgentStreamCall) (*fantasy.AgentResult, error) {
		if call.Prompt != "" || len(call.Files) != 0 || len(historyReferences(call.Messages)) != 2 || len(call.Messages) != 1 {
			t.Fatal("native worker duplicated its prompt or omitted its complete reference input")
		}
		preparedCtx, prepared, err := call.PrepareStep(callCtx, fantasy.PrepareStepFunctionOptions{Model: contextWindowTestModel{modelID: "vision"}, Messages: call.Messages})
		if err != nil {
			return nil, err
		}
		input := call.Messages
		if prepared.Messages != nil {
			input = prepared.Messages
		}
		count, err := nativeWorkerStepTokens(preparedCtx, descriptor, input)
		if err != nil || count <= 2*8192 || len(historyReferences(input)) != 2 {
			t.Fatal("PrepareStep lost image upper bounds or removed its references", count, err)
		}
		return textAgentResult("fixture result"), nil
	}})
	timing := &taskTiming{}
	timing.reset()
	if _, _, err := c.runAgentWithStatusAndHistory(ctx, worker, item.Agent, item.Goal, messages, timing); err != nil {
		t.Fatal(err)
	}
}

func TestNativeWorkerResultRepairUsesTextViewWithoutChangingHistory(t *testing.T) {
	c, ctx, item, request := nativeMediaProviderFixture(t)
	history := request.Call.Prompt
	before := cloneMessages(history)
	ctx = context.WithValue(ctx, nativeWorkerInputKey{}, true)
	ctx = context.WithValue(ctx, protocolRepairExecutionKey{}, true)
	const prompt = "Restate the existing result through submit_result."
	worker := scriptWorker(scriptedTurn{stream: func(_ context.Context, call fantasy.AgentStreamCall) (*fantasy.AgentResult, error) {
		if call.Prompt != prompt || agent.MessagesHaveMedia(call.Messages) || nativeWorkerInput(ctx) {
			t.Fatal("result-only repair inherited native analysis input")
		}
		return textAgentResult("repaired fixture result"), nil
	}})
	timing := &taskTiming{}
	timing.reset()
	if _, _, err := c.runAgentWithStatusAndHistory(ctx, worker, item.Agent, prompt, history, timing); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, []fantasy.Message(history)) || len(historyReferences(history)) != 2 {
		t.Fatal("result-only text view erased the original image history")
	}
}
