package team

import (
	"crypto/sha256"
	"fmt"

	"charm.land/fantasy"
)

type toolPairingError struct {
	Reason       string
	MessageIndex int
	PartIndex    int
	IDHash       string
	PriorMessage int
	PriorPart    int
	PriorResult  bool
}

func (e *toolPairingError) Error() string {
	return fmt.Sprintf("%s at message %d part %d (tool_call_id_sha256=%s prior_message=%d prior_part=%d prior_result=%t)", e.Reason, e.MessageIndex, e.PartIndex, e.IDHash, e.PriorMessage, e.PriorPart, e.PriorResult)
}

func newToolPairingError(reason, id string, message, part int) *toolPairingError {
	// IDs are provider/model input. Keep diagnostics bounded and content-free.
	digest := sha256.Sum256([]byte(id))
	return &toolPairingError{Reason: reason, MessageIndex: message, PartIndex: part, IDHash: fmt.Sprintf("%x", digest[:8]), PriorMessage: -1, PriorPart: -1}
}

func toolPairsIntact(messages []fantasy.Message) bool {
	return validateToolPairs(messages) == nil
}

func validateToolPairs(messages []fantasy.Message) error {
	type position struct{ message, part int }
	calls := make(map[string]position)
	results := make(map[string]bool)
	var order []string
	for i, message := range messages {
		for j, part := range message.Content {
			if call, ok := fantasy.AsMessagePart[fantasy.ToolCallPart](part); ok {
				if call.ToolCallID == "" {
					return newToolPairingError("empty_call_id", "", i, j)
				}
				if prior, duplicate := calls[call.ToolCallID]; duplicate {
					err := newToolPairingError("duplicate_call_id", call.ToolCallID, i, j)
					err.PriorMessage, err.PriorPart, err.PriorResult = prior.message, prior.part, results[call.ToolCallID]
					return err
				}
				calls[call.ToolCallID] = position{i, j}
				order = append(order, call.ToolCallID)
			}
			if result, ok := fantasy.AsMessagePart[fantasy.ToolResultPart](part); ok {
				if result.ToolCallID == "" {
					return newToolPairingError("empty_result_id", "", i, j)
				}
				if _, exists := calls[result.ToolCallID]; !exists {
					return newToolPairingError("result_without_call", result.ToolCallID, i, j)
				}
				if results[result.ToolCallID] {
					return newToolPairingError("duplicate_result_id", result.ToolCallID, i, j)
				}
				results[result.ToolCallID] = true
			}
		}
	}
	for _, id := range order {
		if !results[id] {
			p := calls[id]
			return newToolPairingError("missing_result", id, p.message, p.part)
		}
	}
	return nil
}
