package team

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"

	"charm.land/fantasy"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/media"
	"github.com/kjelly/hufu/internal/runtimestore"
	"github.com/kjelly/hufu/internal/utils"
)

func SaveConversationHistory(workspace string, messages []fantasy.Message) error {
	hasReferences, err := agent.ValidatePersistentMessages(messages)
	if err != nil {
		return err
	}
	filtered := filterMessages(messages)
	if len(filtered) == 0 {
		return nil
	}

	var data []byte
	if hasReferences {
		checkpointID, checkErr := canonicalMediaHistoryProjection(workspace, filtered)
		if checkErr != nil {
			return checkErr
		}
		data, err = agent.EncodeMediaHistory(filtered, checkpointID)
	} else {
		data, err = json.Marshal(filtered)
	}
	if err != nil {
		return fmt.Errorf("marshal conversation history: %w", err)
	}

	redacted, rerr := utils.RedactJSON(data)
	if rerr != nil {
		if hasReferences {
			return fmt.Errorf("redact media history: %w", rerr)
		}
		// On redaction failure never persist the unredacted marshaled JSON;
		// fall back to text redaction so a best-effort scrub still happens.
		redacted = []byte(utils.RedactSecrets(string(data)))
	}
	if err := workspaceDocs(workspace).putDocument(runtimestore.DocumentSessionHistory, redacted); err != nil {
		return fmt.Errorf("write conversation history: %w", err)
	}
	return nil
}

func LoadConversationHistory(workspace string) []fantasy.Message {
	stored, err := loadConversationHistoryDocument(workspace)
	if err != nil {
		return nil
	}
	return stored.messages
}

func restoreLegacyConversationProjection(c *Coordinator, session *TeamSession) {
	stored, err := loadConversationHistoryDocument(session.Workspace)
	if err != nil {
		c.compactionRecoveryErr = err
		return
	}
	if stored.mediaProjection {
		c.compactionRecoveryErr = media.Reject(media.DurabilityRequired, "media history cannot replace its missing canonical checkpoint")
		return
	}
	history := stored.messages
	if len(history) == 0 {
		return
	}
	c.conversationHistory = history
	c.conversationHistorySourceRanges = historySourceRanges(len(history), 0, nil, nil)
	c.conversationHistorySourceCounts = sourceCountsForRanges(c.conversationHistorySourceRanges)
	c.conversationHistoryNextSourceIndex = maxSourceIndex(c.conversationHistorySourceRanges)
	if persisted := LoadSession(session.Workspace); persisted != nil {
		c.conversationHistorySourceRanges = historySourceRanges(len(history), persisted.ConversationHistorySourceOffset, persisted.ConversationHistorySourceCounts, persisted.ConversationHistorySourceRanges)
		c.conversationHistorySourceCounts = sourceCountsForRanges(c.conversationHistorySourceRanges)
		c.conversationHistorySourceOffset = persisted.ConversationHistorySourceOffset
		c.conversationHistoryNextSourceIndex = persisted.ConversationHistoryNextSourceIndex
		if c.conversationHistoryNextSourceIndex < maxSourceIndex(c.conversationHistorySourceRanges) {
			c.conversationHistoryNextSourceIndex = maxSourceIndex(c.conversationHistorySourceRanges)
		}
		if c.conversationHistorySourceOffset < 0 {
			c.conversationHistorySourceOffset = 0
		}
	}
}

type storedConversationHistory struct {
	messages        []fantasy.Message
	mediaProjection bool
	checkpointID    string
}

func loadConversationHistoryDocument(workspace string) (storedConversationHistory, error) {
	var stored storedConversationHistory
	data, exists, err := workspaceDocs(workspace).getDocument(runtimestore.DocumentSessionHistory)
	if err != nil || !exists {
		return stored, err
	}
	var messages []fantasy.Message
	if bytes.HasPrefix(bytes.TrimSpace(data), []byte("{")) {
		messages, stored.checkpointID, err = agent.DecodeMediaHistory(data)
		stored.mediaProjection = true
	} else {
		messages, err = agent.DecodePersistentMessages(data)
		if err == nil {
			hasReferences, checkErr := agent.ValidatePersistentMessages(messages)
			if checkErr != nil {
				return stored, checkErr
			}
			if hasReferences {
				return stored, media.Reject(media.DurabilityRequired, "media history lacks its versioned projection envelope")
			}
		}
	}
	if err != nil {
		return stored, err
	}
	projected, err := projectHistoryToolPairs(messages)
	if err != nil {
		// Do not hide malformed persisted history by trimming away its defect.
		stored.messages = filterMessages(messages)
		return stored, nil
	}
	messages = projected

	if !stored.mediaProjection && len(messages) > maxConversationHistory {
		messages, _, _ = trimHistoryPreservingHead(messages, nil, maxConversationHistory)
	}

	stored.messages = filterMessages(messages)
	return stored, nil
}

// Use the same persisted conversation selected by coordinator startup. A
// resume preview must reject malformed tool pairs before spending provider work.
func validateResumeConversation(ctx context.Context, workspace, branch string) error {
	state, exists, err := LoadConversationCompactionState(workspace)
	if err != nil {
		return err
	}
	if exists {
		if err := validateMediaHistorySources(ctx, workspace, branch, state.Branches[branch].History); err != nil {
			return err
		}
		return validateToolPairs(state.Branches[branch].History)
	}
	stored, err := loadConversationHistoryDocument(workspace)
	if err != nil {
		return err
	}
	if stored.mediaProjection {
		return media.Reject(media.DurabilityRequired, "media history cannot resume without its canonical checkpoint")
	}
	return validateToolPairs(stored.messages)
}

func filterMessages(messages []fantasy.Message) []fantasy.Message {
	filtered := make([]fantasy.Message, 0, len(messages))
	for _, msg := range messages {
		if agent.MessagesHaveMedia([]fantasy.Message{msg}) {
			filtered = append(filtered, msg)
			continue
		}
		// Truncate oversized messages instead of dropping them: a dropped
		// tool result erases fetched content from history and the
		// coordinator re-delegates the same read next round.
		filtered = append(filtered, truncateOversizedMessage(msg, maxMessageSize))
	}
	return filtered
}

func DeleteConversationHistory(workspace string) error {
	_ = DeleteCompactionHistory(workspace)
	return workspaceDocs(workspace).deleteDocument(runtimestore.DocumentSessionHistory)
}

func HasConversationHistory(workspace string) bool {
	_, exists, err := workspaceDocs(workspace).getDocument(runtimestore.DocumentSessionHistory)
	return err == nil && exists
}
