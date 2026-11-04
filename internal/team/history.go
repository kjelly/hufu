package team

import (
	"encoding/json"
	"fmt"

	"charm.land/fantasy"

	"github.com/kjelly/hufu/internal/runtimestore"
	"github.com/kjelly/hufu/internal/utils"
)

func SaveConversationHistory(workspace string, messages []fantasy.Message) error {
	filtered := filterMessages(messages)
	if len(filtered) == 0 {
		return nil
	}

	data, err := json.Marshal(filtered)
	if err != nil {
		return fmt.Errorf("marshal conversation history: %w", err)
	}

	redacted, rerr := utils.RedactJSON(data)
	if rerr != nil {
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
	data, exists, err := workspaceDocs(workspace).getDocument(runtimestore.DocumentSessionHistory)
	if err != nil || !exists {
		return nil
	}

	var raw []json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil
	}

	var messages []fantasy.Message
	for _, r := range raw {
		var msg fantasy.Message
		if err := msg.UnmarshalJSON(r); err != nil {
			continue
		}
		messages = append(messages, msg)
	}
	projected, err := projectHistoryToolPairs(messages)
	if err != nil {
		// Do not hide malformed persisted history by trimming away its defect.
		return filterMessages(messages)
	}
	messages = projected

	if len(messages) > maxConversationHistory {
		messages, _, _ = trimHistoryPreservingHead(messages, nil, maxConversationHistory)
	}

	return filterMessages(messages)
}

// Use the same persisted conversation selected by coordinator startup. A
// resume preview must reject malformed tool pairs before spending provider work.
func validateResumeConversation(workspace, branch string) error {
	state, exists, err := LoadConversationCompactionState(workspace)
	if err != nil {
		return err
	}
	if exists {
		return validateToolPairs(state.Branches[branch].History)
	}
	return validateToolPairs(LoadConversationHistory(workspace))
}

func filterMessages(messages []fantasy.Message) []fantasy.Message {
	filtered := make([]fantasy.Message, 0, len(messages))
	for _, msg := range messages {
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
