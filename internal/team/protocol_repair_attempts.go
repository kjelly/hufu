package team

import (
	"fmt"
	"time"
)

// A missing/preparation-only generation must not consume the one correction
// owed to the first actual, schema-rejected submission. All other failures
// retain the two-turn limit, and the additional schema turn is never repeated.
func protocolRepairAttemptLimit(history []RepairAttemptProvenance) int {
	if len(history) >= 2 && history[0].Attempt == 1 && history[1].Attempt == 2 &&
		!history[0].Success && !history[1].Success &&
		(history[0].FailureReason == RepairFailureNoToolCall || history[0].FailureReason == repairFailurePreparation) &&
		history[1].FailureReason == RepairFailureInvalidSchema {
		return 3
	}
	return 2
}

func taskProtocolRepairAttemptLimit(item *TodoItem) int {
	if item == nil || item.ExecutionReceipt == nil || item.ExecutionReceipt.RepairProvenance == nil {
		return 2
	}
	p := item.ExecutionReceipt.RepairProvenance
	limit := protocolRepairAttemptLimit(p.History)
	for _, grant := range p.OperatorGrants {
		if grant.RunID != "" && grant.AfterAttempts >= 0 && grant.AdditionalAttempts == 2 {
			limit = max(limit, grant.AfterAttempts+grant.AdditionalAttempts)
		}
	}
	return limit
}

func (c *Coordinator) checkpointProtocolRepairAttempt(item *TodoItem, agentName, prompt string, history []RepairAttemptProvenance, result *TaskResult, success bool, reason RepairFailureReason, runErr error) error {
	receipt := ExecutionReceipt{RunID: c.executionRunID, TaskID: item.ID, Attempt: 1, ProducerID: agentName, StartedAt: time.Now()}
	if item.ExecutionReceipt != nil {
		receipt = cloneExecutionReceipt(item.ExecutionReceipt)
	}
	if receipt.RunID == "" {
		receipt.RunID = c.executionRunID
		if receipt.RunID == "" {
			receipt.RunID = c.taskTracker.TodoList().RunID()
		}
	}
	if receipt.TaskID == "" {
		receipt.TaskID = item.ID
	}
	if receipt.Attempt < 1 {
		receipt.Attempt = 1
	}
	receipt.ProducerID = agentName
	receipt.FinishedAt = time.Now()
	attempts := 0
	if item.ExecutionReceipt != nil && item.ExecutionReceipt.RepairProvenance != nil {
		attempts = item.ExecutionReceipt.RepairProvenance.RepairAttempts
	}
	for _, attempt := range history {
		attempts = max(attempts, attempt.Attempt)
	}
	if len(history) > 0 {
		prompt = history[len(history)-1].Prompt
	}
	var grants []ResultRepairGrant
	if receipt.RepairProvenance != nil {
		grants = receipt.RepairProvenance.OperatorGrants
	}
	receipt.RepairProvenance = &RepairProvenance{
		Attempted: true, Success: success, Prompt: prompt, SubmittedResult: result,
		RepairAttempts: attempts, FailureReason: reason, History: history,
		OperatorGrants: grants,
	}
	if runErr != nil && !success {
		receipt.RepairProvenance.Error = runErr.Error()
	}
	c.applyResultContractReceipt(item.ID, result, &receipt)
	if err := c.setAttemptReceipt(item.ID, &receipt); err != nil {
		return fmt.Errorf("checkpoint result repair attempt: %w", err)
	}
	if err := c.emitTaskEventsFromCheckpoint(c.taskTracker.TodoList().Items()); err != nil {
		return fmt.Errorf("commit result repair attempt: %w", err)
	}
	return nil
}
