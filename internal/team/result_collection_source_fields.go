package team

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"slices"
)

const collectionSourceFieldRepairCheck = "source_field_recollection"

func (c *Coordinator) collectionMemoryManifest(cp *ResultCollectionCheckpoint) (*MemoryInjectionManifest, error) {
	item := c.todoItemByID(cp.TaskID)
	if item == nil {
		return nil, errResultCollectionStale
	}
	var found *MemoryInjectionManifest
	for i := range item.MemoryManifests {
		m := &item.MemoryManifests[i]
		if m.RunID != cp.SourceRunID || m.Attempt != cp.Attempt || m.ModelExecutionID != "" && m.ModelExecutionID != cp.ModelExecutionID {
			continue
		}
		if found != nil || m.TaskID != cp.TaskID || m.Agent != cp.Agent || m.RetrievalID == "" || (cp.FanoutLeaf && m.ModelExecutionID == "") {
			return nil, fmt.Errorf("collection memory manifest identity is ambiguous or invalid")
		}
		found = m
	}
	return found, nil
}

func (c *Coordinator) constrainCollectionSourceFields(cp *ResultCollectionCheckpoint, parameters map[string]any) error {
	delete(parameters, "receipt_ids")
	manifest, err := c.collectionMemoryManifest(cp)
	if err != nil {
		return err
	}
	if manifest == nil || len(manifest.Items) == 0 || !memoryLearningEnabled(c.session.Config.MemoryLearning) {
		delete(parameters, "memory_uses")
		return nil
	}
	ids := make([]string, 0, len(manifest.Items))
	for _, item := range manifest.Items {
		ids = append(ids, item.ContextItemID)
	}
	memory, _ := parameters["memory_uses"].(map[string]any)
	items, _ := memory["items"].(map[string]any)
	properties, _ := items["properties"].(map[string]any)
	if len(properties) == 0 {
		return fmt.Errorf("collection memory claim schema has no item properties")
	}
	delete(properties, "retrieval_id")
	properties["context_item_id"] = map[string]any{"type": "string", "enum": ids}
	items["additionalProperties"] = false
	memory["description"] = "Attribution to this work attempt's injected memory, if used. Omit when there is no attribution."
	return nil
}

func (c *Coordinator) collectionSourceFieldsBound(ctx context.Context, cp *ResultCollectionCheckpoint) (bool, error) {
	if cp.Request == nil {
		return false, nil
	}
	body, err := c.readCollectionBlob(ctx, cp, "request", cp.Request)
	if err != nil {
		return false, err
	}
	var marker struct {
		BoundSourceFields bool `json:"bound_source_fields"`
	}
	err = json.Unmarshal(body, &marker)
	return marker.BoundSourceFields, err
}

func (c *Coordinator) collectionStepReceipts(ctx context.Context, cp *ResultCollectionCheckpoint) (*ExecutionStepReceiptRegistry, error) {
	registry := NewExecutionStepReceiptRegistry()
	events, err := c.readActiveLineageEvents(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := LoadStructuredStepReceipts(ctx, c.artifactStoreRootPath(), events)
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		r := row.Receipt
		if r.RunID == cp.SourceRunID && r.BranchID == cp.BranchID && r.TaskID == cp.TaskID && r.Attempt == cp.Attempt && r.OccurrenceRevision == cp.WorkOccurrenceRevision {
			if err := registry.Record(r); err != nil {
				return nil, err
			}
		}
	}
	return registry, nil
}

// Bind only to the sealed work occurrence. The same operation is used by the
// read-only assessment and by the lease-backed result commit.
func (c *Coordinator) bindCollectionReceiptFields(ctx context.Context, cp *ResultCollectionCheckpoint, input *SubmitResultInput) error {
	item := c.todoItemByID(cp.TaskID)
	if item == nil {
		return rejectLocalResult(RepairFailureSource, errResultCollectionStale)
	}
	bound, err := c.collectionSourceFieldsBound(ctx, cp)
	if err != nil {
		return rejectLocalResult(RepairFailureSource, err)
	}
	input.collectionSourcesBound = bound
	if bound && input.receiptIDsSupplied {
		if len(input.ReceiptIDs) == 0 {
			return rejectLocalResult(RepairFailureInvalidSchema, fmt.Errorf("receipt_ids is runtime-owned; omit the field"))
		}
		return rejectLocalResult(RepairFailureSource, fmt.Errorf("receipt_ids is runtime-owned; omit the field"))
	}
	if !bound || len(item.Execution.Steps) == 0 {
		return nil
	}
	registry, err := c.collectionStepReceipts(ctx, cp)
	if err != nil {
		return rejectLocalResult(RepairFailureSource, err)
	}
	ids := registry.ReceiptIDs(cp.TaskID, cp.Attempt)
	if taskResultStatusIsSuccessful(input.Status) {
		err = registry.ValidateSuccessfulContract(cp.TaskID, cp.Attempt, item.Execution, ids)
	} else {
		err = registry.ValidateClaims(cp.TaskID, cp.Attempt, ids)
	}
	if err != nil {
		return rejectLocalResult(RepairFailureSource, err)
	}
	input.ReceiptIDs = ids
	return nil
}

func (c *Coordinator) validateCollectedMemoryInput(ctx context.Context, cp *ResultCollectionCheckpoint, input SubmitResultInput, result *TaskResult) error {
	if !input.memoryUsesSupplied {
		return nil
	}
	if len(result.MemoryUses) == 0 {
		manifest, err := c.collectionMemoryManifest(cp)
		if err != nil {
			return rejectLocalResult(RepairFailureSource, err)
		}
		if manifest == nil || len(manifest.Items) == 0 || !memoryLearningEnabled(c.session.Config.MemoryLearning) {
			return rejectLocalResult(RepairFailureInvalidSchema, fmt.Errorf("omit memory_uses when the original work has no attributable memory"))
		}
	}
	if slices.ContainsFunc(result.MemoryUses, func(use MemoryUseRef) bool { return use.RetrievalID != "" }) {
		return rejectLocalResult(RepairFailureSource, fmt.Errorf("memory retrieval_id is runtime-owned; omit it"))
	}
	if err := c.validateCollectedMemoryUses(ctx, cp, result); err != nil {
		return rejectLocalResult(RepairFailureSource, err)
	}
	return nil
}

func (c *Coordinator) validateCollectedMemoryUses(ctx context.Context, cp *ResultCollectionCheckpoint, result *TaskResult) error {
	if len(result.MemoryUses) == 0 {
		return nil
	}
	manifest, err := c.collectionMemoryManifest(cp)
	if err != nil {
		return err
	}
	if manifest == nil || len(manifest.Items) == 0 || !memoryLearningEnabled(c.session.Config.MemoryLearning) {
		return fmt.Errorf("memory_uses requires attributable memory from the original work attempt")
	}
	return c.bindMemoryUseClaims(ctx, cp.TaskID, cp.Agent, cp.Attempt, manifest, result)
}

// This projection is only for checking other independent claims. Never pass it
// to acceptance or replace the saved response. A new model submission must
// remove the inapplicable fields itself and pass all ordinary source checks.
func (c *Coordinator) collectionSourceFieldRepair(ctx context.Context, cp *ResultCollectionCheckpoint, compiled *CompiledResultContract, raw string) (string, string, error) {
	item := c.todoItemByID(cp.TaskID)
	if item == nil || !metadataReadOnlyTask(item.SideEffect, item.Execution) || acceptedRepairProof(cp) != nil {
		return raw, "", nil
	}
	if !collectionWorkCompleted(collectionReceipt(item, cp), true) {
		return raw, "", nil
	}
	input, err := independentRecoveryClaims(raw)
	if err != nil || input == nil {
		return raw, "", nil // Keep the ordinary independent-claims diagnostic.
	}
	var fields []string
	if len(input.ReceiptIDs) > 0 && len(item.Execution.Steps) == 0 && compiled != nil && compiled.evidenceKeys != nil {
		refs, err := c.verifiedResultEvidenceKeyRefs(ctx, cp.TaskID, compiled)
		if err != nil {
			return raw, "", err
		}
		if allCollectionReferencesObserved(input.ReceiptIDs, refs) {
			fields = append(fields, "receipt_ids")
		}
	}
	if len(input.MemoryUses) > 0 && !memoryLearningEnabled(c.session.Config.MemoryLearning) {
		manifest, err := c.collectionMemoryManifest(cp)
		if err != nil {
			return raw, "", err
		}
		if manifest == nil {
			fields = append(fields, "memory_uses")
		}
	}
	if len(fields) == 0 {
		return raw, "", nil
	}
	var claims map[string]jsontext.Value
	if err := json.Unmarshal([]byte(raw), &claims); err != nil {
		return raw, "", err
	}
	for _, field := range fields {
		delete(claims, field)
	}
	probe, err := json.Marshal(claims, json.Deterministic(true))
	return string(probe), fmt.Sprintf("omit inapplicable source fields %v in the replacement; retain all findings, judgments and evidence", fields), err
}

func allCollectionReferencesObserved(ids []string, refs map[string]ArtifactRef) bool {
	observed := make(map[string]bool, len(refs))
	for _, ref := range refs {
		observed[ref.ID] = true
	}
	return !slices.ContainsFunc(ids, func(id string) bool { return !observed[id] })
}

func assessCollectionSourceFieldRecollection(a *ResultCollectionRecoveryAssessment, diagnostic string, check func(string, error) bool) {
	check(collectionSourceFieldRepairCheck, nil)
	check("canonical_claims", fmt.Errorf("%s", diagnostic))
	a.Disposition, a.AdditionalTurns = RecoveryRepairClaims, 2
}

func hasSourceFieldRecollectionGrant(receipt *ExecutionReceipt, cp *ResultCollectionCheckpoint) bool {
	return slices.ContainsFunc(collectionRepairGrants(receipt), func(grant ResultRepairGrant) bool {
		a := grant.Recovery
		return isCollectionSourceFieldRecollection(a) && a.Checkpoint != nil && a.Checkpoint.Work == cp.Work
	})
}

func isCollectionSourceFieldRecollection(a *ResultCollectionRecoveryAssessment) bool {
	return a != nil && a.ValidatorRevision >= 17 && slices.ContainsFunc(a.Checks, func(proof ResultCollectionRecoveryCheck) bool {
		return proof.Code == collectionSourceFieldRepairCheck && proof.Passed
	})
}

func grantedSourceFieldRecollection(receipt *ExecutionReceipt, cp *ResultCollectionCheckpoint) *ResultCollectionRecoveryAssessment {
	if cp.State != ResultCollectionReceived || cp.FailureReason != RepairFailureInvalidSchema || cp.Response == nil {
		return nil
	}
	for _, grant := range collectionRepairGrants(receipt) {
		a := grant.Recovery
		if isCollectionSourceFieldRecollection(a) && a.Checkpoint != nil && a.Checkpoint.Response != nil &&
			cp.TurnsUsed == grant.AfterAttempts && *cp.Response == *a.Checkpoint.Response && validateStoredRecoveryGrant(grant, cp) == nil {
			return a
		}
	}
	return nil
}
