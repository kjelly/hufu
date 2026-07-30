package team

import (
	"encoding/json"
	"strings"
)

// ReduceToSessionData replays a sequence of RunEvents to reconstruct a SessionData projection.
func ReduceToSessionData(events []RunEvent) *SessionData {
	session := NewSession()
	for _, e := range events {
		switch e.Type {
		case "run_started":
			if session.CreatedAt == "" && e.Timestamp != "" {
				session.CreatedAt = e.Timestamp
			}
		case "user_message_added", "assistant_message_added":
			var payload struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			}
			if err := json.Unmarshal(e.Payload, &payload); err == nil && payload.Content != "" {
				role := payload.Role
				if role == "" {
					if strings.HasPrefix(e.Type, "user") {
						role = "user"
					} else {
						role = "assistant"
					}
				}
				session.AddEntry(role, payload.Content)
			}
		}
	}
	return session
}

// ReduceToTodoList replays a sequence of RunEvents to reconstruct a TodoItem slice projection.
func ReduceToTodoList(events []RunEvent) []*TodoItem {
	taskMap := make(map[string]*TodoItem)
	var taskOrder []string

	for _, e := range events {
		if !strings.HasPrefix(e.Type, "task_") {
			continue
		}

		var payload struct {
			ID                string              `json:"id"`
			Description       string              `json:"description"`
			Desc              string              `json:"desc"`
			Status            string              `json:"status"`
			Output            string              `json:"output"`
			Agent             string              `json:"agent"`
			DependsOn         []string            `json:"depends_on"`
			Verify            string              `json:"verify"`
			VerifyMode        string              `json:"verify_mode"`
			VerifySpec        *VerificationSpec   `json:"verify_spec"`
			VerifyResult      *VerificationResult `json:"verify_result"`
			ExecutionReceipt  *ExecutionReceipt   `json:"execution_receipt"`
			ExecutionReceipts []ExecutionReceipt  `json:"execution_receipts"`
		}
		_ = json.Unmarshal(e.Payload, &payload)

		taskID := e.TaskID
		if taskID == "" {
			taskID = payload.ID
		}
		if taskID == "" {
			continue
		}

		desc := payload.Desc
		if desc == "" {
			desc = payload.Description
		}

		item, exists := taskMap[taskID]
		if !exists {
			item = &TodoItem{
				ID:        taskID,
				Desc:      desc,
				Status:    TaskPending,
				Agent:     payload.Agent,
				DependsOn: payload.DependsOn,
			}
			taskMap[taskID] = item
			taskOrder = append(taskOrder, taskID)
		}

		if desc != "" {
			item.Desc = desc
		}
		if payload.Agent != "" {
			item.Agent = payload.Agent
		}
		if len(payload.DependsOn) > 0 {
			item.DependsOn = payload.DependsOn
		}
		if payload.Verify != "" {
			item.Verify = payload.Verify
			item.VerifyMode = payload.VerifyMode
		}
		if payload.VerifySpec != nil {
			item.VerifySpec = cloneVerificationSpecPtr(payload.VerifySpec)
		}
		if payload.VerifyResult != nil {
			item.VerifyResult = payload.VerifyResult
		}
		if payload.ExecutionReceipt != nil {
			item.ExecutionReceipt = payload.ExecutionReceipt
			item.ExecutionReceipts = append(item.ExecutionReceipts, *payload.ExecutionReceipt)
		}
		if len(payload.ExecutionReceipts) > 0 {
			item.ExecutionReceipts = payload.ExecutionReceipts
		}

		switch e.Type {
		case "task_created":
			if payload.Status != "" {
				item.Status = TaskStatus(payload.Status)
			}
		case "task_started":
			item.Status = TaskInProgress
		case "task_completed":
			item.Status = TaskDone
			if payload.Output != "" {
				item.Output = payload.Output
			}
		case "task_failed":
			item.Status = TaskError
			if payload.Output != "" {
				item.Output = payload.Output
			}
		case "task_skipped":
			item.Status = TaskSkipped
		case "task_blocked":
			item.Status = TaskBlocked
		case "task_protocol_incomplete":
			item.Status = TaskProtocolIncomplete
		case "task_reset":
			item.Status = TaskPending
		}
	}

	result := make([]*TodoItem, 0, len(taskOrder))
	for _, id := range taskOrder {
		result = append(result, taskMap[id])
	}
	return result
}
