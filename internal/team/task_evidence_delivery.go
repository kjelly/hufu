package team

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
)

// validateAcceptedDependencyPayload checks persisted identity without binding
// tool evidence again: downstream workers must see the already downgraded value.
func (c *Coordinator) validateAcceptedDependencyPayload(source *TodoItem) error {
	return c.validateAcceptedDependencyPayloadWithContext(context.Background(), source)
}

func (c *Coordinator) validateAcceptedDependencyPayloadWithContext(ctx context.Context, source *TodoItem) error {
	ctx, graph := withDependencyPayloadValidation(c.withRecoveryEvidenceReplay(ctx))
	return c.validateAcceptedDependencyPayloadGraph(ctx, source, graph)
}

type dependencyPayloadValidationKey struct{}

type dependencyPayloadValidation struct {
	visiting, validated map[string]bool
}

func withDependencyPayloadValidation(ctx context.Context) (context.Context, *dependencyPayloadValidation) {
	if graph, ok := ctx.Value(dependencyPayloadValidationKey{}).(*dependencyPayloadValidation); ok {
		return ctx, graph
	}
	graph := &dependencyPayloadValidation{visiting: make(map[string]bool), validated: make(map[string]bool)}
	return context.WithValue(ctx, dependencyPayloadValidationKey{}, graph), graph
}

func (c *Coordinator) validateAcceptedDependencyPayloadGraph(ctx context.Context, source *TodoItem, graph *dependencyPayloadValidation) error {
	if err := artifactReadContextError(ctx); err != nil {
		return err
	}
	if source == nil || source.TypedResult == nil {
		return fmt.Errorf("evidence source has no accepted result")
	}
	if graph.visiting[source.ID] {
		return fmt.Errorf("result evidence contains a dependency cycle at %s", source.ID)
	}
	graph.visiting[source.ID] = true
	defer delete(graph.visiting, source.ID)
	payload := source.TypedResult.StructuredPayload
	if payload == nil {
		if source.ResultContract != nil && source.ResultContract.RequireStructured {
			return fmt.Errorf("evidence source %s is missing its required structured payload", source.ID)
		}
		compiled, err := c.compiledResultContract(source.ResultContract)
		if err != nil {
			return err
		}
		omissions := resultSourceOmissions{runtimeCompletion: runtimeCompletionFailure(compiled, source.TypedResult.Status)}
		if err := c.validateIndependentResultSources(ctx, source.ID, compiled, omissions); err != nil {
			return fmt.Errorf("evidence source %s optional result sources: %w", source.ID, err)
		}
		return validateResultEnvelope(compiled, source.TypedResult)
	}
	key := source.ID + ":" + payload.SHA256 + ":" + source.TypedResult.Status
	if graph.validated[key] {
		return nil
	}
	if source.ResultContract == nil || payload.Contract != *source.ResultContract || len(payload.Value) > resultPayloadMaxBytes {
		return fmt.Errorf("evidence source %s payload does not match its admitted contract", source.ID)
	}
	sum := sha256.Sum256(payload.Value)
	if payload.SHA256 != hex.EncodeToString(sum[:]) {
		return fmt.Errorf("evidence source %s payload hash mismatch", source.ID)
	}
	compiled, err := c.acceptedDependencySchema(ctx, source, payload)
	if err != nil {
		return err
	}
	value, err := decodeResultContractJSON(payload.Value)
	if err != nil {
		return err
	}
	if err := compiled.schema.Validate(value); err != nil {
		return err
	}
	if err := validateResultEnvelope(compiled, source.TypedResult); err != nil {
		return err
	}
	if hasResultSourceBindings(compiled) {
		if err := c.validateResultSourceBindings(ctx, source, compiled, payload, value, &source.TypedResult.FilesRead, true); err != nil {
			return fmt.Errorf("evidence source %s stored result bindings: %w", source.ID, err)
		}
	} else if err := validateResultFixedCollections(value, payload.CollectionBindings, nil); err != nil {
		return err
	}
	graph.validated[key] = true
	return nil
}

func (c *Coordinator) acceptedDependencySchema(ctx context.Context, source *TodoItem, payload *ResultPayload) (*CompiledResultContract, error) {
	if legacyNormalizedContract(payload.Contract) {
		if !matchesAcceptedNormalizedReceipt(source, source.ExecutionReceipt, payload) {
			return nil, fmt.Errorf("evidence source %s lacks its original normalized result receipt", source.ID)
		}
		return c.matchingResultSchema(payload.Contract)
	}
	if !validNormalizedResultPayload(payload) {
		return nil, fmt.Errorf("evidence source %s payload hash mismatch", source.ID)
	}
	if payload.Normalization != nil {
		if err := c.validateNormalizedDependencyReceipt(ctx, source, payload); err != nil {
			return nil, err
		}
	}
	return c.compiledResultContract(source.ResultContract)
}

func (c *Coordinator) validateNormalizedDependencyReceipt(ctx context.Context, source *TodoItem, payload *ResultPayload) error {
	runID := coordinatorRuntimeRunID(c)
	receipt := latestSuccessfulExecutionReceipt(source, runID)
	if receipt == nil {
		var err error
		receipt, err = c.recoveredNormalizedDependencyReceipt(ctx, source, runID)
		if err != nil {
			return fmt.Errorf("evidence source %s normalized result receipt: %w", source.ID, err)
		}
	}
	if !matchesAcceptedNormalizedReceipt(source, receipt, payload) ||
		!validArtifactID(receipt.TranscriptRef) || !strings.EqualFold(receipt.ProducerID, source.Agent) ||
		receipt.RunInputSnapshotID != source.RunInputSnapshotID || receipt.RunInputSnapshotHash != source.RunInputSnapshotHash {
		return fmt.Errorf("evidence source %s lacks its original normalized result receipt", source.ID)
	}
	if receipt.RunID != runID {
		if err := c.validateResumeTranscript(ctx, receipt); err != nil {
			return fmt.Errorf("evidence source %s normalized result receipt transcript: %w", source.ID, err)
		}
	}
	return nil
}

func (c *Coordinator) recoveredNormalizedDependencyReceipt(ctx context.Context, source *TodoItem, runID string) (*ExecutionReceipt, error) {
	if _, selectedRun := c.terminalRunInputs(runID); selectedRun == "" {
		return nil, nil
	}
	// Authenticate the original result. The dependency view can add or remove
	// artifact references for the receiving worker.
	canonical := c.todoItemByID(source.ID)
	request := EvidenceBuildRequest{RunID: runID, Workspace: c.artifactStoreRootPath(), Items: []*TodoItem{canonical}}
	if err := c.bindRecoveredEvidence(ctx, &request); err != nil {
		return nil, err
	}
	originalRun := request.RecoveredTaskRuns[source.ID]
	if originalRun == "" {
		return nil, nil
	}
	receipt := latestSuccessfulExecutionReceipt(source, originalRun)
	if !sameCanonicalEvidence(receipt, latestSuccessfulExecutionReceipt(canonical, originalRun)) {
		return nil, fmt.Errorf("receipt differs from canonical recovered source")
	}
	return receipt, nil
}

func matchesAcceptedNormalizedReceipt(source *TodoItem, receipt *ExecutionReceipt, payload *ResultPayload) bool {
	return source.Status == TaskDone && taskResultStatusIsSuccessful(source.TypedResult.Status) &&
		receipt != nil && receipt.Succeeded() && receipt.RunID != "" && receipt.TaskID == source.ID &&
		receipt.Attempt > 0 && receipt.Attempt == source.TypedResult.Attempt && source.TypedResult.TaskID == source.ID &&
		receipt.ResultValidation == ResultValidationValid && receiptMatchesResultPayload(receipt, payload) &&
		(receipt.ResultContractID == payload.Contract.ID && receipt.ResultContractSHA256 == payload.Contract.SchemaSHA256 ||
			validContractRevalidation(source, receipt))
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

func (c *Coordinator) validateDependencyPayloads(ctx context.Context, todoID string, results []TaskResult) error {
	if err := artifactReadContextError(ctx); err != nil {
		return err
	}
	owner := c.dependencyViewOwner(todoID)
	ctx, graph := withDependencyPayloadValidation(owner.withRecoveryEvidenceReplay(ctx))
	for _, result := range results {
		source := owner.todoItemByID(result.TaskID)
		if source == nil {
			return fmt.Errorf("dependency %s has no admitted task", result.TaskID)
		}
		source.TypedResult = &result
		if err := owner.validateAcceptedDependencyPayloadGraph(ctx, source, graph); err != nil {
			return err
		}
	}
	return nil
}

func validateRequiredEvidenceContext(input WorkerContextInput, compiled CompiledContext) error {
	if err := validateSelectedDependencyContext(input, compiled); err != nil {
		return err
	}
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
func (c *Coordinator) validateRequiredEvidenceDelivery(ctx context.Context, todoID string, attempt int, runIDs ...string) error {
	var result *TaskResult
	if item := c.todoItemByID(todoID); item != nil {
		result = item.TypedResult
	}
	return c.validateRequiredEvidenceDeliveryForResult(ctx, todoID, attempt, result, runIDs...)
}

func (c *Coordinator) validateRequiredEvidenceDeliveryForResult(ctx context.Context, todoID string, attempt int, result *TaskResult, runIDs ...string) error {
	if err := artifactReadContextError(ctx); err != nil {
		return err
	}
	if err := c.validateHistoricalResultDelivery(ctx, todoID, attempt, runIDs...); err != nil {
		return err
	}
	if err := c.validateSelectedDependencyDelivery(ctx, todoID, attempt, runIDs...); err != nil {
		return err
	}
	item := c.todoItemByID(todoID)
	if item == nil {
		return nil
	}
	// Schema-owned reference bindings apply independently of the opt-in
	// context-delivery proof, including external results and replayed tasks.
	if result != nil {
		if err := c.validateResultReferenceReads(ctx, todoID, result); err != nil {
			return err
		}
		if err := c.validateEvidenceInputBindings(ctx, todoID, result.StructuredPayload, item.Status == TaskDone); err != nil {
			return err
		}
	}
	if !item.Execution.RequiresEvidence {
		return nil
	}
	task := taskDefFromTodoItem(item)
	results := c.dependencyResultsForTaskWithContext(ctx, todoID)
	if err := artifactReadContextError(ctx); err != nil {
		return err
	}
	if err := validateRequiredEvidenceInputs(task, results); err != nil {
		return err
	}
	if err := c.validateDependencyPayloads(ctx, todoID, results); err != nil {
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
		if !supportedDeliveryContextManifest(manifest) || !strings.EqualFold(manifest.Agent, strings.TrimSpace(item.Agent)) {
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
	return nil
}

func (c *Coordinator) validateCompletedEvidenceDeliveries(ctx context.Context) error {
	ctx = c.withRecoveryEvidenceReplay(ctx)
	for _, item := range c.taskTracker.TodoList().Items() {
		if err := artifactReadContextError(ctx); err != nil {
			return err
		}
		if item.Status == TaskDone {
			attempt := c.taskAttempt(item.ID)
			if item.TypedResult != nil && item.TypedResult.Attempt > 0 {
				attempt = item.TypedResult.Attempt
			}
			if err := c.validateRequiredEvidenceDelivery(ctx, item.ID, attempt); err != nil {
				return err
			}
		}
	}
	return artifactReadContextError(ctx)
}
