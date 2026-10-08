package team

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
)

// validateAcceptedDependencyPayload checks persisted identity without binding
// tool evidence again: downstream workers must see the already downgraded value.
func (c *Coordinator) validateAcceptedDependencyPayload(source *TodoItem) error {
	if source == nil || source.TypedResult == nil {
		return fmt.Errorf("evidence source has no accepted result")
	}
	payload := source.TypedResult.StructuredPayload
	if payload == nil {
		if source.ResultContract != nil && source.ResultContract.RequireStructured {
			return fmt.Errorf("evidence source %s is missing its required structured payload", source.ID)
		}
		return nil
	}
	if source.ResultContract == nil || payload.Contract != *source.ResultContract || len(payload.Value) > resultPayloadMaxBytes {
		return fmt.Errorf("evidence source %s payload does not match its admitted contract", source.ID)
	}
	sum := sha256.Sum256(payload.Value)
	if payload.SHA256 != hex.EncodeToString(sum[:]) {
		return fmt.Errorf("evidence source %s payload hash mismatch", source.ID)
	}
	compiled, err := c.compiledResultContract(source.ResultContract)
	if err != nil {
		return err
	}
	value, err := decodeResultContractJSON(payload.Value)
	if err != nil {
		return err
	}
	return compiled.schema.Validate(value)
}

func validateRequiredEvidenceInputs(task TaskDef, results []TaskResult) error {
	if !task.Execution.RequiresEvidence {
		return nil
	}
	if len(task.EvidenceFrom) == 0 {
		return fmt.Errorf("required evidence was not delivered: evidence_from is empty")
	}
	for _, id := range task.EvidenceFrom {
		found := false
		for _, result := range results {
			if result.TaskID == id && taskResultStatusIsSuccessful(result.Status) {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("required evidence task %s was not delivered; submit partial or blocked, not success", id)
		}
	}
	return nil
}

func (c *Coordinator) validateDependencyPayloads(results []TaskResult) error {
	for _, result := range results {
		source := c.todoItemByID(result.TaskID)
		if source == nil {
			return fmt.Errorf("dependency %s has no admitted task", result.TaskID)
		}
		source.TypedResult = &result
		if err := c.validateAcceptedDependencyPayload(source); err != nil {
			return err
		}
	}
	return nil
}

func validateRequiredEvidenceContext(input WorkerContextInput, compiled CompiledContext) error {
	if !input.TaskDef.Execution.RequiresEvidence {
		return nil
	}
	if err := validateRequiredEvidenceInputs(input.TaskDef, input.DependencyResults); err != nil {
		return err
	}
	expected := FormatDependencyResults(input.DependencyResults)
	for _, item := range compiled.IncludedItems {
		if item.ID == "dependency_results" && item.Kind == "dependency_result" && item.Required && !item.Compressed && item.Content == expected && strings.Contains(compiled.Prompt, expected) {
			return nil
		}
	}
	return fmt.Errorf("required evidence context was omitted, compressed or changed; cannot accept a complete audit")
}

// The canonical manifest is the durable proof of delivery, also after replay.
// Old manifests without a dependency hash cannot prove a complete audit.
func (c *Coordinator) validateRequiredEvidenceDelivery(todoID string, attempt int, runIDs ...string) error {
	item := c.todoItemByID(todoID)
	if item == nil || !item.Execution.RequiresEvidence {
		return nil
	}
	task := taskDefFromTodoItem(item)
	results := c.dependencyResultsForTask(todoID)
	if err := validateRequiredEvidenceInputs(task, results); err != nil {
		return err
	}
	if err := c.validateDependencyPayloads(results); err != nil {
		return err
	}
	runID := ""
	if len(runIDs) > 0 {
		runID = runIDs[0]
	} else if item.ExecutionReceipt != nil && item.ExecutionReceipt.Attempt == attempt {
		runID = item.ExecutionReceipt.RunID
	}
	expectedHash := hashContentKey(FormatDependencyResults(results))
	matched := false
	for _, manifest := range item.ContextManifests {
		if manifest.TaskID != todoID || manifest.Attempt != attempt || !manifest.ModelCalled || (runID != "" && manifest.RunID != runID) {
			continue
		}
		if (manifest.Trigger != ContextTriggerTaskDispatch || manifest.Purpose != contextPurposeForTrigger(ContextTriggerTaskDispatch)) &&
			(manifest.Trigger != ContextTriggerRetry || manifest.Purpose != contextPurposeForTrigger(ContextTriggerRetry)) {
			continue
		}
		if manifest.SchemaVersion != ContextManifestSchemaVersion || !strings.EqualFold(manifest.Agent, strings.TrimSpace(item.Agent)) || manifest.Fingerprint != contextManifestFingerprint(manifest) {
			return fmt.Errorf("required evidence delivery manifest identity is invalid")
		}
		// Every model in a fanout must receive the same full evidence. Repair
		// and auxiliary manifests do not stand in for the original dispatch.
		delivered := false
		for _, entry := range manifest.Items {
			if entry.ID == "dependency_results" && entry.Kind == "dependency_result" && entry.Included && !entry.Compressed && entry.ContentHash == expectedHash {
				delivered = true
			}
		}
		if !delivered {
			return fmt.Errorf("required evidence was not fully delivered to task %s attempt %d; submit partial or blocked, not success", todoID, attempt)
		}
		matched = true
	}
	if !matched {
		return fmt.Errorf("required evidence delivery is unproven for task %s attempt %d; submit partial or blocked", todoID, attempt)
	}
	if item.TypedResult != nil {
		if err := c.validateSubmittedEvidenceInputs(todoID, item.TypedResult.StructuredPayload); err != nil {
			return err
		}
	}
	return nil
}

func (c *Coordinator) validateCompletedEvidenceDeliveries() error {
	for _, item := range c.taskTracker.TodoList().Items() {
		if item.Status == TaskDone && item.Execution.RequiresEvidence {
			attempt := c.taskAttempt(item.ID)
			if item.TypedResult != nil && item.TypedResult.Attempt > 0 {
				attempt = item.TypedResult.Attempt
			}
			if err := c.validateRequiredEvidenceDelivery(item.ID, attempt); err != nil {
				return err
			}
		}
	}
	return nil
}
