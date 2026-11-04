package team

import (
	"fmt"
	"slices"

	"charm.land/fantasy"
)

// projectHistoryToolPairs disambiguates provider IDs recycled in later, closed
// exchanges. It never changes native tool observations or execution receipts.
// Concurrent collisions, duplicate results and incomplete exchanges fail closed.
func projectHistoryToolPairs(messages []fantasy.Message) ([]fantasy.Message, error) {
	if err := validateToolPairs(messages); err == nil {
		return messages, nil
	}
	type position struct{ message, part int }
	used := make(map[string]bool)
	for _, message := range messages {
		for _, part := range message.Content {
			if call, ok := fantasy.AsMessagePart[fantasy.ToolCallPart](part); ok {
				used[call.ToolCallID] = true
			}
		}
	}
	active := make(map[string]string)
	previous := make(map[string]position)
	finished := make(map[string]int)
	var order []string
	sequence := 0
	projected := slices.Clone(messages)
	for i, message := range messages {
		projected[i].Content = slices.Clone(message.Content)
		for j, part := range message.Content {
			if call, ok := fantasy.AsMessagePart[fantasy.ToolCallPart](part); ok {
				id := call.ToolCallID
				if id == "" {
					return messages, newToolPairingError("empty_call_id", id, i, j)
				}
				prior, seen := previous[id]
				_, pending := active[id]
				if pending || seen && finished[id] == i {
					err := newToolPairingError("duplicate_call_id", id, i, j)
					err.PriorMessage, err.PriorPart, err.PriorResult = prior.message, prior.part, !pending
					return messages, err
				}
				alias := id
				if seen {
					for {
						alias = fmt.Sprintf("hufu_history_%d", sequence)
						sequence++
						if !used[alias] {
							break
						}
					}
					used[alias] = true
					call.ToolCallID = alias
					projected[i].Content[j] = call
				}
				active[id], previous[id] = alias, position{i, j}
				order = append(order, id)
			}
			if result, ok := fantasy.AsMessagePart[fantasy.ToolResultPart](part); ok {
				id := result.ToolCallID
				if id == "" {
					return messages, newToolPairingError("empty_result_id", id, i, j)
				}
				alias, pending := active[id]
				if !pending {
					reason := "result_without_call"
					if _, seen := finished[id]; seen {
						reason = "duplicate_result_id"
					}
					return messages, newToolPairingError(reason, id, i, j)
				}
				result.ToolCallID = alias
				projected[i].Content[j] = result
				delete(active, id)
				finished[id] = i
			}
		}
	}
	for _, id := range order {
		if _, pending := active[id]; pending {
			p := previous[id]
			return messages, newToolPairingError("missing_result", id, p.message, p.part)
		}
	}
	return projected, validateToolPairs(projected)
}
