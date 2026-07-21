package team

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"charm.land/fantasy"
)

const historyFile = "session_history.json"

func SaveConversationHistory(workspace string, messages []fantasy.Message) error {
	filtered := filterMessages(messages)
	if len(filtered) == 0 {
		return nil
	}

	data, err := json.MarshalIndent(filtered, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal conversation history: %w", err)
	}

	path := filepath.Join(workspace, historyFile)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("write conversation history: %w", err)
	}
	return nil
}

func LoadConversationHistory(workspace string) []fantasy.Message {
	path := filepath.Join(workspace, historyFile)
	data, err := os.ReadFile(path)
	if err != nil {
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

	if len(messages) > maxConversationHistory {
		messages = messages[len(messages)-maxConversationHistory:]
	}

	return filterMessages(messages)
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
	path := filepath.Join(workspace, historyFile)
	err := os.Remove(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func HasConversationHistory(workspace string) bool {
	path := filepath.Join(workspace, historyFile)
	_, err := os.Stat(path)
	return err == nil
}
