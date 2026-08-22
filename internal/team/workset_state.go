package team

import "sort"

// WorksetGroupStates returns the canonical bounded projection of every
// expansion receipt visible in the current task/session state.
func (c *Coordinator) WorksetGroupStates() []WorksetGroupState {
	if c == nil || c.taskTracker == nil || c.taskTracker.TodoList() == nil {
		return nil
	}
	items := c.taskTracker.TodoList().Items()
	visible := make([]*WorksetExpansionReceipt, 0, len(items))
	for _, item := range items {
		if item != nil && item.WorksetReceipt != nil {
			visible = append(visible, item.WorksetReceipt)
		}
	}
	receipts, conflicts := collectWorksetReceipts(visible)
	ids := make([]string, 0, len(receipts))
	for id := range receipts {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	states := make([]WorksetGroupState, 0, len(ids))
	for _, id := range ids {
		receipt := receipts[id]
		state := WorksetGroupState{
			WorksetID: receipt.WorksetID, ParentTaskID: receipt.ParentTaskID,
			SourceArtifactID: receipt.SourceArtifactID, SourceSHA256: receipt.SourceSHA256,
			Expected: receipt.ItemCount,
		}
		if _, conflicted := conflicts[id]; conflicted {
			state.Failed = 1
			state.State = "failed"
			states = append(states, state)
			continue
		}
		if len(receipt.Children) != receipt.ItemCount {
			state.Failed++
		}
		childIDs := make(map[string]struct{}, len(receipt.Children))
		for key, childID := range receipt.Children {
			childIDs[childID] = struct{}{}
			item := findTodoItem(items, childID)
			if item == nil || item.WorksetBinding == nil || item.WorksetBinding.WorksetID != receipt.WorksetID || item.WorksetBinding.ItemKey != key {
				state.Failed++
				continue
			}
			if item.Status != TaskDone || item.TypedResult == nil || !taskResultStatusIsSuccessful(item.TypedResult.Status) {
				state.Failed++
				continue
			}
			state.Completed++
			if item.VerifyResult != nil && isVerifySuccess(item.VerifyResult) {
				state.Verified++
			}
		}
		for _, item := range items {
			if item != nil && item.WorksetBinding != nil && item.WorksetBinding.WorksetID == receipt.WorksetID {
				if _, bound := childIDs[item.ID]; !bound {
					state.Failed++
				}
			}
		}
		switch {
		case state.Expected <= 0:
			state.State = "empty"
		case state.Failed > 0:
			state.State = "failed"
		case state.Completed == state.Expected && state.Verified == state.Expected:
			state.State = "complete"
		case state.Completed == state.Expected:
			state.State = "completed"
		default:
			state.State = "partial"
		}
		states = append(states, state)
	}
	return states
}

func findTodoItem(items []*TodoItem, id string) *TodoItem {
	for _, item := range items {
		if item != nil && item.ID == id {
			return item
		}
	}
	return nil
}
