package team

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/kjelly/hufu/internal/runtimestore"
)

type directNormalizationCheckpointKey struct{}

type directNormalizationCheckpoint struct {
	BranchID         string                      `json:"branch_id"`
	Identity         submitResultRuntimeIdentity `json:"identity"`
	EvidenceIdentity submitResultRuntimeIdentity `json:"evidence_identity,omitzero"`
	Receipt          ExecutionReceipt            `json:"receipt"`
}

func directNormalizationCheckpointDocument(branch, task string, revision int, dispatch string) string {
	return fmt.Sprintf("direct-normalization-checkpoint/%s/%s/%d/%s", branch, task, revision, dispatch)
}

func (p *directNormalizationCheckpoint) recordDispatchStart(root string) error {
	id := p.Identity
	key := directNormalizationCheckpointDocument(p.BranchID, id.TaskID, id.OccurrenceRevision, id.DispatchID)
	expected := sha256HexBytes([]byte(key))
	err := createRuntimeEffectRecord(root, key, expected)
	if errors.Is(err, os.ErrExist) {
		var binding string
		binding, err = readRuntimeEffectRecord(root, key)
		if err == nil && binding != expected {
			return rejectResultSourceRead(errors.New("normalization start does not match the original dispatch"))
		}
	}
	return rejectLocalResultPersistence(err)
}

func (p *directNormalizationCheckpoint) store(ctx context.Context, tx *sql.Tx) error {
	body, err := json.Marshal(p)
	if err != nil {
		return err
	}
	id := p.Identity
	return runtimestore.PutDocument(ctx, tx, directNormalizationCheckpointDocument(p.BranchID, id.TaskID, id.OccurrenceRevision, id.DispatchID), body)
}

// The reservation and its recovery pointer share one database transaction.
// An independent dispatch start detects loss of the pointer after the effect
// began, including when the runner never wrote its final execution receipt.
func (c *Coordinator) withDirectNormalizationCheckpoint(ctx context.Context, identity submitResultRuntimeIdentity, submissionSHA string, raw []byte) (context.Context, error) {
	transcript, _ := ctx.Value(taskTranscriptKey{}).(*taskTranscript)
	if submissionSHA == "" || !c.hasDurableEventJournal() {
		return ctx, nil
	}
	source, repair, err := c.originalNormalizationEvidence(ctx, identity.TaskID)
	if err != nil || transcript == nil && !repair {
		return ctx, err
	}
	item := c.todoItemByID(identity.TaskID)
	if item == nil || item.ResultContract == nil || item.OccurrenceRevision != identity.OccurrenceRevision || item.DispatchID != identity.DispatchID {
		return ctx, errTaskResultStale
	}
	var receipt ExecutionReceipt
	if repair {
		if err := c.validateProtocolRepairEvidenceReceipt(item, item.ResultContract); err != nil {
			return ctx, err
		}
		receipt = cloneExecutionReceipt(item.ExecutionReceipt)
	} else {
		receipt, err = c.checkpointNormalizationWorkReceipt(ctx, item, identity, transcript, "")
		if err != nil {
			return ctx, err
		}
	}
	receipt.InterruptedResultNormalization = ResultNormalizationEvidence{
		SubmissionSHA256: submissionSHA, OccurrenceSHA256: normalizationOccurrence(identity, nil),
		RawSHA256: sha256HexBytes(raw), ProviderSHA256: item.ResultContract.NormalizerSHA256,
	}
	p := &directNormalizationCheckpoint{BranchID: c.activeBranchID(), Identity: identity, EvidenceIdentity: source, Receipt: receipt}
	return context.WithValue(ctx, directNormalizationCheckpointKey{}, p), nil
}

func (c *Coordinator) checkpointNormalizationWorkReceipt(ctx context.Context, item *TodoItem, identity submitResultRuntimeIdentity, transcript *taskTranscript, acceptedCallID string) (ExecutionReceipt, error) {
	if transcript.todoID != identity.TaskID || transcript.runID != identity.RunID || transcript.attempt != identity.Attempt || transcript.agent != identity.Agent {
		return ExecutionReceipt{}, errTaskResultStale
	}
	metadata, ok := invocationMetadataFromContext(ctx)
	if !ok || metadata.ModelExecutionID == "" || metadata.RunID != identity.RunID || metadata.TaskID != identity.TaskID || metadata.AgentName != identity.Agent || metadata.Attempt != identity.Attempt {
		return ExecutionReceipt{}, errTaskResultStale
	}
	artifact, err := transcript.manifest(true, acceptedCallID)
	if err != nil {
		return ExecutionReceipt{}, err
	}
	scope, _ := ctx.Value(artifactAccessScopeKey).(*ArtifactAccessScope)
	worldID := ""
	if world := isolatedAttemptFromContext(ctx); world != nil {
		worldID = world.worldID()
	}
	started, _ := ctx.Value(workerAttemptStartedAtKey{}).(time.Time)
	if started.IsZero() {
		started = item.StartedAt
	}
	return ExecutionReceipt{
		RunID: identity.RunID, TaskID: item.ID, Attempt: identity.Attempt, OccurrenceAttempt: item.Retries + 1, ProducerID: identity.Agent,
		ExecutionTarget: receiptExecutionTarget(item.ExecutionTarget), ExecutionWorldID: worldID, StartedAt: started, FinishedAt: time.Now(),
		TranscriptRef: artifact.ID, ArtifactScope: cloneArtifactAccessScope(scope), ModelExecutionID: metadata.ModelExecutionID,
		RunInputSnapshotID: item.RunInputSnapshotID, RunInputSnapshotHash: item.RunInputSnapshotHash,
		ResultContractID: item.ResultContract.ID, ResultContractSHA256: item.ResultContract.SchemaSHA256,
		ResultWorkDispatchID: identity.DispatchID, ResultWorkOccurrenceRevision: identity.OccurrenceRevision,
	}, nil
}

func (c *Coordinator) loadInterruptedNormalizationReceipt(ctx context.Context, item *TodoItem) (*ExecutionReceipt, error) {
	if canResumeDirectNormalization(item) {
		if item.Status == TaskInProgress {
			return item.ExecutionReceipt, nil
		}
		return nil, nil
	}
	p, err := c.readDirectNormalizationCheckpoint(item)
	if err != nil {
		return nil, rejectNormalizationRecoveryRead(err)
	}
	if p == nil {
		return nil, nil
	}
	ref, _, err := c.boundResultContract(item.ID)
	if err != nil {
		return c.interruptedNormalizationReadFailure(p, err)
	}
	proof := &p.Receipt.InterruptedResultNormalization
	stage, found, err := c.readStagedNormalization(*ref, proof.OccurrenceSHA256, proof.RawSHA256)
	if err != nil {
		return c.interruptedNormalizationReadFailure(p, err)
	}
	if !found {
		return c.interruptedNormalizationReadFailure(p, errors.New("sealed normalization stage is missing"))
	}
	reason, message := RepairFailureCommit, "normalization sealed before the interrupted result commit"
	if stage == nil {
		reason, message = RepairFailureAction, "interrupted normalizer invocation has no durable output"
	} else {
		proof.OutputSHA256 = stage.OutputSHA256
	}
	if current := item.ExecutionReceipt; current != nil && current.RepairProvenance != nil &&
		current.InterruptedResultNormalization == *proof && current.RepairProvenance.FailureReason != RepairFailureCommit {
		if stage == nil && current.RepairProvenance.FailureReason == RepairFailureAction {
			return current, rejectLocalResult(reason, errors.New(message))
		}
		return nil, nil
	}
	p.recordRecoveryOutcome(reason, message)
	if reason == RepairFailureAction {
		return &p.Receipt, rejectLocalResult(reason, errors.New(message))
	}
	original := *item
	original.ExecutionReceipt = &p.Receipt
	if _, err := c.loadDirectNormalizationRecovery(ctx, &original); err != nil {
		return c.interruptedNormalizationReadFailure(p, err)
	}
	return &p.Receipt, nil
}

func rejectNormalizationRecoveryRead(err error) error {
	if _, ok := errors.AsType[*localResultAdmissionError](err); ok {
		return err
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return rejectLocalResult(RepairFailureCommit, err)
	}
	return rejectResultSourceRead(err)
}

func (c *Coordinator) interruptedNormalizationReadFailure(p *directNormalizationCheckpoint, failure error) (*ExecutionReceipt, error) {
	err := rejectNormalizationRecoveryRead(failure)
	if localResultRejectionReason(err) == RepairFailureCommit {
		return nil, err
	}
	p.recordRecoveryOutcome(RepairFailureSource, c.FailureDetail(err, FailureSourceError))
	return &p.Receipt, err
}

func (p *directNormalizationCheckpoint) recordRecoveryOutcome(reason RepairFailureReason, detail string) {
	if p.EvidenceIdentity == (submitResultRuntimeIdentity{}) {
		p.Receipt.WorkSubmissionFailure = reason
	}
	if p.Receipt.RepairProvenance == nil {
		p.Receipt.RepairProvenance = &RepairProvenance{}
	}
	p.Receipt.RepairProvenance.Success = false
	p.Receipt.RepairProvenance.FailureReason = reason
	p.Receipt.RepairProvenance.Error = detail
}

func (c *Coordinator) readDirectNormalizationCheckpoint(item *TodoItem) (*directNormalizationCheckpoint, error) {
	if item == nil || (item.Status != TaskInProgress && item.Status != TaskProtocolIncomplete) ||
		item.ResultContract == nil || item.ResultContract.Normalizer == "" || item.DispatchID == "" {
		return nil, nil
	}
	return c.readDirectNormalizationDispatch(item)
}

func (c *Coordinator) readDirectNormalizationDispatch(item *TodoItem) (*directNormalizationCheckpoint, error) {
	branch := c.activeBranchID()
	key := directNormalizationCheckpointDocument(branch, item.ID, item.OccurrenceRevision, item.DispatchID)
	raw, found, err := workspaceDocs(c.session.Workspace).getDocument(key)
	if err != nil {
		return nil, err
	}
	binding, err := readRuntimeEffectRecord(c.artifactStoreRootPath(), key)
	if err == nil {
		if binding != sha256HexBytes([]byte(key)) {
			return nil, errors.New("normalization start does not match the original dispatch")
		}
		if !found {
			return nil, errors.New("normalization checkpoint is missing for the started dispatch")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if !found {
		return nil, nil
	}
	var p directNormalizationCheckpoint
	if len(raw) > resultPayloadMaxBytes || json.Unmarshal(raw, &p, json.RejectUnknownMembers(true)) != nil || !p.matches(item, branch) {
		return nil, errors.New("normalization checkpoint does not match the interrupted dispatch")
	}
	return &p, nil
}

func (c *Coordinator) acceptedNormalizationOccurrence(item *TodoItem, stage *resultNormalization) (string, error) {
	source, ok := normalizationWorkEvidenceIdentity(item)
	if !ok {
		return "", errors.New("normalized result has no original work occurrence")
	}
	original := normalizationOccurrence(source, nil)
	if item.ExecutionReceipt.RepairProvenance == nil {
		return original, nil
	}
	return c.acceptedNormalizedCorrectionOccurrence(stage, source, original)
}

type normalizedCorrectionAcceptanceKey struct{}

func (c *Coordinator) normalizedCorrectionKey(source submitResultRuntimeIdentity) string {
	return "normalized-correction/" + directNormalizationCheckpointDocument(c.activeBranchID(), source.TaskID, source.OccurrenceRevision, source.DispatchID)
}

func (c *Coordinator) checkpointNormalizedCorrection(ctx context.Context, result *TaskResult) error {
	correction, ok := ctx.Value(normalizedCorrectionAcceptanceKey{}).(*directNormalizationRecovery)
	if !ok || !isCompletedLocalResult(result) || !c.hasDurableEventJournal() {
		return nil
	}
	proof := correction.stage.evidence()
	proof.SubmissionSHA256 = correction.digest
	body, err := json.Marshal(proof)
	if err != nil {
		return rejectLocalResultPersistence(err)
	}
	return c.sealNormalizedHandoff(ctx, c.normalizedCorrectionKey(correction.submission.EvidenceIdentity), body)
}

func (c *Coordinator) acceptedNormalizedCorrectionOccurrence(stage *resultNormalization, source submitResultRuntimeIdentity, original string) (string, error) {
	docs := workspaceDocs(c.session.Workspace)
	body, found, err := docs.getDocument(c.normalizedCorrectionKey(source))
	if err != nil {
		return "", err
	}
	if !found && stage.OccurrenceSHA256 == original {
		return original, nil
	}
	var proof ResultNormalizationEvidence
	expected := stage.evidence()
	if !found || json.Unmarshal(body, &proof, json.RejectUnknownMembers(true)) != nil {
		return "", errors.New("normalized correction has no accepted submission checkpoint")
	}
	expected.SubmissionSHA256 = proof.SubmissionSHA256
	if proof != expected || !validCollectionDigest(proof.SubmissionSHA256) {
		return "", errors.New("normalized correction belongs to a different submission")
	}
	body, found, err = docs.getDocument(directNormalizationSubmissionPrefix + proof.SubmissionSHA256)
	if err != nil {
		return "", err
	}
	var submission directNormalizationSubmission
	if !found || sha256HexBytes(body) != proof.SubmissionSHA256 || json.Unmarshal(body, &submission, json.RejectUnknownMembers(true)) != nil ||
		submission.BranchID != c.activeBranchID() || !sameTaskResultOccurrence(submission.EvidenceIdentity, source) ||
		!validSubmitResultIdentity(submission.Identity) || submission.Identity.TaskID != source.TaskID || submission.Identity.Attempt != source.Attempt ||
		normalizationOccurrence(submission.Identity, nil) != proof.OccurrenceSHA256 {
		return "", errors.New("normalized correction submission does not match the original work")
	}
	raw, err := submittedNormalizerDraft(submission.Wire, true)
	if err != nil || !bytes.Equal(raw, stage.Raw) {
		return "", errors.New("normalized correction submission bytes do not match the accepted stage")
	}
	return proof.OccurrenceSHA256, nil
}

func (p *directNormalizationCheckpoint) matches(item *TodoItem, branch string) bool {
	identity, receipt := p.Identity, p.Receipt
	proof := receipt.InterruptedResultNormalization
	source := identity
	if p.EvidenceIdentity != (submitResultRuntimeIdentity{}) {
		original, ok := normalizationWorkEvidenceIdentity(item)
		if !ok || !sameTaskResultOccurrence(original, p.EvidenceIdentity) || item.ExecutionReceipt.TranscriptRef != receipt.TranscriptRef ||
			receipt.ResultWorkDispatchID != original.DispatchID || receipt.ResultWorkOccurrenceRevision != original.OccurrenceRevision {
			return false
		}
		source = original
	}
	return p.BranchID == branch && validSubmitResultIdentity(identity) &&
		identity.TaskID == item.ID && identity.Agent == item.Agent && identity.OccurrenceRevision == item.OccurrenceRevision && identity.DispatchID == item.DispatchID &&
		receipt.TaskID == identity.TaskID && receipt.ProducerID == identity.Agent && receipt.Attempt == identity.Attempt && receipt.RunID == source.RunID &&
		receipt.RunInputSnapshotID == item.RunInputSnapshotID && receipt.RunInputSnapshotHash == item.RunInputSnapshotHash &&
		validCollectionDigest(proof.SubmissionSHA256) && validCollectionDigest(proof.RawSHA256) &&
		proof.ProviderSHA256 == item.ResultContract.NormalizerSHA256 && proof.OccurrenceSHA256 == normalizationOccurrence(identity, nil)
}

func (c *Coordinator) restoreInterruptedDirectNormalization(ctx context.Context, item *TodoItem) (*TodoItem, error) {
	receipt, err := c.loadInterruptedNormalizationReceipt(ctx, item)
	if receipt == nil {
		return item, err
	}
	if persistErr := c.setAttemptReceipt(item.ID, receipt); persistErr != nil {
		return item, persistErr
	}
	if err != nil {
		return c.todoItemByID(item.ID), err
	}
	if item.Status == TaskProtocolIncomplete {
		return c.todoItemByID(item.ID), nil
	}
	if err := c.commitTaskTransitionFromCurrent(ctx, item.ID, TaskProtocolIncomplete, "resuming sealed normalization", item.Output, map[string]any{"protocol_checkpoint": true}); err != nil {
		return item, err
	}
	return c.todoItemByID(item.ID), nil
}
