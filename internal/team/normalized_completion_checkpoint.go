package team

import (
	"context"
	jsonv1 "encoding/json"
	"encoding/json/v2"
	"errors"
	"fmt"

	"github.com/kjelly/hufu/internal/agent"
)

// This record belongs to the original work dispatch, even when a new
// publication lease accepted its result. Save independently of lifecycle
// events so a failed append can be resumed without repeating completed checks.
type normalizedCompletionCheckpoint struct {
	BindingSHA256       string                     `json:"binding_sha256"`
	ChecksSHA256        string                     `json:"checks_sha256"`
	Objective           normalizedObjectiveOutcome `json:"objective,omitzero"`
	Skeptic             normalizedCheckOutcome     `json:"skeptic,omitzero"`
	DeltaSHA256         string                     `json:"delta_sha256,omitempty"`
	DeltaManifestSHA256 string                     `json:"delta_manifest_sha256,omitempty"`
	ApplySHA256         string                     `json:"apply_sha256,omitempty"`

	key           string
	docs          workspaceDocs
	root          string
	pendingChecks []pendingVerificationPublication
}

const (
	objectiveVerification   = "objective verification"
	adversarialVerification = "adversarial verification"
)

type normalizedCheckOutcome struct {
	Started                 bool   `json:"started,omitempty"`
	Completed               bool   `json:"completed,omitempty"`
	Error                   string `json:"error,omitempty"`
	WorkspaceManifestSHA256 string `json:"workspace_manifest_sha256,omitempty"`
}

func (outcome normalizedCheckOutcome) err() error {
	if outcome.Error == "" {
		return nil
	}
	return errors.New(outcome.Error)
}

type normalizedObjectiveOutcome struct {
	normalizedCheckOutcome
	Result *VerificationResult `json:"result,omitempty"`
}

func (c *Coordinator) loadNormalizedCompletion(taskID string, result *TaskResult, collectionOutput string) (*normalizedCompletionCheckpoint, error) {
	return c.loadBoundNormalizedCompletion(c.todoItemByID(taskID), result, collectionOutput)
}

func (c *Coordinator) loadBoundNormalizedCompletion(item *TodoItem, result *TaskResult, collectionOutput string) (*normalizedCompletionCheckpoint, error) {
	key, digest, err := normalizedCompletionBinding(c.activeBranchID(), item, result, collectionOutput)
	if err != nil {
		return nil, err
	}
	checkpoint, found, err := readNormalizedCompletionCheckpoint(workspaceDocs(c.session.Workspace), c.artifactStoreRootPath(), key)
	if err != nil {
		return nil, err
	}
	if found {
		if err := checkpoint.validateBinding(digest); err != nil {
			return nil, err
		}
	}
	checkpoint.BindingSHA256 = digest
	if err := checkpoint.restoreCheckEvidence(); err != nil {
		return nil, err
	}
	return &checkpoint, nil
}

func normalizedCompletionBinding(branch string, item *TodoItem, result *TaskResult, collectionOutput string) (key, digest string, err error) {
	source, collection, ok := normalizedCompletionIdentity(item, result)
	if !ok {
		return "", "", rejectResultSourceRead(errors.New("normalized completion lacks a matching accepted work receipt"))
	}
	binding := struct {
		Source         submitResultRuntimeIdentity
		Branch         string
		SnapshotSHA256 string
		WorldID        string
		Contract       *ResultContractRef
		Result         *TaskResult
		Verify         string
		VerifyMode     string
		VerifySpec     *agent.VerificationSpec
		Adversarial    int
		Collection     *ResultCollectionCheckpoint `json:"collection,omitempty"`
		CollectionView string                      `json:"collection_view,omitempty"`
	}{
		Source: source, Branch: branch, SnapshotSHA256: item.RunInputSnapshotHash, WorldID: item.ExecutionReceipt.ExecutionWorldID, Contract: item.ResultContract, Result: result,
		Verify: item.Verify, VerifyMode: item.VerifyMode, VerifySpec: item.VerifySpec, Adversarial: item.AdversarialVerify,
		Collection: collection,
	}
	if collection != nil {
		if collection.BranchID != branch {
			return "", "", rejectResultSourceRead(errors.New("normalized collection completion lacks its original branch"))
		}
		binding.CollectionView = collectionOutput
	}
	body, err := marshalNormalizedCheckpoint(binding)
	if err != nil {
		return "", "", rejectResultSourceRead(err)
	}
	return normalizedCompletionKey(branch, source), sha256HexBytes(body), nil
}

func (checkpoint normalizedCompletionCheckpoint) validateBinding(digest string) error {
	if checkpoint.BindingSHA256 != digest {
		return rejectResultSourceRead(errors.New("normalized completion checkpoint does not match the accepted work"))
	}
	if actual, err := checkpoint.checksDigest(); err != nil || actual != checkpoint.ChecksSHA256 {
		return rejectResultSourceRead(errors.New("normalized completion checkpoint does not match the sealed verification outcome"))
	}
	return nil
}

func normalizedCompletionKey(branch string, source submitResultRuntimeIdentity) string {
	return "normalized-completion/" + directNormalizationCheckpointDocument(branch, source.TaskID, source.OccurrenceRevision, source.DispatchID)
}

func readNormalizedCompletionCheckpoint(docs workspaceDocs, root, key string) (normalizedCompletionCheckpoint, bool, error) {
	checkpoint := normalizedCompletionCheckpoint{docs: docs, root: root, key: key}
	body, found, err := docs.getDocument(key)
	if err != nil {
		return checkpoint, false, rejectNormalizationRecoveryRead(err)
	}
	if !found {
		return checkpoint, false, nil
	}
	if json.Unmarshal(body, &checkpoint, jsonv1.DefaultOptionsV1(), json.RejectUnknownMembers(true)) != nil || !validCollectionDigest(checkpoint.BindingSHA256) ||
		!checkpoint.Objective.valid() || checkpoint.Objective.Result != nil && !checkpoint.Objective.Completed || !checkpoint.Skeptic.valid() || !checkpoint.validApplyBinding() {
		return checkpoint, false, rejectResultSourceRead(errors.New("invalid normalized completion checkpoint"))
	}
	return checkpoint, true, nil
}

func (c *Coordinator) readFanoutCheckProgress(item *TodoItem, cp *ResultCollectionCheckpoint, work *resultCollectionWork, result *TaskResult, projection *RuntimeMediaProjection) (*normalizedCompletionCheckpoint, error) {
	docs := workspaceDocs(work.Workspace)
	key := normalizedCompletionKey(cp.BranchID, collectionWorkIdentity(cp))
	checkpoint, found, err := readNormalizedCompletionCheckpoint(docs, c.artifactStoreRootPath(), key)
	if err != nil {
		return nil, err
	}
	leaf := *item
	leaf.ExecutionReceipt = collectionReceipt(item, cp)
	result, output := fanoutVerifierInput(&leaf, work, result, projection)
	_, digest, err := normalizedCompletionBinding(cp.BranchID, &leaf, result, output)
	if err != nil {
		return nil, err
	}
	if found {
		if err := checkpoint.validateBinding(digest); err != nil {
			return nil, err
		}
	}
	checkpoint.BindingSHA256 = digest
	if err := checkpoint.restoreCheckEvidence(); err != nil {
		return nil, err
	}
	if !found && !checkpoint.Objective.Started && !checkpoint.Skeptic.Started {
		return nil, nil
	}
	if checkpoint.Objective.Started && !checkpoint.Objective.Completed {
		return nil, unknownVerificationOutcome(objectiveVerification)
	}
	if checkpoint.Skeptic.Started && !checkpoint.Skeptic.Completed {
		return nil, unknownVerificationOutcome(adversarialVerification)
	}
	return &checkpoint, nil
}

// Both live verification and cold preflight derive the input from the sealed
// collection and its original media projection.
func fanoutVerifierInput(item *TodoItem, work *resultCollectionWork, result *TaskResult, projection *RuntimeMediaProjection) (*TaskResult, string) {
	if projection != nil {
		result = cloneTaskResult(result)
		attachTaskRuntimeMedia(result, projection)
	}
	if taskUsesVerbatimTranscript(taskDefFromTodoItem(item)) {
		return result, formatVerbatimTranscriptManifest(result.RawOutputRef)
	}
	return result, coordinatorTaskOutput(string(work.Report), result)
}

func (c *Coordinator) loadVerificationCompletion(ctx context.Context, item *TodoItem, result *TaskResult, output string) (*normalizedCompletionCheckpoint, *TaskResult, string, error) {
	if cp := currentResultCollection(item); cp != nil && cp.FanoutLeaf {
		work, err := c.loadCollectionWork(ctx, item, cp)
		if err != nil {
			return nil, nil, "", rejectNormalizationRecoveryRead(err)
		}
		accepted, err := c.readAcceptedCollectionResult(ctx, item, cp)
		if err != nil {
			return nil, nil, "", rejectNormalizationRecoveryRead(err)
		}
		projection, err := c.verifyCollectedWorkEvidence(ctx, cp, accepted, item.ExecutionReceipt)
		if err != nil {
			return nil, nil, "", err
		}
		result, output = fanoutVerifierInput(item, work, accepted, projection)
	}
	checkpoint, err := c.loadBoundNormalizedCompletion(item, result, output)
	return checkpoint, result, output, err
}

func normalizedCompletionIdentity(item *TodoItem, result *TaskResult) (submitResultRuntimeIdentity, *ResultCollectionCheckpoint, bool) {
	if cp := currentResultCollection(item); cp != nil && cp.State == ResultCollectionCommitted && (cp.FanoutLeaf || isCommittedNormalizedCollection(item)) {
		source := collectionWorkIdentity(cp)
		receipt := collectionReceipt(item, cp)
		if receipt == nil || !validSubmitResultIdentity(source) || !isCompletedLocalResult(result) {
			return source, cp, false
		}
		if payload := result.StructuredPayload; payload != nil {
			return source, cp, item.ResultContract != nil && payload.Contract == *item.ResultContract && collectionReceiptMatchesResultPayload(receipt, cp, payload)
		}
		return source, cp, (item.ResultContract == nil || !item.ResultContract.RequireStructured) && receipt.ResultPayloadSHA256 == "" && receipt.ResultRawPayloadSHA256 == "" &&
			receipt.ResultNormalizerOutputSHA256 == "" && receipt.ResultNormalizerOccurrenceSHA256 == ""
	}
	source, ok := normalizationWorkEvidenceIdentity(item)
	return source, nil, ok && normalizedWorkerVerificationHandoff(item.ResultContract, item.ExecutionReceipt, result)
}

func (checkpoint *normalizedCompletionCheckpoint) validApplyBinding() bool {
	if checkpoint.DeltaSHA256 == "" && checkpoint.DeltaManifestSHA256 == "" && checkpoint.ApplySHA256 == "" {
		return true
	}
	return validCollectionDigest(checkpoint.DeltaSHA256) && validCollectionDigest(checkpoint.DeltaManifestSHA256) && validCollectionDigest(checkpoint.ApplySHA256)
}

func (checkpoint *normalizedCompletionCheckpoint) applySealDigest() string {
	if checkpoint.DeltaSHA256 == "" {
		return ""
	}
	return sha256HexBytes([]byte(checkpoint.BindingSHA256 +
		checkpoint.DeltaSHA256 + checkpoint.DeltaManifestSHA256 + checkpoint.ApplySHA256))
}

func (checkpoint *normalizedCompletionCheckpoint) validateApplyEvidence(worldDir string) error {
	intent, err := readPreparedApplyIntent(checkpoint.root, checkpoint.key+"/apply-intent", worldDir, checkpoint.applySealDigest())
	if err != nil {
		return err
	}
	if !intent.published {
		return rejectResultSourceRead(errors.New("normalized apply intent was never published"))
	}
	return nil
}

func (outcome normalizedCheckOutcome) valid() bool {
	return (!outcome.Completed || outcome.Started) && (outcome.Error == "" || outcome.Completed) &&
		(outcome.WorkspaceManifestSHA256 == "" || outcome.Completed && outcome.Error == "" && validCollectionDigest(outcome.WorkspaceManifestSHA256))
}

func (checkpoint *normalizedCompletionCheckpoint) checksDigest() (string, error) {
	body, err := marshalNormalizedCheckpoint(struct {
		Binding   string
		Objective normalizedObjectiveOutcome
		Skeptic   normalizedCheckOutcome
	}{checkpoint.BindingSHA256, checkpoint.Objective, checkpoint.Skeptic})
	if err != nil {
		return "", err
	}
	return sha256HexBytes(body), nil
}

func (checkpoint *normalizedCompletionCheckpoint) save() error {
	digest, err := checkpoint.checksDigest()
	if err != nil {
		return rejectLocalResultPersistence(fmt.Errorf("seal normalized completion checks: %w", err))
	}
	checkpoint.ChecksSHA256 = digest
	body, err := marshalNormalizedCheckpoint(checkpoint)
	if err == nil {
		err = checkpoint.docs.putDocument(checkpoint.key, body)
	}
	if err != nil {
		return rejectLocalResultPersistence(fmt.Errorf("save normalized completion checkpoint: %w", err))
	}
	return nil
}

func (checkpoint *normalizedCompletionCheckpoint) startCheck(outcome *normalizedCheckOutcome, name string) error {
	if outcome.Started {
		return unknownVerificationOutcome(name)
	}
	if err := checkpoint.createCheckStart(name); err != nil {
		return err
	}
	outcome.Started = true
	return checkpoint.save()
}

func unknownVerificationOutcome(name string) error {
	return rejectResultSourceRead(fmt.Errorf("completion of the previously started %s cannot be established", name))
}

func (checkpoint *normalizedCompletionCheckpoint) recordCheck(name string, outcome *normalizedCheckOutcome, detail string) error {
	outcome.Completed, outcome.Error = true, detail
	if err := checkpoint.sealCheckOutcome(name); err != nil {
		return err
	}
	return checkpoint.publishPendingChecks()
}

func (c *Coordinator) verifyNormalizedDeliverable(ctx context.Context, taskID string, agentDef *agent.AgentDef, task TaskDef, result *TaskResult, output string) (*VerificationResult, error) {
	if err := ValidateVerificationAssumptionRefs(task.VerifySpec); err != nil {
		return nil, err
	}
	item := c.todoItemByID(taskID)
	checkpoint, result, _, err := c.loadVerificationCompletion(ctx, item, result, output)
	if err != nil {
		return nil, err
	}
	if err := checkpoint.publishPendingChecks(); err != nil {
		return nil, err
	}
	outcome := &checkpoint.Objective
	if !outcome.Completed {
		steps, err := c.normalizedVerificationSteps(ctx, item, task)
		if err != nil {
			return nil, err
		}
		if err := checkpoint.startCheck(&outcome.normalizedCheckOutcome, objectiveVerification); err != nil {
			return nil, err
		}
		var verificationErr error
		outcome.Result, verificationErr = c.runTaskDeliverableVerification(ctx, agentDef, task, steps, result)
		detail := ""
		if verificationErr != nil {
			detail = c.FailureDetail(verificationErr, FailureSourceError)
		}
		if verificationErr == nil {
			outcome.WorkspaceManifestSHA256, err = verificationWorkspaceManifest(ctx, "")
			if err != nil {
				return nil, err
			}
		}
		if err := checkpoint.recordCheck(objectiveVerification, &outcome.normalizedCheckOutcome, detail); err != nil {
			return nil, err
		}
	}
	if task.VerifySpec != nil && len(task.VerifySpec.AssumptionRefs) != 0 {
		passed := outcome.Error == "" && outcome.Result != nil && outcome.Result.ExitCode == 0
		checks := AssumptionChecksFromVerification(task.VerifySpec, passed)
		if _, err := c.ApplyAssumptionChecks(ctx, taskID, checks, AssumptionSourceVerification); err != nil {
			var storage *assumptionCheckPersistenceError
			if errors.As(err, &storage) {
				return outcome.Result, rejectLocalResultPersistence(err)
			}
			return outcome.Result, err
		}
	}
	return outcome.Result, outcome.err()
}

func (c *Coordinator) verifyNormalizedWithSkeptic(ctx context.Context, task TaskDef, taskID string, result *TaskResult, output string) error {
	checkpoint, _, output, err := c.loadVerificationCompletion(ctx, c.todoItemByID(taskID), result, output)
	if err != nil {
		return err
	}
	if err := checkpoint.publishPendingChecks(); err != nil {
		return err
	}
	if !checkpoint.Skeptic.Completed {
		manifest, err := verificationWorkspaceManifest(ctx, checkpoint.Objective.WorkspaceManifestSHA256)
		if err != nil {
			return err
		}
		if err := checkpoint.startCheck(&checkpoint.Skeptic, adversarialVerification); err != nil {
			return err
		}
		detail := ""
		if err := c.adversarialVerify(context.WithValue(ctx, todoIDKey{}, taskID), task, taskID, output); err != nil {
			detail = c.FailureDetail(err, FailureSourceError)
		} else {
			// Skeptic review has no workspace effects. Keep the snapshot it
			// reviewed, including any legitimate objective verifier writes.
			checkpoint.Skeptic.WorkspaceManifestSHA256 = manifest
		}
		if err := checkpoint.recordCheck(adversarialVerification, &checkpoint.Skeptic, detail); err != nil {
			return err
		}
	}
	return checkpoint.Skeptic.err()
}

func verificationWorkspaceManifest(ctx context.Context, expected string) (string, error) {
	isolated := isolatedAttemptFromContext(ctx)
	if isolated == nil && expected == "" {
		return "", nil
	}
	if isolated == nil {
		return "", rejectResultSourceRead(errors.New("verified isolated workspace is unavailable"))
	}
	delta, err := (preparedApplyIntent{}).loadDelta(ctx, isolated, expected)
	if err != nil {
		retainIsolatedNormalization(isolated, expected != "" && localResultRejectionReason(err) == RepairFailureSource)
		return "", err
	}
	return normalizedDeltaDigest(delta)
}

func (c *Coordinator) prepareNormalizedApply(ctx context.Context, isolated *isolatedAttempt, started *attemptApplyStartedPayload) (*AttemptWorkspaceDelta, error) {
	checkpoint, err := c.loadNormalizedCompletion(isolated.taskID, started.TypedResult, "")
	if err != nil {
		return nil, err
	}
	key := checkpoint.key + "/apply-intent"
	intent, err := readPreparedApplyIntent(checkpoint.root, key, isolated.worldDir(), checkpoint.applySealDigest())
	if err != nil {
		return nil, err
	}
	item := c.todoItemByID(isolated.taskID)
	if item.ExecutionReceipt.ExecutionWorldID != isolated.worldID() || !checkpoint.verified(item, started.VerifyResult) ||
		!sameCanonicalEvidence(item.ExecutionReceipt, started.ExecutionReceipt) {
		return nil, rejectResultSourceRead(errors.New("normalized apply lacks completed verification evidence"))
	}
	verifiedManifest, err := checkpoint.verifiedWorkspaceManifest(item, intent.published)
	if err != nil {
		return nil, err
	}
	if checkpoint.DeltaSHA256 != "" {
		started.DeltaDigest = checkpoint.DeltaSHA256
		if !checkpoint.matchesApply(*started) || verifiedManifest != "" && verifiedManifest != checkpoint.DeltaManifestSHA256 {
			return nil, rejectResultSourceRead(errors.New("normalized apply differs from the prepared completion"))
		}
		delta, err := checkpoint.readDelta(isolated.worldDir(), isolated.worldID(), isolated.prepared.isolated.baseline.digest())
		if err != nil {
			return nil, rejectNormalizationRecoveryRead(err)
		}
		if !intent.published {
			return delta, publishRuntimeEffectProjection(checkpoint.root, key, intent.record.SealSHA256)
		}
		return delta, nil
	}
	delta, err := intent.loadDelta(ctx, isolated, verifiedManifest)
	if err != nil {
		return nil, err
	}
	started.DeltaDigest = delta.Digest
	digest, err := normalizedApplyDigest(*started)
	if err != nil {
		return nil, rejectResultSourceRead(err)
	}
	manifestSHA, err := normalizedDeltaDigest(delta)
	if err != nil {
		return nil, rejectResultSourceRead(err)
	}
	checkpoint.DeltaSHA256, checkpoint.DeltaManifestSHA256, checkpoint.ApplySHA256 = delta.Digest, manifestSHA, digest
	seal := checkpoint.applySealDigest()
	if err := intent.seal(checkpoint.root, key, isolated.worldDir(), seal, delta); err != nil {
		return nil, err
	}
	if err := checkpoint.save(); err != nil {
		return nil, err
	}
	return delta, publishRuntimeEffectProjection(checkpoint.root, key, seal)
}

func (checkpoint *normalizedCompletionCheckpoint) verifiedWorkspaceManifest(item *TodoItem, publishedApply bool) (string, error) {
	// A previously published apply already binds its original delta. Older
	// verification seals can lack a manifest, so only that apply may replay.
	if publishedApply && checkpoint.Objective.WorkspaceManifestSHA256 == "" && checkpoint.Skeptic.WorkspaceManifestSHA256 == "" {
		return "", nil
	}
	var digest string
	if item.Verify != "" || item.VerifySpec != nil {
		digest = checkpoint.Objective.WorkspaceManifestSHA256
		if digest == "" || item.AdversarialVerify > 0 && digest != checkpoint.Skeptic.WorkspaceManifestSHA256 {
			return "", rejectResultSourceRead(errors.New("isolated apply lacks its original objective workspace evidence"))
		}
	} else if item.AdversarialVerify > 0 {
		digest = checkpoint.Skeptic.WorkspaceManifestSHA256
		if digest == "" {
			return "", rejectResultSourceRead(errors.New("isolated apply lacks its original skeptic workspace evidence"))
		}
	}
	return digest, nil
}

func (checkpoint *normalizedCompletionCheckpoint) readDelta(worldDir, worldID, baselineDigest string) (*AttemptWorkspaceDelta, error) {
	return readVerifiedAttemptDelta(worldDir, worldID, baselineDigest, checkpoint.DeltaSHA256, checkpoint.DeltaManifestSHA256)
}

func readVerifiedAttemptDelta(worldDir, worldID, baselineDigest, digest, manifest string) (*AttemptWorkspaceDelta, error) {
	record, err := readPreparedApplyRecord(worldDir)
	if err != nil {
		return nil, err
	}
	if record == nil || record.Digest != digest {
		return nil, rejectResultSourceRead(errors.New("prepared world delta does not match the original apply"))
	}
	delta := &record.AttemptWorkspaceDelta
	actual, err := normalizedDeltaDigest(delta)
	if err != nil || actual != manifest || delta.WorldID != worldID || delta.BaselineDigest != baselineDigest {
		return nil, errors.New("normalized delta manifest differs from the verified attempt world")
	}
	return delta, nil
}

func normalizedDeltaDigest(delta *AttemptWorkspaceDelta) (string, error) {
	body, err := json.Marshal(delta, jsonv1.DefaultOptionsV1(), json.Deterministic(true))
	if err != nil {
		return "", err
	}
	return sha256HexBytes(body), nil
}

func (checkpoint *normalizedCompletionCheckpoint) verified(item *TodoItem, verification *VerificationResult) bool {
	objective := (item.Verify == "" && item.VerifySpec == nil) || checkpoint.Objective.Completed && checkpoint.Objective.Error == ""
	skeptic := item.AdversarialVerify == 0 || checkpoint.Skeptic.Completed && checkpoint.Skeptic.Error == ""
	return objective && skeptic && len(checkpoint.pendingChecks) == 0 && sameCanonicalEvidence(checkpoint.Objective.Result, verification)
}

func (checkpoint *normalizedCompletionCheckpoint) matchesApply(started attemptApplyStartedPayload) bool {
	digest, err := normalizedApplyDigest(started)
	return err == nil && checkpoint.DeltaSHA256 != "" && checkpoint.DeltaSHA256 == started.DeltaDigest && checkpoint.ApplySHA256 == digest
}

func normalizedApplyDigest(started attemptApplyStartedPayload) (string, error) {
	body, err := marshalNormalizedCheckpoint(started)
	if err != nil {
		return "", err
	}
	return sha256HexBytes(body), nil
}
