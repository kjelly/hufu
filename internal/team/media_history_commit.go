package team

import (
	"context"
	"fmt"
	"reflect"
	"time"

	"charm.land/fantasy"
	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/media"
)

func mediaSnapshotMessage(snapshot media.InputSnapshot) (fantasy.Message, error) {
	if err := snapshot.Validate(media.Limits{}); err != nil {
		return fantasy.Message{}, err
	}
	refs := make(map[string]media.Ref, len(snapshot.Refs))
	for _, ref := range snapshot.Refs {
		refs[ref.ID] = ref
	}
	message := fantasy.Message{Role: fantasy.MessageRoleUser}
	for index, part := range snapshot.Input.Parts {
		if part.Type == "text" {
			message.Content = append(message.Content, fantasy.TextPart{Text: part.Text})
			continue
		}
		message.Content = append(message.Content, agent.MediaReferencePart{Reference: media.HistoryReference{
			Version: media.Version, Source: media.Selection{SnapshotID: snapshot.ID, SnapshotHash: snapshot.Hash, TurnID: snapshot.TurnID, PartIndex: index},
			Ref: refs[part.MediaID], PolicyHash: snapshot.PolicyHash, BindingHash: snapshot.Hash,
		}})
	}
	return message, nil
}

// persistMediaInputHistory uses the existing canonical conversation store for
// the first input, before there is any model-produced compaction generation.
// A native_input generation has no model, token usage, summary or claimed
// compacted ranges. Its checkpoint retains all original source ranges.
func (c *Coordinator) persistMediaInputHistory(ctx context.Context, snapshot media.InputSnapshot) error {
	message, err := mediaSnapshotMessage(snapshot)
	if err != nil {
		return err
	}
	branch, workspace := c.compactionBranch(), c.sessionWorkspace()
	if err := validateMediaHistorySources(ctx, workspace, branch, []fantasy.Message{message}); err != nil {
		return err
	}
	state, exists, err := LoadConversationCompactionState(workspace)
	if err != nil {
		return err
	}
	history := cloneMessages(c.conversationHistory)
	ranges := historySourceRanges(len(history), c.conversationHistorySourceOffset, c.conversationHistorySourceCounts, c.conversationHistorySourceRanges)
	nextSource := max(c.conversationHistoryNextSourceIndex, maxSourceIndex(ranges))
	checkpoint, hasCheckpoint := ConversationCompactionCheckpoint{}, false
	if exists {
		checkpoint, hasCheckpoint = state.Branches[branch]
		if hasCheckpoint {
			history, ranges = cloneMessages(checkpoint.History), cloneSourceRanges(checkpoint.SourceRanges)
			nextSource = checkpoint.NextSourceIndex
		}
	}
	if found, err := mediaTurnAlreadyRecorded(history, message, snapshot.ID); err != nil {
		return err
	} else if found {
		if !hasCheckpoint {
			return media.Reject(media.DurabilityRequired, "media turn has no canonical checkpoint")
		}
		if err := c.reconcileCompactionState(ctx, branch); err != nil {
			return err
		}
		if err := SaveConversationHistory(workspace, history); err != nil {
			c.markCompactionRecovery(err)
			return err
		}
		return c.restoreCanonicalCompactionForBranch(branch)
	}
	history = append(history, message)
	ranges = append(ranges, []CompactionRange{{StartIndex: nextSource, EndIndex: nextSource, MsgCount: 1}})
	nextSource++
	if hasCheckpoint {
		if err := c.persistConversationCheckpointWithProvenance(history, sourceOffsetForRanges(ranges, 0), sourceCountsForRanges(ranges), ranges, nextSource); err != nil {
			return err
		}
	} else if err := c.commitNativeInputCheckpoint(ctx, state, branch, history, ranges, nextSource); err != nil {
		return err
	}
	if err := SaveConversationHistory(workspace, history); err != nil {
		c.markCompactionRecovery(err)
		return err
	}
	c.conversationHistoryMu.Lock()
	c.conversationHistory = cloneMessages(history)
	c.conversationHistorySourceRanges = cloneSourceRanges(ranges)
	c.conversationHistorySourceCounts = sourceCountsForRanges(ranges)
	c.conversationHistorySourceOffset = sourceOffsetForRanges(ranges, 0)
	c.conversationHistoryNextSourceIndex = nextSource
	c.conversationHistoryMu.Unlock()
	return nil
}

func mediaTurnAlreadyRecorded(history []fantasy.Message, message fantasy.Message, snapshotID string) (bool, error) {
	var recorded []media.HistoryReference
	for _, location := range historyReferences(history) {
		if location.Reference.Source.SnapshotID == snapshotID && location.Reference.TaskID == "" {
			recorded = append(recorded, location.Reference)
		}
	}
	if len(recorded) == 0 {
		return false, nil
	}
	var expected []media.HistoryReference
	for _, location := range historyReferences([]fantasy.Message{message}) {
		expected = append(expected, location.Reference)
	}
	if !reflect.DeepEqual(recorded, expected) {
		return false, media.Reject(media.BindingConflict, "recorded media turn has different occurrences")
	}
	return true, nil
}

func (c *Coordinator) commitNativeInputCheckpoint(ctx context.Context, state *ConversationCompactionState, branch string, history []fantasy.Message, ranges [][]CompactionRange, nextSource int) error {
	return c.commitNativeConversationCheckpoint(ctx, state, branch, history, ranges, nextSource, nativeInputGenerationKind)
}

func (c *Coordinator) commitNativeConversationCheckpoint(ctx context.Context, state *ConversationCompactionState, branch string, history []fantasy.Message, ranges [][]CompactionRange, nextSource int, kind string) error {
	if err := validatePersistentHistories(history); err != nil {
		return err
	}
	if state == nil {
		state = newCompactionState()
	} else {
		state = cloneCompactionState(state)
	}
	if state == nil {
		return media.Reject(media.DurabilityRequired, "cannot clone canonical conversation state")
	}
	state.Version = max(state.Version, mediaCompactionStateVersion)
	if agent.MessagesHaveToolMedia(history) {
		state.Version = toolMediaCompactionStateVersion
	}
	generation := CompactionGeneration{Kind: kind, ID: nextCompactionGenerationID(branch), BranchID: branch, CreatedAt: time.Now().UTC(), Replacement: cloneMessages(history)}
	generation.SummaryDigest = digestStructuredSummary(&generation.Summary)
	generation.ReplacementDigest = digestMessages(generation.Replacement)
	generation.Checksum = digestGeneration(generation)
	state.Generations[generation.ID] = generation
	checkpoint := ConversationCompactionCheckpoint{BranchID: branch, GenerationID: generation.ID, AttestationMode: compactionCheckpointAttestationMode,
		History: cloneMessages(history), SourceOffset: sourceOffsetForRanges(ranges, nextSource), SourceCounts: sourceCountsForRanges(ranges), SourceRanges: cloneSourceRanges(ranges), NextSourceIndex: nextSource, HistoryDigest: digestMessages(history)}
	state.Branches[branch] = checkpoint
	state, err := redactedCompactionState(state)
	if err != nil {
		return err
	}
	generation, checkpoint = state.Generations[generation.ID], state.Branches[branch]
	checkpoint.EventID = compactionCheckpointEventID(checkpoint)
	state.Branches[branch] = checkpoint
	state.Checkpoints[branch] = append(state.Checkpoints[branch], checkpoint)
	if err := saveConversationCompactionState(c.sessionWorkspace(), state, c.eventStore); err != nil {
		return err
	}
	committed, exists, err := LoadConversationCompactionState(c.sessionWorkspace())
	if err != nil || !exists {
		if err == nil {
			err = media.Reject(media.DurabilityRequired, "canonical input checkpoint disappeared")
		}
		c.markCompactionRecovery(err)
		return err
	}
	c.publishCommittedCompaction(committed, branch)
	if err := c.attestCompactionGeneration(ctx, generation); err != nil {
		c.markCompactionRecovery(err)
		return fmt.Errorf("attest native input generation: %w", err)
	}
	if err := c.attestCompactionCheckpoint(ctx, checkpoint, generation); err != nil {
		c.markCompactionRecovery(err)
		return fmt.Errorf("attest native input checkpoint: %w", err)
	}
	return nil
}
