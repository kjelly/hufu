package team

import (
	"errors"
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
	if item == nil || item.ExecutionReceipt == nil {
		return 2
	}
	receipt := item.ExecutionReceipt
	p := receipt.RepairProvenance
	if p == nil {
		return protocolRepairLimit(receipt.WorkSubmissionFailure, nil)
	}
	limit := protocolRepairLimit(receipt.WorkSubmissionFailure, p.History)
	if receipt.WorkSubmissionFailure == RepairFailureInvalidSchema {
		return limit
	}
	for _, grant := range p.OperatorGrants {
		if grant.RunID != "" && grant.AfterAttempts >= 0 && grant.AdditionalAttempts == 2 &&
			(!hasBoundProtocolSchemaRepair(receipt) || protocolSchemaRejectionWithinGrant(p.History, grant)) {
			limit = max(limit, grant.AfterAttempts+grant.AdditionalAttempts)
		}
	}
	return limit
}

// A granted turn that first encounters a schema rejection can use the one
// correction reserved by that grant. Preexisting schema failures stay bounded.
func protocolSchemaRejectionWithinGrant(history []RepairAttemptProvenance, grant ResultRepairGrant) bool {
	for _, attempt := range history {
		if attempt.FailureReason == RepairFailureInvalidSchema {
			return attempt.Attempt == grant.AfterAttempts+1
		}
	}
	return false
}

func hasBoundProtocolSchemaRepair(receipt *ExecutionReceipt) bool {
	return receipt.WorkSubmissionFailure == RepairFailureInvalidSchema || receipt.RepairProvenance != nil &&
		(receipt.RepairProvenance.FailureReason == RepairFailureInvalidSchema || protocolSchemaCorrectionConsumed(receipt.RepairProvenance.History))
}

func protocolRepairLimit(workFailure RepairFailureReason, history []RepairAttemptProvenance) int {
	if workFailure == RepairFailureInvalidSchema {
		return 1
	}
	return protocolRepairAttemptLimit(history)
}

func protocolSchemaCorrectionConsumed(history []RepairAttemptProvenance) bool {
	for _, attempt := range history {
		if attempt.FailureReason == RepairFailureInvalidSchema {
			return true
		}
	}
	return false
}

func protocolRepairHistory(receipt *ExecutionReceipt) []RepairAttemptProvenance {
	if receipt.RepairProvenance == nil {
		return nil
	}
	return receipt.RepairProvenance.History
}

// Persist the spent correction before generating it. Recovery can finalize an
// accepted typed result or stop on this receipt after an interrupted correction.
func (c *Coordinator) reserveProtocolSchemaCorrection(taskID, agentName, prompt string, attempt int, history []RepairAttemptProvenance) error {
	item := c.todoItemByID(taskID)
	if item == nil || item.ExecutionReceipt == nil {
		return errors.New("schema correction requires the original attempt receipt")
	}
	pending := append(append([]RepairAttemptProvenance(nil), history...), RepairAttemptProvenance{
		Attempt: attempt, InvocationRunID: c.executionRunID, Prompt: prompt, FailureReason: RepairFailureInvalidSchema,
	})
	return c.checkpointProtocolRepairAttempt(item, agentName, prompt, pending, nil, false, RepairFailureInvalidSchema, nil)
}

func (c *Coordinator) checkpointProtocolRepairAttempt(item *TodoItem, agentName, prompt string, history []RepairAttemptProvenance, result *TaskResult, success bool, reason RepairFailureReason, runErr error) error {
	receipt := ExecutionReceipt{RunID: c.executionRunID, TaskID: item.ID, Attempt: 1, OccurrenceAttempt: c.taskAttempt(item.ID), ProducerID: agentName, StartedAt: time.Now()}
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
	if receipt.ResultWorkDispatchID == "" {
		receipt.FinishedAt = time.Now()
	}
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
