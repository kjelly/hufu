package team

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"
)

func (c *Coordinator) setCurrentTaskAttempt(todoID string, attempt int) {
	if c == nil || strings.TrimSpace(todoID) == "" || attempt <= 0 {
		return
	}
	identity, ok := c.taskResultOccurrenceForAttempt(todoID, attempt)
	if !ok {
		return
	}
	// The occurrence controller publishes the attempt, identity, latch, and
	// pending state under one per-todo gate. taskAttempts is only a legacy
	// snapshot for receipt-related fixtures; it is not an authorization source.
	c.openTaskOccurrence(identity)
	c.taskAttemptsMu.Lock()
	if c.taskAttempts == nil {
		c.taskAttempts = make(map[string]int)
	}
	c.taskAttempts[todoID] = attempt
	c.taskAttemptsMu.Unlock()
}

func (c *Coordinator) taskResultOccurrenceForAttempt(todoID string, attempt int) (submitResultRuntimeIdentity, bool) {
	if c == nil || c.taskTracker == nil || c.taskTracker.TodoList() == nil {
		return submitResultRuntimeIdentity{}, false
	}
	runID := strings.TrimSpace(c.executionRunID)
	if runID == "" {
		runID = strings.TrimSpace(c.taskTracker.TodoList().RunID())
	}
	if runID == "" {
		runID = "direct-" + todoID
	}
	for _, item := range c.taskTracker.TodoList().Items() {
		if item != nil && item.ID == todoID {
			agentName := strings.ToLower(strings.TrimSpace(item.Agent))
			return submitResultRuntimeIdentity{RunID: runID, TaskID: todoID, Attempt: attempt, Agent: agentName}, runID != "" && agentName != ""
		}
	}
	return submitResultRuntimeIdentity{}, false
}

func (c *Coordinator) currentTaskAttempt(todoID string) int {
	if c == nil {
		return 0
	}
	if identity, ok := c.activeTaskResultOccurrence(todoID); ok {
		return identity.Attempt
	}
	c.taskAttemptsMu.RLock()
	attempt := c.taskAttempts[todoID]
	c.taskAttemptsMu.RUnlock()
	return attempt
}

func (c *Coordinator) executionStepReceiptRegistry() *ExecutionStepReceiptRegistry {
	if c == nil {
		return nil
	}
	c.taskAttemptsMu.Lock()
	if c.stepReceipts == nil {
		c.stepReceipts = NewExecutionStepReceiptRegistry()
	}
	registry := c.stepReceipts
	c.taskAttemptsMu.Unlock()
	return registry
}

func (c *Coordinator) recordActualToolReceipt(todoID string, attempt int, toolCallID, tool, input, output string, isError bool, startedAt time.Time) error {
	if c == nil || strings.TrimSpace(todoID) == "" || attempt <= 0 || strings.TrimSpace(toolCallID) == "" {
		return nil
	}
	if startedAt.IsZero() {
		startedAt = time.Now().UTC()
	}
	exitCode := 0
	if parsed, ok := transcriptExitCode(tool, output); ok {
		exitCode = parsed
	} else if isError {
		exitCode = 1
	}
	sum := sha256.Sum256([]byte(input))
	validatorVerdict := ""
	stepID := toolCallID
	if step, ok := c.executionStepForTool(todoID, attempt, tool); ok {
		stepID = step.ID
		if step.Effect == ExecutionEffectValidate {
			validatorVerdict = "pass"
			if exitCode != 0 || isError {
				validatorVerdict = "fail"
			}
		}
	}
	policyVerdict := c.takeToolPolicyVerdict(toolCallID)
	if policyVerdict == "" {
		policyVerdict = "allowed"
	}
	receipt := ExecutionStepReceipt{
		ID:               toolCallID,
		TaskID:           todoID,
		Attempt:          attempt,
		StepID:           stepID,
		Tool:             tool,
		InputSHA256:      hex.EncodeToString(sum[:]),
		StartedAt:        startedAt.UTC(),
		FinishedAt:       time.Now().UTC(),
		ExitCode:         exitCode,
		Stdout:           output,
		PolicyVerdict:    policyVerdict,
		ValidatorVerdict: validatorVerdict,
	}
	if isError {
		receipt.Stderr = output
		receipt.Stdout = ""
	}
	return c.executionStepReceiptRegistry().Record(receipt)
}

func (c *Coordinator) executionStepForTool(todoID string, attempt int, tool string) (ExecutionStep, bool) {
	if c == nil || c.taskTracker == nil || c.taskTracker.TodoList() == nil {
		return ExecutionStep{}, false
	}
	for _, item := range c.taskTracker.TodoList().Items() {
		if item == nil || item.ID != todoID {
			continue
		}
		ordered, err := orderedStructuredExecutionSteps(item.Execution.Steps)
		if err != nil {
			return ExecutionStep{}, false
		}
		next := len(c.executionStepReceiptRegistry().ReceiptIDs(todoID, attempt))
		if next < len(ordered) && ordered[next].Tool == tool {
			return ordered[next], true
		}
		break
	}
	return ExecutionStep{}, false
}

func (c *Coordinator) setToolPolicyVerdict(toolCallID, verdict string) {
	if c == nil || strings.TrimSpace(toolCallID) == "" {
		return
	}
	c.toolPolicyVerdictsMu.Lock()
	if c.toolPolicyVerdicts == nil {
		c.toolPolicyVerdicts = make(map[string]string)
	}
	c.toolPolicyVerdicts[toolCallID] = verdict
	c.toolPolicyVerdictsMu.Unlock()
}

func (c *Coordinator) takeToolPolicyVerdict(toolCallID string) string {
	if c == nil || strings.TrimSpace(toolCallID) == "" {
		return ""
	}
	c.toolPolicyVerdictsMu.Lock()
	verdict := c.toolPolicyVerdicts[toolCallID]
	delete(c.toolPolicyVerdicts, toolCallID)
	c.toolPolicyVerdictsMu.Unlock()
	return verdict
}

func (c *Coordinator) validateTaskResultReceiptClaims(todoID string, result *TaskResult) error {
	if result == nil {
		return fmt.Errorf("task result is nil")
	}
	attempt := c.currentTaskAttempt(todoID)
	if result.Attempt > 0 && attempt > 0 && result.Attempt != attempt {
		return fmt.Errorf("result claims attempt %d while task %q is executing attempt %d", result.Attempt, todoID, attempt)
	}
	if attempt == 0 {
		attempt = result.Attempt
	}
	result.Attempt = attempt
	requiresClaims := false
	if c != nil && c.taskTracker != nil && c.taskTracker.TodoList() != nil {
		for _, item := range c.taskTracker.TodoList().Items() {
			if item != nil && item.ID == todoID {
				requiresClaims = len(item.Execution.Steps) > 0
				break
			}
		}
	}
	if !requiresClaims && len(result.ReceiptIDs) == 0 {
		return nil
	}
	if attempt <= 0 {
		return fmt.Errorf("receipt-backed result requires an active task attempt")
	}
	if requiresClaims && len(result.ReceiptIDs) == 0 {
		return fmt.Errorf("structured execution result must cite execution receipt_ids")
	}
	registry := c.executionStepReceiptRegistry()
	var err error
	if requiresClaims && taskResultStatusIsSuccessful(result.Status) {
		contract := c.executionContractForTask(todoID)
		err = registry.ValidateSuccessfulContract(todoID, attempt, contract, result.ReceiptIDs)
	} else {
		err = registry.ValidateClaims(todoID, attempt, result.ReceiptIDs)
	}
	if err != nil {
		return fmt.Errorf("invalid receipt-backed result: %w", err)
	}
	return nil
}

func (c *Coordinator) executionContractForTask(todoID string) ExecutionContract {
	if c == nil || c.taskTracker == nil || c.taskTracker.TodoList() == nil {
		return ExecutionContract{}
	}
	for _, item := range c.taskTracker.TodoList().Items() {
		if item != nil && item.ID == todoID {
			return item.Execution
		}
	}
	return ExecutionContract{}
}

func taskResultStatusIsSuccessful(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case TaskResultStatusSuccess, TaskResultStatusCompletedWithGaps:
		return true
	default:
		return false
	}
}
