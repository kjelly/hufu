package team

import (
	"context"
	"fmt"
	"strings"

	"charm.land/fantasy"
)

func (c *Coordinator) validateHistoricalResultCall(ctx context.Context, manifest ContextInjectionManifest) error {
	if manifest.SchemaVersion != HistoricalResultManifestSchemaVersion {
		return nil
	}
	owner := c.dependencyViewOwner(manifest.TaskID)
	item := owner.todoItemByID(manifest.TaskID)
	if item == nil {
		return fmt.Errorf("historical result dispatch has no admitted task")
	}
	expected, err := owner.expectedHistoricalResultContext(ctx, item)
	if err != nil {
		return err
	}
	if expected == nil {
		return ErrHistoricalResultTaskBinding
	}
	return validateHistoricalResultManifestAgainstSource(manifest, expected)
}

// Each native provider request revalidates sources and protects the complete
// presentations. Existing messages are protected as-is; a continuation that
// no longer carries a source receives one required, fenced copy. The ordinary
// full-request admission counts these messages with actual system and tools.
func protectHistoricalResultMessages(ctx context.Context, messages []fantasy.Message) ([]fantasy.Message, []fantasy.Message, error) {
	state, _ := ctx.Value(dependencyViewCallKey{}).(*dependencyViewCallState)
	if state == nil {
		return messages, nil, nil
	}
	state.mu.Lock()
	manifest := *cloneContextInjectionManifest(state.manifest)
	state.mu.Unlock()
	if manifest.SchemaVersion != HistoricalResultManifestSchemaVersion {
		return messages, nil, nil
	}
	if metadata, ok := invocationMetadataFromContext(ctx); ok && metadata.ParentRequestID != manifest.RequestID {
		return messages, nil, nil
	}
	owner := state.coordinator.dependencyViewOwner(manifest.TaskID)
	item := owner.todoItemByID(manifest.TaskID)
	if item == nil {
		return nil, nil, ErrHistoricalResultTaskBinding
	}
	expected, err := owner.expectedHistoricalResultContext(ctx, item)
	if err != nil {
		return nil, nil, err
	}
	if expected == nil {
		return nil, nil, ErrHistoricalResultTaskBinding
	}
	if err := validateHistoricalResultManifestAgainstSource(manifest, expected); err != nil {
		return nil, nil, err
	}
	prepared, protected := protectHistoricalPresentations(messages, expected)
	return prepared, protected, nil
}

func validateHistoricalResultOutbound(ctx context.Context, prompt string, history []fantasy.Message) error {
	state, _ := ctx.Value(dependencyViewCallKey{}).(*dependencyViewCallState)
	if state == nil {
		return nil
	}
	state.mu.Lock()
	manifest := *cloneContextInjectionManifest(state.manifest)
	state.mu.Unlock()
	if manifest.SchemaVersion != HistoricalResultManifestSchemaVersion {
		return nil
	}
	if metadata, ok := invocationMetadataFromContext(ctx); ok && metadata.ParentRequestID != manifest.RequestID {
		return nil
	}
	owner := state.coordinator.dependencyViewOwner(manifest.TaskID)
	item := owner.todoItemByID(manifest.TaskID)
	if item == nil {
		return ErrHistoricalResultTaskBinding
	}
	expected, err := owner.expectedHistoricalResultContext(ctx, item)
	if err != nil {
		return err
	}
	if expected == nil {
		return ErrHistoricalResultTaskBinding
	}
	if err := validateHistoricalResultManifestAgainstSource(manifest, expected); err != nil {
		return err
	}
	for _, item := range expected.items {
		if !strings.Contains(prompt, item.Content) && historicalPresentationMessageIndex(history, item.Content) < 0 {
			return fmt.Errorf("outbound worker request omitted a required historical presentation")
		}
	}
	return nil
}

func historicalPresentationMessageIndex(messages []fantasy.Message, content string) int {
	for i, message := range messages {
		for _, part := range message.Content {
			text, ok := fantasy.AsMessagePart[fantasy.TextPart](part)
			if ok && strings.Contains(text.Text, content) {
				return i
			}
		}
	}
	return -1
}

func protectHistoricalPresentations(messages []fantasy.Message, expected *historicalResultContext) ([]fantasy.Message, []fantasy.Message) {
	result := cloneMessages(messages)
	protected := make([]fantasy.Message, 0, len(expected.items))
	protectedIndex := make(map[int]bool)
	for _, item := range expected.items {
		if i := historicalPresentationMessageIndex(result, item.Content); i >= 0 {
			if !protectedIndex[i] {
				protected = append(protected, result[i])
				protectedIndex[i] = true
			}
		} else {
			message := fantasy.NewUserMessage(item.Content)
			result = append(result, message)
			protected = append(protected, message)
			protectedIndex[len(result)-1] = true
		}
	}
	return result, protected
}
