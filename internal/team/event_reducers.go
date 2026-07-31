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
		case "criterion_re_evaluated":
			var payload struct {
				After        []CriterionResult `json:"after"`
				Progress     TaskProgress      `json:"progress"`
				ProgressedAt string            `json:"progressed_at"`
			}
			if err := json.Unmarshal(e.Payload, &payload); err == nil && len(payload.After) > 0 {
				session.CriterionResults = payload.After
				if payload.Progress == ProgressAdvanced {
					if payload.ProgressedAt != "" {
						session.LastCriterionProgressAt = payload.ProgressedAt
					} else if e.Timestamp != "" {
						// Older events did not include an explicit timestamp. The
						// event time remains a deterministic approximation only for
						// a recorded criterion advance.
						session.LastCriterionProgressAt = e.Timestamp
					}
				}
			}
		case "criterion_checkpoint_saved":
			var payload struct {
				Checkpoint CriterionCheckpoint `json:"checkpoint"`
			}
			if err := json.Unmarshal(e.Payload, &payload); err == nil && payload.Checkpoint.CriterionID != "" {
				filtered := session.CriterionCheckpoints[:0]
				for _, checkpoint := range session.CriterionCheckpoints {
					if checkpoint.CriterionID != payload.Checkpoint.CriterionID {
						filtered = append(filtered, checkpoint)
					}
				}
				session.CriterionCheckpoints = append(filtered, payload.Checkpoint)
			}
		}
	}
	session.Tasks = ReduceToTodoList(events)
	return session
}

// ReduceToTodoList replays a sequence of RunEvents to reconstruct a TodoItem slice projection.
func ReduceToTodoList(events []RunEvent) []*TodoItem {
	taskMap := make(map[string]*TodoItem)
	var taskOrder []string

	for _, e := range events {
		if e.Type == "criterion_re_evaluated" && e.TaskID != "" {
			var payload struct {
				Progress         TaskProgress `json:"progress"`
				ProgressCriteria []string     `json:"progress_criteria"`
			}
			if err := json.Unmarshal(e.Payload, &payload); err == nil && payload.Progress != "" {
				if item := taskMap[e.TaskID]; item != nil {
					item.Progress = payload.Progress
					item.ProgressCriteria = append([]string(nil), payload.ProgressCriteria...)
				}
			}
			continue
		}
		if e.Type == "failure_fingerprint" {
			var payload struct {
				Fingerprint FailureFingerprint `json:"fingerprint"`
			}
			if err := json.Unmarshal(e.Payload, &payload); err == nil && payload.Fingerprint.Digest != "" && e.TaskID != "" {
				item, exists := taskMap[e.TaskID]
				if !exists {
					item = &TodoItem{ID: e.TaskID, Status: TaskError, Agent: e.Actor}
					taskMap[e.TaskID] = item
					taskOrder = append(taskOrder, e.TaskID)
				}
				seen := false
				for i := range item.FailureFingerprints {
					existing := &item.FailureFingerprints[i]
					if existing.Digest == payload.Fingerprint.Digest {
						if existing.Occurrences < 1 {
							existing.Occurrences = 1
						}
						occurrences := payload.Fingerprint.Occurrences
						if occurrences < 1 {
							occurrences = 1
						}
						if occurrences > existing.Occurrences {
							existing.Occurrences = occurrences
						}
						seen = true
						break
					}
				}
				if !seen {
					if payload.Fingerprint.Occurrences < 1 {
						payload.Fingerprint.Occurrences = 1
					}
					item.FailureFingerprints = append(item.FailureFingerprints, payload.Fingerprint)
				}
			}
			continue
		}
		if !strings.HasPrefix(e.Type, "task_") {
			continue
		}

		var payload struct {
			ID                  string               `json:"id"`
			Description         string               `json:"description"`
			Desc                string               `json:"desc"`
			Status              string               `json:"status"`
			MaxRetries          int                  `json:"max_retries"`
			Retries             int                  `json:"retries"`
			Output              string               `json:"output"`
			Agent               string               `json:"agent"`
			DependsOn           []string             `json:"depends_on"`
			Verify              string               `json:"verify"`
			VerifyMode          string               `json:"verify_mode"`
			VerifySpec          *VerificationSpec    `json:"verify_spec"`
			VerifyResult        *VerificationResult  `json:"verify_result"`
			ExecutionReceipt    *ExecutionReceipt    `json:"execution_receipt"`
			ExecutionReceipts   []ExecutionReceipt   `json:"execution_receipts"`
			Kind                TaskKind             `json:"kind"`
			Advances            []string             `json:"advances"`
			ExpectedStateChange string               `json:"expected_state_change"`
			Progress            TaskProgress         `json:"progress"`
			ProgressCriteria    []string             `json:"progress_criteria"`
			FailureFingerprints []FailureFingerprint `json:"failure_fingerprints"`
			Execution           ExecutionContract    `json:"execution"`
			RecoveryHypothesis  *RecoveryHypothesis  `json:"recovery_hypothesis"`
			SideEffect          SideEffectClass      `json:"side_effect"`
			Recovery            RecoveryPolicy       `json:"recovery"`
			ReconcileTool       string               `json:"reconcile_tool"`
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
				ID:                  taskID,
				Desc:                desc,
				Status:              TaskPending,
				MaxRetries:          payload.MaxRetries,
				Retries:             payload.Retries,
				Agent:               payload.Agent,
				DependsOn:           payload.DependsOn,
				Kind:                payload.Kind,
				Advances:            append([]string(nil), payload.Advances...),
				ExpectedStateChange: payload.ExpectedStateChange,
				Progress:            payload.Progress,
				ProgressCriteria:    append([]string(nil), payload.ProgressCriteria...),
				FailureFingerprints: append([]FailureFingerprint(nil), payload.FailureFingerprints...),
				Execution:           payload.Execution,
				RecoveryHypothesis:  cloneRecoveryHypothesis(payload.RecoveryHypothesis),
				SideEffect:          payload.SideEffect,
				Recovery:            payload.Recovery,
				ReconcileTool:       payload.ReconcileTool,
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
		if payload.Kind != "" {
			item.Kind = payload.Kind
		}
		if len(payload.Advances) > 0 {
			item.Advances = append([]string(nil), payload.Advances...)
		}
		if payload.ExpectedStateChange != "" {
			item.ExpectedStateChange = payload.ExpectedStateChange
		}
		if payload.Progress != "" {
			item.Progress = payload.Progress
			item.ProgressCriteria = append([]string(nil), payload.ProgressCriteria...)
		}
		if payload.MaxRetries != 0 {
			item.MaxRetries = payload.MaxRetries
		}
		if payload.Retries != 0 {
			item.Retries = payload.Retries
		}
		for _, fingerprint := range payload.FailureFingerprints {
			seen := false
			for i := range item.FailureFingerprints {
				existing := &item.FailureFingerprints[i]
				if existing.Digest == fingerprint.Digest {
					if existing.Occurrences < 1 {
						existing.Occurrences = 1
					}
					occurrences := fingerprint.Occurrences
					if occurrences < 1 {
						occurrences = 1
					}
					if occurrences > existing.Occurrences {
						existing.Occurrences = occurrences
					}
					seen = true
					break
				}
			}
			if !seen {
				if fingerprint.Occurrences < 1 {
					fingerprint.Occurrences = 1
				}
				item.FailureFingerprints = append(item.FailureFingerprints, fingerprint)
			}
		}
		if payload.Execution.Kind != "" || payload.Execution.AllowsReplay != nil || payload.Execution.RequiresResult || payload.Execution.RequiresVerification {
			item.Execution = payload.Execution
		}
		if payload.SideEffect != "" {
			item.SideEffect = payload.SideEffect
		}
		if payload.Recovery != "" {
			item.Recovery = payload.Recovery
		}
		if payload.ReconcileTool != "" {
			item.ReconcileTool = payload.ReconcileTool
		}
		if payload.RecoveryHypothesis != nil {
			item.RecoveryHypothesis = cloneRecoveryHypothesis(payload.RecoveryHypothesis)
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
