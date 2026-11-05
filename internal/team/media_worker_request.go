package team

import (
	"context"
	"slices"

	"charm.land/fantasy"
	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/media"
)

type nativeWorkerInputKey struct{}

func (c *Coordinator) prepareNativeWorkerRequest(ctx context.Context, request *AttemptRequest, task TaskDef, def *agent.AgentDef, tools []fantasy.AgentTool, bound agent.ProviderAdmissionContext) (context.Context, error) {
	if !taskHasMedia(task) || protocolRepairExecution(ctx) {
		return ctx, nil
	}
	history, err := c.nativeWorkerMessages(ctx, request.TaskID, request.Prompt, request.History)
	if err != nil {
		return ctx, err
	}
	request.History = history
	ctx = context.WithValue(withNativeProviderCounter(ctx, c), nativeWorkerInputKey{}, true)
	preflight := providerCallFromContextRequest(request.ModelID, def.System, "", history, tools)
	preflight.AdmissionContext = bound
	if _, err := c.resolveNativeProviderMedia(ctx, preflight); err != nil {
		return ctx, err
	}
	return ctx, admitProviderRequestCapacity(ctx, preflight)
}

func nativeWorkerInput(ctx context.Context) bool {
	value, _ := ctx.Value(nativeWorkerInputKey{}).(bool)
	return value && !protocolRepairExecution(ctx)
}

// An initial image-bearing user turn stays at its original position and is
// protected by media compaction. A later plain retry instruction uses ordinary
// current-prompt protection; it is still not appended through SDK Prompt.
func nativeWorkerAdmissionPrompt(prompt string, history []fantasy.Message) string {
	index := nativeWorkerPromptIndex(history, prompt)
	if index < 0 || len(history[index].Content) != 1 {
		return ""
	}
	if _, ok := fantasy.AsMessagePart[fantasy.TextPart](history[index].Content[0]); !ok {
		return ""
	}
	return prompt
}

func (c *Coordinator) validateLocalMediaAttempt(ctx context.Context, request AttemptRequest) error {
	item := c.todoItemByID(request.TaskID)
	if !taskHasMedia(request.Task) && (item == nil || item.MediaBinding == nil && item.MediaInputDigest == "") {
		return nil
	}
	if c.executionPolicy == nil {
		return media.Reject(media.DurabilityRequired, "native worker has no frozen execution policy")
	}
	if err := validateNativeMediaInvocation(item, c.executionPolicy.snapshot); err != nil {
		return err
	}
	if protocolRepairExecution(ctx) {
		// Result-only collection has its own candidate/lease checks. It must
		// use the explicit text view and can never acquire an image read grant.
		return nil
	}
	return c.validateMediaAttempt(ctx, request.Task, request.TaskID)
}

func nativeWorkerStepTokens(ctx context.Context, descriptor contextWindowRequestDescriptor, messages []fantasy.Message) (int64, error) {
	request := providerCallFromContextRequest(descriptor.ModelID, descriptor.System, "", messages, descriptor.Tools)
	request.AdmissionContext = descriptor.AdmissionContext
	count, err := defaultCounter.CountProviderRequest(ctx, descriptor.ModelID, request)
	return int64(count), err
}

// mediaTextResultView is an explicit non-visual view for result-only work.
// It reveals no image IDs, dimensions or content and never mutates history.
func mediaTextResultView(history []fantasy.Message) ([]fantasy.Message, error) {
	if _, err := agent.ValidatePersistentMessages(history); err != nil {
		return nil, err
	}
	messages := cloneMessages(history)
	for index := range messages {
		var parts []fantasy.MessagePart
		for _, part := range messages[index].Content {
			switch part.(type) {
			case agent.MediaReferencePart, *agent.MediaReferencePart:
			default:
				parts = append(parts, part)
			}
		}
		if len(parts) == 0 && len(messages[index].Content) > 0 {
			parts = []fantasy.MessagePart{fantasy.TextPart{Text: "Prior media input is omitted from this text-only result view."}}
		}
		messages[index].Content = parts
	}
	return messages, nil
}

func (c *Coordinator) nativeWorkerMessages(ctx context.Context, taskID, prompt string, history []fantasy.Message) ([]fantasy.Message, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	item := c.todoItemByID(taskID)
	if item == nil || item.MediaBinding == nil {
		return nil, media.Reject(media.AccessDenied, "native worker has no canonical image input")
	}
	if err := c.validateMediaAttempt(ctx, taskDefFromTodoItem(item), taskID); err != nil {
		return nil, err
	}
	if err := validateNativeMediaInvocation(item, c.executionPolicy.snapshot); err != nil {
		return nil, err
	}
	policy, err := c.requiredMediaPolicy()
	if err != nil {
		return nil, err
	}
	return nativeWorkerInputMessages(item.MediaBinding, prompt, history, policy.Policy.Image)
}

// nativeWorkerInputMessages is a reference-only projection. The current turn
// appears once, retries preserve the same ordered selections, and caller
// history is never overwritten with this task's owner or hydrated bytes.
func nativeWorkerInputMessages(binding *media.TaskBinding, prompt string, history []fantasy.Message, limits media.Limits) ([]fantasy.Message, error) {
	if binding == nil {
		return nil, media.Reject(media.AccessDenied, "native worker input has no canonical binding")
	}
	if err := binding.Validate(limits); err != nil {
		return nil, err
	}
	if _, err := agent.ValidatePersistentMessages(history); err != nil {
		return nil, err
	}
	messages := cloneMessages(history)
	for index := range messages {
		messages[index].Content = slices.Clone(messages[index].Content)
	}
	locations := historyReferences(messages)
	if len(locations) == 0 {
		index := nativeWorkerPromptIndex(messages, prompt)
		if index < 0 {
			messages = append(messages, fantasy.NewUserMessage(prompt))
			index = len(messages) - 1
		}
		for position, selection := range binding.Selections {
			messages[index].Content = append(messages[index].Content, nativeWorkerReference(binding, position, selection))
		}
	} else {
		if err := validateNativeProviderReferences(messages, locations, binding, limits); err != nil {
			return nil, err
		}
		// An admitted turn's unbound source references can become this task's
		// private input only after the canonical selection above matched.
		for index, location := range locations {
			messages[location.Message].Content[location.Part] = nativeWorkerReference(binding, index, binding.Selections[index])
		}
		if prompt != "" && nativeWorkerPromptIndex(messages, prompt) < 0 {
			messages = append(messages, fantasy.NewUserMessage(prompt))
		}
	}
	if err := validateNativeProviderReferences(messages, historyReferences(messages), binding, limits); err != nil {
		return nil, err
	}
	return messages, nil
}

func nativeWorkerReference(binding *media.TaskBinding, index int, selection media.Selection) agent.MediaReferencePart {
	return agent.MediaReferencePart{Reference: media.HistoryReference{Version: media.Version,
		Source: selection, Ref: binding.Refs[index], PolicyHash: binding.PolicyHash, TaskID: binding.TaskID, BindingHash: binding.Hash}}
}

func nativeWorkerPromptIndex(messages []fantasy.Message, prompt string) int {
	for index := len(messages) - 1; index >= 0; index-- {
		message := messages[index]
		if message.Role != fantasy.MessageRoleUser {
			continue
		}
		textCount, found := 0, false
		for _, part := range message.Content {
			if text, ok := fantasy.AsMessagePart[fantasy.TextPart](part); ok {
				textCount++
				found = text.Text == prompt
				continue
			}
			switch part.(type) {
			case agent.MediaReferencePart, *agent.MediaReferencePart:
			default:
				textCount = 2
			}
		}
		if textCount == 1 && found {
			return index
		}
	}
	return -1
}
