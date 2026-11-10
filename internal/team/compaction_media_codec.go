package team

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"reflect"
	"strings"

	"charm.land/fantasy"
	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/media"
)

const mediaCompactionStateVersion = 2
const toolMediaCompactionStateVersion = 3
const nativeInputGenerationKind = "native_input"
const nativeResetGenerationKind = "context_reset"

func validatePersistentHistories(histories ...[]fantasy.Message) error {
	for _, history := range histories {
		if _, err := agent.ValidatePersistentMessages(history); err != nil {
			return err
		}
	}
	return nil
}

// The default marshal representation is unchanged. Only decoding message
// arrays needs an adapter, because the SDK cannot restore Hufu reference parts.
func (c *ConversationCompactionCheckpoint) UnmarshalJSON(data []byte) error {
	type plain ConversationCompactionCheckpoint
	var decoded plain
	wire := struct {
		*plain
		History jsontext.Value `json:"history"`
	}{plain: &decoded}
	if err := json.Unmarshal(data, &wire, json.RejectUnknownMembers(true)); err != nil {
		return err
	}
	messages, err := agent.DecodePersistentMessages(wire.History)
	if err != nil {
		return err
	}
	decoded.History = messages
	*c = ConversationCompactionCheckpoint(decoded)
	return nil
}

func (g *CompactionGeneration) UnmarshalJSON(data []byte) error {
	type plain CompactionGeneration
	var decoded plain
	wire := struct {
		*plain
		Replacement jsontext.Value `json:"replacement"`
	}{plain: &decoded}
	if err := json.Unmarshal(data, &wire, json.RejectUnknownMembers(true)); err != nil {
		return err
	}
	messages, err := agent.DecodePersistentMessages(wire.Replacement)
	if err != nil {
		return err
	}
	decoded.Replacement = messages
	*g = CompactionGeneration(decoded)
	return nil
}

func validateCompactionMedia(state *ConversationCompactionState) (bool, error) {
	if state == nil {
		return false, nil
	}
	hasReferences := false
	check := func(messagesHasRefs bool, err error) error {
		if messagesHasRefs {
			hasReferences = true
		}
		return err
	}
	for _, generation := range state.Generations {
		if err := check(agent.ValidatePersistentMessages(generation.Replacement)); err != nil {
			return false, err
		}
	}
	for _, checkpoint := range state.Branches {
		if err := check(agent.ValidatePersistentMessages(checkpoint.History)); err != nil {
			return false, err
		}
	}
	for _, checkpoints := range state.Checkpoints {
		for _, checkpoint := range checkpoints {
			if err := check(agent.ValidatePersistentMessages(checkpoint.History)); err != nil {
				return false, err
			}
		}
	}
	return hasReferences, nil
}

func validateCompactionMediaVersion(state *ConversationCompactionState) error {
	hasReferences, err := validateCompactionMedia(state)
	if err != nil {
		return err
	}
	if hasReferences && !isMediaCompactionVersion(state.Version) {
		return media.Reject(media.DurabilityRequired, "media history requires a media-aware checkpoint version")
	}
	if compactionHasToolMedia(state) && state.Version != toolMediaCompactionStateVersion {
		return media.Reject(media.DurabilityRequired, "tool media history requires a tool-aware checkpoint version")
	}
	return nil
}

func isMediaCompactionVersion(version int) bool {
	return version == mediaCompactionStateVersion || version == toolMediaCompactionStateVersion
}

func compactionHasToolMedia(state *ConversationCompactionState) bool {
	if state == nil {
		return false
	}
	for _, generation := range state.Generations {
		if agent.MessagesHaveToolMedia(generation.Replacement) {
			return true
		}
	}
	for _, checkpoint := range state.Branches {
		if agent.MessagesHaveToolMedia(checkpoint.History) {
			return true
		}
	}
	for _, checkpoints := range state.Checkpoints {
		for _, checkpoint := range checkpoints {
			if agent.MessagesHaveToolMedia(checkpoint.History) {
				return true
			}
		}
	}
	return false
}

func validateCompactionStateVersion(state *ConversationCompactionState) error {
	if state.Version != conversationCompactionStateVersion && !isMediaCompactionVersion(state.Version) {
		return fmt.Errorf("unsupported version %d", state.Version)
	}
	return validateCompactionMediaVersion(state)
}

func validateCompactionGenerationOrigin(generation CompactionGeneration, version int) error {
	if strings.TrimSpace(generation.BranchID) == "" {
		return fmt.Errorf("generation has no branch identity")
	}
	switch generation.Kind {
	case "":
		if strings.TrimSpace(generation.ModelID) == "" || len(generation.SourceRanges) == 0 {
			return fmt.Errorf("compaction generation lacks model or source provenance")
		}
	case nativeInputGenerationKind, nativeResetGenerationKind:
		hasReferences, err := agent.ValidatePersistentMessages(generation.Replacement)
		if err != nil {
			return err
		}
		if !isMediaCompactionVersion(version) || generation.ModelID != "" || generation.PredecessorID != "" ||
			len(generation.SourceRanges) != 0 || generation.TokensBefore != 0 || generation.TokensAfter != 0 || !reflect.DeepEqual(generation.Summary, StructuredSummary{}) {
			return media.Reject(media.BindingConflict, "invalid uncompacted media input checkpoint")
		}
		if generation.Kind == nativeInputGenerationKind && !hasReferences || generation.Kind == nativeResetGenerationKind && len(generation.Replacement) != 0 {
			return media.Reject(media.BindingConflict, "native conversation generation has invalid replacement content")
		}
	default:
		return media.Reject(media.Unsupported, "unknown conversation generation kind")
	}
	return nil
}

func compactionGenerationSummary(generation CompactionGeneration) *StructuredSummary {
	if generation.Kind == nativeInputGenerationKind || generation.Kind == nativeResetGenerationKind {
		return nil
	}
	return cloneStructuredSummary(&generation.Summary)
}
