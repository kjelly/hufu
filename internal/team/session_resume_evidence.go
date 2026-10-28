package team

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"slices"
)

// validateResumeSources runs before any provider boundary. Completed sources
// must match canonical replay, their frozen inputs, contract and sealed bytes.
func (c *Coordinator) validateResumeSources(ctx context.Context) error {
	resume, _ := ctx.Value(sessionResumeKey{}).(bool)
	if !resume {
		return nil
	}
	workspace := c.session.Workspace
	fail := func(err error) error { return ResumeSourceError(workspace, err) }
	source, err := ReadResumeSource(workspace, c.activeBranchID(), "")
	if err != nil {
		return err
	}
	items := c.taskTracker.TodoList().Items()
	canonical := make(map[string]*TodoItem, len(source.Session.Tasks))
	for _, item := range source.Session.Tasks {
		if !IsPrimaryOccurrence(item) {
			canonical[item.ID] = item
		}
	}
	for _, item := range items {
		if IsPrimaryOccurrence(item) {
			continue
		}
		stored := canonical[item.ID]
		if !sameResumeTaskEvidence(stored, item) {
			return fail(fmt.Errorf("loaded task %s does not match branch %q canonical replay", item.ID, source.BranchID))
		}
		delete(canonical, item.ID)
	}
	if len(canonical) != 0 {
		return fail(fmt.Errorf("loaded tasks omit canonical tasks in branch %q", source.BranchID))
	}
	snapshot := c.RunInputSnapshot()
	if snapshot == nil || ValidateRunInputSnapshot(snapshot) != nil {
		return fail(fmt.Errorf("selected branch has no valid frozen inputs"))
	}
	if snapshot.Team != c.session.Config.Name || !slices.ContainsFunc(source.Session.RunInputSnapshots, func(original RunInputSnapshot) bool {
		return original.ID == snapshot.ID && sameCanonicalEvidence(&original, snapshot)
	}) {
		return fail(fmt.Errorf("frozen inputs have no matching canonical source for this team and branch"))
	}
	for _, item := range items {
		if item == nil || IsPrimaryOccurrence(item) {
			continue
		}
		if _, _, err := c.boundResultContract(item.ID); err != nil {
			return fail(fmt.Errorf("task %s contract: %w", item.ID, err))
		}
		if item.RunInputSnapshotID != snapshot.ID || item.RunInputSnapshotHash != snapshot.SnapshotHash {
			return fail(fmt.Errorf("task %s frozen inputs differ from the selected recovery snapshot", item.ID))
		}
		if item.Status != TaskDone {
			if checkpoint := currentResultCollection(item); checkpoint != nil {
				if _, err := c.loadCollectionWork(ctx, item, checkpoint); err != nil {
					return fail(fmt.Errorf("task %s collection evidence: %w", item.ID, err))
				}
				if checkpoint.State == ResultCollectionCommitted {
					if _, err := c.acceptedCollectionResult(ctx, item, checkpoint); err != nil {
						return fail(fmt.Errorf("task %s accepted collection: %w", item.ID, err))
					}
				}
			}

			if item.ResultCandidate != nil {
				if _, err := c.loadResultCandidate(ctx, item, item.ResultCandidate); err != nil {
					return fail(fmt.Errorf("task %s candidate: %w", item.ID, err))
				}
			}

			if !item.ExecutionTarget.IsZero() {
				if _, err := c.ResolveBackendExecutionPolicy(item.ExecutionTarget); err != nil {
					return fail(fmt.Errorf("task %s target admission: %w", item.ID, err))
				}
			}
			continue
		}
		if err := c.validateCompletedResumeEvidence(ctx, item, snapshot); err != nil {
			return fail(err)
		}
	}
	if taskID, _ := ctx.Value(resultOnlyRepairKey{}).(string); taskID != "" {
		if err := c.validateResumeRepairSources(ctx, taskID); err != nil {
			return fail(err)
		}
	}

	return nil
}

func (c *Coordinator) validateCompletedResumeEvidence(ctx context.Context, item *TodoItem, snapshot *RunInputSnapshot) error {
	if item.TypedResult == nil || !taskResultStatusIsSuccessful(item.TypedResult.Status) {
		return fmt.Errorf("task %s has no accepted successful result", item.ID)
	}
	if err := c.validateAcceptedDependencyPayload(item); err != nil {
		return fmt.Errorf("task %s result: %w", item.ID, err)
	}
	receipt := recoveredEvidenceReceipt(item, snapshot)
	if receipt == nil {
		return fmt.Errorf("task %s has no successful input-bound original receipt", item.ID)
	}
	if item.TypedResult.StructuredPayload != nil && receipt.ResultPayloadSHA256 != item.TypedResult.StructuredPayload.SHA256 {
		return fmt.Errorf("task %s receipt payload hash differs from its accepted result", item.ID)
	}
	if ref := item.ResultContract; ref != nil && (receipt.ResultContractID != ref.ID || receipt.ResultContractSHA256 != ref.SchemaSHA256) {
		return fmt.Errorf("task %s receipt does not match the frozen result contract", item.ID)
	}
	if item.TypedResult.StructuredPayload != nil && receipt.ResultValidation != ResultValidationValid {
		return fmt.Errorf("task %s receipt has no accepted structured result validation", item.ID)
	}
	if err := c.validateResumeTranscript(ctx, receipt); err != nil {
		return fmt.Errorf("task %s transcript: %w", item.ID, err)
	}
	return nil
}

func sameResumeTaskEvidence(stored, loaded *TodoItem) bool {
	return stored != nil && loaded != nil && stored.ID == loaded.ID && stored.Status == loaded.Status && stored.Agent == loaded.Agent &&
		stored.ContractID == loaded.ContractID && stored.ContractHash == loaded.ContractHash && stored.Retries == loaded.Retries &&
		stored.ExecutionTarget == loaded.ExecutionTarget && sameCanonicalEvidence(stored.ExecutionTopology, loaded.ExecutionTopology) &&
		stored.RunInputSnapshotID == loaded.RunInputSnapshotID && stored.RunInputSnapshotHash == loaded.RunInputSnapshotHash &&
		sameCanonicalEvidence(stored.ResultCandidate, loaded.ResultCandidate) && sameCanonicalEvidence(stored.ResultContract, loaded.ResultContract) && sameCanonicalEvidence(stored.TypedResult, loaded.TypedResult) &&
		sameCanonicalEvidence(stored.ExecutionReceipt, loaded.ExecutionReceipt) && sameCanonicalEvidence(stored.ExecutionReceipts, loaded.ExecutionReceipts) &&
		sameCanonicalEvidence(stored.VerifyResult, loaded.VerifyResult) && sameCanonicalEvidence(stored.EvidenceFrom, loaded.EvidenceFrom) &&
		sameCanonicalEvidence(stored.DependsOn, loaded.DependsOn) && sameCanonicalEvidence(stored.WorksetBinding, loaded.WorksetBinding) &&
		sameCanonicalEvidence(taskDefFromTodoItem(stored), taskDefFromTodoItem(loaded))
}

func (c *Coordinator) validateResumeTranscript(ctx context.Context, receipt *ExecutionReceipt) error {
	store, err := OpenFileArtifactStoreReadOnly(c.artifactStoreRootPath(), c.artifactStoreRootPath())
	if err != nil {
		return err
	}
	ref, err := store.Get(ctx, receipt.TranscriptRef)
	if err != nil {
		return err
	}
	if ref.RunID != receipt.RunID || ref.TaskID != receipt.TaskID || ref.Agent != receipt.ProducerID || ref.Attempt != receipt.Attempt ||
		ref.Bytes < 0 || ref.Bytes > 16<<20 || receipt.TranscriptRef != "sha256-"+ref.SHA256 {
		return fmt.Errorf("original transcript identity/size/digest does not match receipt")
	}
	body, err := store.Open(ctx, receipt.TranscriptRef)
	if err != nil {
		return err
	}
	defer func() { _ = body.Close() }()
	hash := sha256.New()
	n, err := io.Copy(hash, io.LimitReader(body, (16<<20)+1))
	if err != nil {
		return err
	}
	if n != ref.Bytes || hex.EncodeToString(hash.Sum(nil)) != ref.SHA256 {
		return fmt.Errorf("original transcript bytes do not match receipt")
	}
	return nil
}

func (c *Coordinator) validateResumeRepairSources(ctx context.Context, taskID string) error {
	if model, _ := ctx.Value(resultRepairModelKey{}).(string); model != "" {
		if _, err := c.resultRepairTarget(model); err != nil {
			return err
		}
	}
	item := c.todoItemByID(taskID)
	if currentResultCollection(item) != nil {
		if model, _ := ctx.Value(resultRepairModelKey{}).(string); model != "" {
			return fmt.Errorf("collection repair must retain its original target")
		}
		assessment, err := c.AssessResultCollectionRecovery(ctx, taskID)
		if err != nil {
			return err
		}
		if err := validateRecoveryAssessment(assessment); err != nil {
			return fmt.Errorf("task %s recovery blocked: %v: %w", taskID, assessment.Checks, err)
		}
		return nil
	}
	if err := validateResultRepairTarget(item); err != nil {
		return fmt.Errorf("task %s: %w", taskID, err)
	}
	if err := c.validateResumeTranscript(ctx, item.ExecutionReceipt); err != nil {
		return fmt.Errorf("task %s: %w", taskID, err)
	}
	if _, err := c.restoreProtocolRepairToolEvidence(ctx, item); err != nil {
		return fmt.Errorf("task %s tool evidence: %w", taskID, err)
	}
	results := c.dependencyResultsForTask(taskID)
	if len(results) != len(resultDependencyIDs(item)) {
		return fmt.Errorf("task %s lacks accepted evidence_from results", taskID)
	}
	if err := c.validateDependencyPayloads(results); err != nil {
		return err
	}
	return nil
}
