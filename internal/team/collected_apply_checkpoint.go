package team

import (
	"context"
	jsonv1 "encoding/json"
	"encoding/json/v2"
	"errors"
	"fmt"
)

// Collection acceptance precedes verification. The verified apply gets its
// own immutable record before the lifecycle event can authorize effects.
type collectedApplyCheckpoint struct {
	Apply          attemptApplyStartedPayload `json:"apply"`
	ManifestSHA256 string                     `json:"manifest_sha256"`
}

func isCommittedNormalizedCollection(item *TodoItem) bool {
	if item == nil || item.ResultContract == nil || item.ResultContract.Normalizer == "" {
		return false
	}
	checkpoint := currentResultCollection(item)
	return checkpoint != nil && checkpoint.State == ResultCollectionCommitted
}

func collectedApplyKey(cp *ResultCollectionCheckpoint) string {
	return "collected-apply/" + directNormalizationCheckpointDocument(cp.BranchID, cp.TaskID, cp.WorkOccurrenceRevision, cp.WorkDispatchID)
}

func (c *Coordinator) readCollectedApply(cp *ResultCollectionCheckpoint, worldDir string) (*collectedApplyCheckpoint, preparedApplyIntent, error) {
	key := collectedApplyKey(cp)
	body, found, err := workspaceDocs(c.session.Workspace).getDocument(key)
	if err != nil {
		return nil, preparedApplyIntent{}, err
	}
	var digest string
	if found {
		digest = sha256HexBytes(body)
	}
	intent, err := readPreparedApplyIntent(c.artifactStoreRootPath(), key, worldDir, digest)
	if err != nil || !found {
		return nil, intent, err
	}
	var checkpoint collectedApplyCheckpoint
	if json.Unmarshal(body, &checkpoint, jsonv1.DefaultOptionsV1(), json.RejectUnknownMembers(true)) != nil ||
		!validCollectionDigest(checkpoint.ManifestSHA256) || !validCollectionDigest(checkpoint.Apply.DeltaDigest) {
		return nil, intent, errors.New("committed collection apply checkpoint is invalid")
	}
	return &checkpoint, intent, nil
}

func (checkpoint *collectedApplyCheckpoint) readDelta(worldDir, worldID, baseline string) (*AttemptWorkspaceDelta, error) {
	return readVerifiedAttemptDelta(worldDir, worldID, baseline, checkpoint.Apply.DeltaDigest, checkpoint.ManifestSHA256)
}

func (checkpoint *collectedApplyCheckpoint) matches(started attemptApplyStartedPayload) bool {
	expected, expectedErr := normalizedApplyDigest(checkpoint.Apply)
	actual, actualErr := normalizedApplyDigest(started)
	return actualErr == nil && expectedErr == nil && expected == actual
}

func (c *Coordinator) prepareCollectedApply(ctx context.Context, isolated *isolatedAttempt, item *TodoItem, cp *ResultCollectionCheckpoint, started *attemptApplyStartedPayload) (*AttemptWorkspaceDelta, error) {
	checkpoint, intent, err := c.readCollectedApply(cp, isolated.worldDir())
	if err != nil {
		return nil, rejectNormalizationRecoveryRead(err)
	}
	if err := c.validateCollectedApplySources(ctx, item, cp, isolated.worldID(), *started); err != nil {
		return nil, rejectNormalizationRecoveryRead(err)
	}
	verifiedManifest, err := c.collectedVerifiedWorkspaceManifest(ctx, item, *started, intent.published)
	if err != nil {
		return nil, err
	}
	root, key := c.artifactStoreRootPath(), collectedApplyKey(cp)
	if checkpoint != nil {
		started.DeltaDigest = checkpoint.Apply.DeltaDigest
		if !checkpoint.matches(*started) || verifiedManifest != "" && verifiedManifest != checkpoint.ManifestSHA256 {
			return nil, rejectResultSourceRead(errors.New("collection apply differs from the committed verification"))
		}
		delta, err := checkpoint.readDelta(isolated.worldDir(), isolated.worldID(), isolated.prepared.isolated.baseline.digest())
		if err != nil {
			return nil, rejectNormalizationRecoveryRead(err)
		}
		if !intent.published {
			return delta, publishRuntimeEffectProjection(root, key, intent.record.SealSHA256)
		}
		return delta, nil
	}
	delta, err := intent.loadDelta(ctx, isolated, verifiedManifest)
	if err != nil {
		return nil, err
	}
	started.DeltaDigest = delta.Digest
	digest, err := normalizedDeltaDigest(delta)
	if err != nil {
		return nil, rejectResultSourceRead(err)
	}
	body, err := marshalNormalizedCheckpoint(collectedApplyCheckpoint{Apply: *started, ManifestSHA256: digest})
	if err != nil {
		return nil, rejectLocalResultPersistence(err)
	}
	seal := sha256HexBytes(body)
	if err := intent.seal(root, key, isolated.worldDir(), seal, delta); err != nil {
		return nil, err
	}
	if err := c.sealNormalizedHandoff(ctx, key, body); err != nil {
		return nil, err
	}
	return delta, publishRuntimeEffectProjection(root, key, seal)
}

func (c *Coordinator) collectedVerifiedWorkspaceManifest(ctx context.Context, item *TodoItem, started attemptApplyStartedPayload, publishedApply bool) (string, error) {
	completion, _, _, err := c.loadVerificationCompletion(ctx, item, started.TypedResult, started.CoordinatorOutput)
	if err != nil {
		return "", err
	}
	if !completion.verified(item, started.VerifyResult) {
		return "", rejectResultSourceRead(errors.New("collection apply lacks completed verification evidence"))
	}
	return completion.verifiedWorkspaceManifest(item, publishedApply)
}

func (c *Coordinator) reconcilePreparedCollectedApply(ctx context.Context, worldDir string, owner attemptWorldOwner, record *attemptWorldRecord, item *TodoItem, cp *ResultCollectionCheckpoint) error {
	if item.ExecutionReceipt == nil || item.ExecutionReceipt.ExecutionWorldID != owner.WorldID {
		return nil
	}
	checkpoint, _, err := c.readCollectedApply(cp, worldDir)
	if err != nil {
		return c.settleNormalizedRecoveryRead(item, taskDefFromTodoItem(item), rejectNormalizationRecoveryRead(err))
	}
	if checkpoint == nil || owner.WorldID != checkpoint.Apply.WorldID {
		return nil
	}
	if owner.RunID != cp.SourceRunID || owner.TaskID != cp.TaskID || owner.Attempt != cp.Attempt ||
		owner.OccurrenceAttempt != item.Retries+1 || record.BranchID != cp.BranchID {
		return c.settleNormalizedRecoveryRead(item, taskDefFromTodoItem(item), rejectResultSourceRead(errors.New("verified collection apply has a different workspace owner")))
	}
	if err := c.verifyNormalizedCollectionTerminal(ctx, item, taskDefFromTodoItem(item), checkpoint.Apply.CoordinatorOutput); err != nil {
		return err
	}
	world := NewIsolatedCopyExecutionWorld()
	prepared, _, err := world.Load(worldDir, record.BaselineDigest)
	if err != nil {
		return c.settleNormalizedRecoveryRead(item, taskDefFromTodoItem(item), rejectNormalizationRecoveryRead(err))
	}
	attempt := &isolatedAttempt{world: world, prepared: prepared, taskID: item.ID, attempt: record.Attempt}
	started := checkpoint.Apply
	evidence := isolatedApplyEvidence{TypedResult: started.TypedResult, VerifyResult: started.VerifyResult, Receipt: started.ExecutionReceipt, CoordinatorOutput: started.CoordinatorOutput}
	if err := c.integrateIsolatedAttempt(ctx, attempt, evidence); err != nil {
		if _, ok := errors.AsType[*localResultAdmissionError](err); ok {
			return c.settleNormalizedRecoveryRead(item, taskDefFromTodoItem(item), err)
		}
		if _, conflict := errors.AsType[*AttemptWorkspaceConflictError](err); conflict {
			blocked := TaskBlocked
			return c.persistFailureWithOutput(item.Agent, item.Desc, item.ID, c.FailureDetail(err, FailureSourceError), NeedsHuman, FailureWorkspaceConflict, &blocked, item.Output)
		}
		return fmt.Errorf("resume verified collection apply: %w", err)
	}
	return c.finishRecoveredAttempt(ctx, worldDir, owner, item, started, true)
}
