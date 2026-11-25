package team

import (
	"bytes"
	"context"
	jsonv1 "encoding/json"
	"encoding/json/v2"
	"errors"
	"fmt"
	"slices"
	"strings"

	"charm.land/fantasy"
)

const directNormalizationSubmissionPrefix = "result-normalization-submission/"

type directNormalizationRecoveryKey struct{}

type directNormalizationSubmission struct {
	BranchID         string                      `json:"branch_id"`
	Identity         submitResultRuntimeIdentity `json:"identity"`
	EvidenceIdentity submitResultRuntimeIdentity `json:"evidence_identity,omitzero"`
	Wire             []byte                      `json:"wire"`
	Artifacts        []ArtifactRef               `json:"artifacts,omitempty"`
}

type directNormalizationRecovery struct {
	submission directNormalizationSubmission
	stage      *resultNormalization
	digest     string
}

func normalizationContract(item *TodoItem) bool {
	return item != nil && item.ResultContract != nil && item.ResultContract.Normalizer != ""
}

func (c *Coordinator) settleNormalizedSubmissionFailure(ctx context.Context, item *TodoItem, task TaskDef, output string, reason RepairFailureReason, failure error) error {
	if reason == RepairFailureCommit && canResumeDirectNormalization(item) {
		return errors.Join(failure, c.checkpointNormalizedContinuation(ctx, item.ID, output))
	}
	blocked := TaskBlocked
	detail := c.FailureDetail(failure, FailureSourceError)
	return errors.Join(failure, c.persistFailureWithOutput(item.Agent, task.Goal, item.ID, detail, NeedsHuman, FailureProtocol, &blocked, output))
}

func hasSealedDirectNormalization(ctx context.Context) bool {
	_, ok := ctx.Value(directNormalizationRecoveryKey{}).(*directNormalizationRecovery)
	return ok
}

func (s directNormalizationSubmission) evidenceIdentity() submitResultRuntimeIdentity {
	if validSubmitResultIdentity(s.EvidenceIdentity) {
		return s.EvidenceIdentity
	}
	return s.Identity
}

func normalizationWorkEvidenceIdentity(item *TodoItem) (submitResultRuntimeIdentity, bool) {
	if item == nil || item.ResultContract == nil || item.ResultContract.Normalizer == "" || item.ExecutionReceipt == nil {
		return submitResultRuntimeIdentity{}, false
	}
	receipt := item.ExecutionReceipt
	source := submitResultRuntimeIdentity{TaskID: item.ID, RunID: receipt.RunID, Attempt: receipt.Attempt,
		Agent: receipt.ProducerID, OccurrenceRevision: receipt.ResultWorkOccurrenceRevision, DispatchID: receipt.ResultWorkDispatchID}
	return source, validSubmitResultIdentity(source) && source.OccurrenceRevision <= item.OccurrenceRevision &&
		receipt.TaskID == item.ID && strings.EqualFold(source.Agent, item.Agent)
}

func (c *Coordinator) originalNormalizationEvidence(ctx context.Context, taskID string) (submitResultRuntimeIdentity, bool, error) {
	if recovery, ok := ctx.Value(directNormalizationRecoveryKey{}).(*directNormalizationRecovery); ok {
		if recovery.submission.BranchID != c.activeBranchID() {
			return submitResultRuntimeIdentity{}, false, errTaskResultStale
		}
		return recovery.submission.evidenceIdentity(), true, nil
	}
	if protocolRepairExecution(ctx) {
		source, ok := normalizationWorkEvidenceIdentity(c.todoItemByID(taskID))
		return source, ok, nil
	}
	return submitResultRuntimeIdentity{}, false, nil
}

// A new dispatch lease authorizes the result write. Only the verified
// original dispatch can supply observations for that exact submission.
func (c *Coordinator) resultSubmissionEvidenceIdentity(ctx context.Context, taskID string) (submitResultRuntimeIdentity, error) {
	identity, err := submitResultRuntimeIdentityFromContext(ctx, c, taskID)
	if err != nil {
		return identity, err
	}
	original, historical, err := c.originalNormalizationEvidence(ctx, taskID)
	if err != nil || !historical {
		return identity, err
	}
	active, opened := c.activeTaskResultOccurrence(taskID)
	if !opened || !sameTaskResultOccurrence(active, identity) ||
		original.TaskID != identity.TaskID || original.Attempt != identity.Attempt || !strings.EqualFold(original.Agent, identity.Agent) {
		return identity, errTaskResultStale
	}
	return original, nil
}

func (c *Coordinator) normalizeDirectSubmission(ctx context.Context, identity submitResultRuntimeIdentity, input SubmitResultInput) (*directNormalizationRecovery, error) {
	if recovery, ok := ctx.Value(directNormalizationRecoveryKey{}).(*directNormalizationRecovery); ok {
		original := recovery.submission.Identity
		if identity.TaskID != original.TaskID || identity.Agent != original.Agent || identity.Attempt != original.Attempt ||
			input.normalizerSubmission != string(recovery.submission.Wire) || !bytes.Equal(input.normalizerDraft, recovery.stage.Raw) {
			return nil, rejectLocalResult(RepairFailureSource, errors.New("normalization recovery submission has changed"))
		}
		ref, _, err := c.boundResultContract(identity.TaskID)
		if err != nil || ref == nil {
			return nil, rejectLocalResult(RepairFailureContract, errors.Join(err, errors.New("normalization recovery contract is unavailable")))
		}
		return recovery, c.publishStagedNormalization(ctx, *ref, recovery.stage)
	}
	wire := input.normalizerSubmission
	if len(wire) > resultPayloadMaxBytes || !normalizerBytesPersistable([]byte(wire)) {
		return nil, rejectLocalResult(RepairFailureInvalidSchema, errors.New("normalization submission must be bounded and omit credentials, tokens, and keys"))
	}
	submission := directNormalizationSubmission{BranchID: c.activeBranchID(), Identity: identity, Wire: []byte(wire)}
	var err error
	if protocolRepairExecution(ctx) {
		source, evidenceErr := c.resultSubmissionEvidenceIdentity(ctx, identity.TaskID)
		if evidenceErr != nil {
			return nil, rejectResultSourceRead(evidenceErr)
		}
		if !sameTaskResultOccurrence(source, identity) {
			submission.EvidenceIdentity = source
		}
	}
	submission.Artifacts, err = c.sealNormalizationArtifacts(ctx, identity, input.taskResult().Artifacts)
	if err != nil {
		return nil, rejectResultSourceRead(err)
	}
	digest, err := c.sealDirectNormalizationSubmission(ctx, submission)
	if err != nil {
		return nil, err
	}
	ctx, err = c.withDirectNormalizationCheckpoint(ctx, identity, digest, input.normalizerDraft)
	if err != nil {
		return nil, rejectLocalResult(RepairFailureCommit, err)
	}
	stage, err := c.stageResultNormalization(ctx, identity.TaskID, input.normalizerDraft, nil)
	return &directNormalizationRecovery{submission: submission, stage: stage, digest: digest}, err
}

func (c *Coordinator) sealDirectNormalizationSubmission(ctx context.Context, submission directNormalizationSubmission) (string, error) {
	if len(submission.Wire) == 0 {
		return "", nil
	}
	body, err := json.Marshal(submission)
	if err != nil {
		return "", rejectLocalResult(RepairFailureCommit, err)
	}
	if len(body) > 2*resultPayloadMaxBytes {
		return "", rejectLocalResult(RepairFailureInvalidSchema, errors.New("normalization submission evidence exceeds the size limit"))
	}
	digest := sha256HexBytes(body)
	key := directNormalizationSubmissionPrefix + digest
	created, err := c.writeNormalizationStage(ctx, key, nil, body)
	if err != nil {
		return "", rejectLocalResult(RepairFailureCommit, err)
	}
	if !created {
		stored, found, err := workspaceDocs(c.session.Workspace).getDocument(key)
		if err != nil || !found || !bytes.Equal(stored, body) {
			return "", rejectLocalResult(RepairFailureCommit, errors.Join(errors.New("normalization submission could not be sealed"), err))
		}
	}
	return digest, nil
}

func (c *Coordinator) sealNormalizationArtifacts(ctx context.Context, identity submitResultRuntimeIdentity, claims []ArtifactRef) ([]ArtifactRef, error) {
	if len(claims) == 0 {
		return nil, nil
	}
	workspace := c.session.Workspace
	store, err := NewFileArtifactStore(workspace, workspace)
	if err != nil {
		return nil, err
	}
	refs := make([]ArtifactRef, len(claims))
	for i, claim := range claims {
		refs[i], err = putSubmittedArtifact(ctx, store, identity, claim)
		if err != nil {
			return nil, fmt.Errorf("seal normalization artifact %q: %w", claim.Path, err)
		}
	}
	return refs, nil
}

func (c *Coordinator) validateDirectResultArtifactClaims(ctx context.Context, workspace string, claims []ArtifactRef) error {
	recovery, ok := ctx.Value(directNormalizationRecoveryKey{}).(*directNormalizationRecovery)
	if !ok {
		return validateSubmittedArtifactClaims(ctx, workspace, claims)
	}
	refs := recovery.submission.Artifacts
	if len(refs) != len(claims) {
		return errors.New("original artifact claims are missing from the sealed normalization submission")
	}
	for i, claim := range claims {
		if ref := refs[i]; ref.Path != claim.Path || ref.Description != claim.Description || ref.Kind != claim.Kind {
			return fmt.Errorf("original artifact claim %q does not match the sealed artifact", claim.Path)
		}
	}
	return c.verifyNormalizationArtifacts(ctx, recovery.submission.Identity, recovery.submission.Artifacts)
}

func (c *Coordinator) verifyNormalizationArtifacts(ctx context.Context, identity submitResultRuntimeIdentity, refs []ArtifactRef) error {
	return c.verifyNormalizedArtifactRefs(ctx, identity, refs, nil)
}

func (c *Coordinator) verifyNormalizedResultArtifacts(ctx context.Context, identity submitResultRuntimeIdentity, result *TaskResult, receipt *ExecutionReceipt) error {
	if result == nil || receipt == nil {
		return errors.New("normalized result has no accepted artifact envelope")
	}
	if projection := result.RuntimeMedia; projection != nil || receipt.RuntimeMediaHash != "" {
		if projection == nil || projection.RunID != identity.RunID || projection.TaskID != identity.TaskID ||
			projection.Attempt != identity.Attempt || !strings.EqualFold(projection.Agent, identity.Agent) ||
			projection.DispatchID != identity.DispatchID || projection.OccurrenceRevision != identity.OccurrenceRevision {
			return errors.New("normalized result media does not belong to the original work")
		}
		view, err := c.mediaLineage(ctx)
		if err != nil {
			return err
		}
		if err := verifyRuntimeMediaResult(result, receipt, view.events); err != nil {
			return err
		}
	}
	return c.verifyNormalizedArtifactRefs(ctx, identity, result.Artifacts, result.RuntimeMedia.artifacts())
}

func (c *Coordinator) normalizedWorkMedia(ctx context.Context, source submitResultRuntimeIdentity, fanoutModelID string, result *TaskResult, receipt *ExecutionReceipt) (*RuntimeMediaProjection, error) {
	projection, err := c.runtimeMediaForModelOccurrence(ctx, source, fanoutModelID, "")
	if err != nil {
		return nil, rejectNormalizationRecoveryRead(err)
	}
	hash := ""
	if projection != nil {
		hash = projection.Hash
	}
	if receipt.RuntimeMediaHash != "" && receipt.RuntimeMediaHash != hash ||
		result.RuntimeMedia != nil && !sameCanonicalEvidence(result.RuntimeMedia, projection) {
		return nil, rejectLocalResult(RepairFailureSource, errors.New("normalized media disagrees with the original work publication"))
	}
	candidate := result
	verifiedReceipt := *receipt
	if projection != nil {
		candidate = cloneTaskResult(result)
		attachTaskRuntimeMedia(candidate, projection)
		verifiedReceipt.RuntimeMediaHash = hash
	}
	if err := c.verifyNormalizedResultArtifacts(ctx, source, candidate, &verifiedReceipt); err != nil {
		return nil, rejectNormalizationRecoveryRead(err)
	}
	return projection, nil
}

func (c *Coordinator) verifyNormalizedArtifactRefs(ctx context.Context, identity submitResultRuntimeIdentity, refs, media []ArtifactRef) error {
	if len(refs) == 0 && len(media) == 0 {
		return nil
	}
	workspace := c.artifactStoreRootPath()
	store, err := OpenFileArtifactStoreReadOnly(workspace, workspace)
	if err != nil {
		return err
	}
	for _, ref := range refs {
		if isMediaArtifactID(ref.ID) {
			index := slices.IndexFunc(media, func(expected ArtifactRef) bool { return sameArtifactRef(ref, expected) })
			if index < 0 {
				return errors.New("sealed normalization artifact has no matching media publication")
			}
			media = slices.Delete(media, index, index+1)
		} else if ref.RunID != identity.RunID || ref.TaskID != identity.TaskID || ref.Attempt != identity.Attempt || ref.Agent != identity.Agent {
			return errors.New("sealed normalization artifact has a different producer occurrence")
		}
		if err := store.Verify(ctx, ref); err != nil {
			return err
		}
	}
	if len(media) != 0 {
		return errors.New("sealed normalization result omits published media artifacts")
	}
	return nil
}

func canResumeDirectNormalization(item *TodoItem) bool {
	return normalizedDirectContinuationReceipt(item) && item.ExecutionReceipt.RepairProvenance.FailureReason == RepairFailureCommit
}

func isNormalizedVerificationPending(item *TodoItem) bool {
	if item == nil || item.ResultContract == nil || item.ResultContract.Normalizer == "" {
		return false
	}
	receipt := item.ExecutionReceipt
	if item.Status == TaskProtocolIncomplete {
		return receipt != nil && receipt.ResultWorkDispatchID != "" && receipt.ResultCollection == nil &&
			(isCompletedLocalResult(receipt.SubmittedResult) || receipt.RepairProvenance != nil && receipt.RepairProvenance.Success)
	}
	if item.Status != TaskVerifying {
		return false
	}
	return receipt == nil || receipt.ResultCollection == nil
}

func normalizedWorkerVerificationHandoff(ref *ResultContractRef, receipt *ExecutionReceipt, result *TaskResult) bool {
	return isCompletedLocalResult(result) && normalizedWorkerAcceptedHandoff(ref, receipt, result)
}

func normalizedWorkerAcceptedHandoff(ref *ResultContractRef, receipt *ExecutionReceipt, result *TaskResult) bool {
	if ref == nil || ref.Normalizer == "" || receipt == nil || receipt.ResultWorkDispatchID == "" || receipt.ResultCollection != nil ||
		receipt.RepairProvenance != nil && !receipt.RepairProvenance.Success ||
		!isAcceptedLocalResult(result) || receipt.ResultContractID != ref.ID || receipt.ResultContractSHA256 != ref.SchemaSHA256 {
		return false
	}
	if payload := result.StructuredPayload; payload != nil {
		return payload.Contract == *ref && receiptMatchesResultPayload(receipt, payload) && matchingNormalizedResult(receipt, result)
	}
	return !ref.RequireStructured && receipt.ResultPayloadSHA256 == "" && receipt.ResultRawPayloadSHA256 == "" && receipt.ResultNormalizerOutputSHA256 == "" &&
		receipt.ResultNormalizerOccurrenceSHA256 == "" && matchingNormalizedResult(receipt, result)
}

func isAcceptedLocalResult(result *TaskResult) bool {
	return result != nil && isLocalSubmittedResultSource(result.Source) && validateSubmittedTaskResult(result) == nil
}

func isCompletedLocalResult(result *TaskResult) bool {
	return result != nil && isLocalSubmittedResultSource(result.Source) && validateCompletedTaskResult(result) == nil
}

func matchingNormalizedResult(receipt *ExecutionReceipt, result *TaskResult) bool {
	submitted := receipt.SubmittedResult
	if submitted == nil && receipt.RepairProvenance != nil && receipt.RepairProvenance.Success {
		submitted = receipt.RepairProvenance.SubmittedResult
	}
	if submitted == nil || result.TaskID != receipt.TaskID || result.Attempt != receipt.Attempt || !strings.EqualFold(result.Agent, receipt.ProducerID) {
		return false
	}
	durable, durableErr := json.Marshal(submitted, jsonv1.DefaultOptionsV1(), json.Deterministic(true))
	projected, projectedErr := json.Marshal(result, jsonv1.DefaultOptionsV1(), json.Deterministic(true))
	return durableErr == nil && projectedErr == nil && bytes.Equal(durable, projected)
}

func (c *Coordinator) checkpointNormalizedContinuation(ctx context.Context, taskID, output string) error {
	current := c.todoItemByID(taskID)
	if current != nil && current.Status == TaskVerifying {
		return nil
	}
	return c.commitTaskTransitionFromCurrent(ctx, taskID, TaskProtocolIncomplete, "sealed normalization awaiting local completion", output, map[string]any{"protocol_checkpoint": true})
}

func normalizedDirectContinuationReceipt(item *TodoItem) bool {
	if item == nil || item.ExecutionReceipt == nil || item.ResultContract == nil || item.ResultContract.Normalizer == "" {
		return false
	}
	receipt := item.ExecutionReceipt
	return receipt.ResultCollection == nil && receipt.RepairProvenance != nil &&
		validDirectNormalizationProof(item.ResultContract, receipt.InterruptedResultNormalization)
}

func validDirectNormalizationProof(ref *ResultContractRef, proof ResultNormalizationEvidence) bool {
	return ref.Normalizer != "" &&
		validCollectionDigest(proof.SubmissionSHA256) && validCollectionDigest(proof.OccurrenceSHA256) &&
		validCollectionDigest(proof.RawSHA256) && validCollectionDigest(proof.OutputSHA256) &&
		proof.ProviderSHA256 == ref.NormalizerSHA256
}

// The most recent returned commit failure owns the resumable submission.
// Both inline and resumed model corrections settle through this receipt.
func (r *ExecutionReceipt) bindInterruptedRepairNormalization(ref *ResultContractRef) {
	p := r.RepairProvenance
	if r.ResultCollection != nil || p == nil || p.Success || p.FailureReason != RepairFailureCommit || len(p.History) == 0 {
		return
	}
	last := p.History[len(p.History)-1]
	if last.FailureReason == RepairFailureCommit && validDirectNormalizationProof(ref, last.InterruptedResultNormalization) {
		r.InterruptedResultNormalization = last.InterruptedResultNormalization
	}
}

func (c *Coordinator) loadDirectNormalizationRecovery(ctx context.Context, item *TodoItem) (*directNormalizationRecovery, error) {
	if !normalizedDirectContinuationReceipt(item) {
		return nil, errResultCollectionStale
	}
	ref, _, err := c.boundResultContract(item.ID)
	if err != nil {
		return nil, err
	}
	if err := c.validateProtocolRepairEvidenceReceipt(item, ref); err != nil {
		return nil, err
	}
	if err := c.verifyResumeTranscript(ctx, item.ExecutionReceipt, taskTranscriptMediaType); err != nil {
		return nil, err
	}
	proof := item.ExecutionReceipt.InterruptedResultNormalization
	body, found, err := workspaceDocs(c.session.Workspace).getDocument(directNormalizationSubmissionPrefix + proof.SubmissionSHA256)
	if err != nil {
		return nil, err
	}
	var submission directNormalizationSubmission
	if !found || len(body) > 2*resultPayloadMaxBytes || sha256HexBytes(body) != proof.SubmissionSHA256 || json.Unmarshal(body, &submission, json.RejectUnknownMembers(true)) != nil || !c.matchesDirectNormalizationSubmission(item, submission, proof) {
		return nil, errors.New("original normalization submission evidence is invalid")
	}
	stage, _, err := c.readStagedNormalization(*ref, proof.OccurrenceSHA256, proof.RawSHA256)
	if err != nil {
		return nil, err
	}
	draft, err := submittedNormalizerDraft(submission.Wire, true)
	if err != nil || stage == nil || !bytes.Equal(draft, stage.Raw) || stage.OutputSHA256 != proof.OutputSHA256 {
		return nil, errors.New("sealed normalizer output does not match the original submission receipt")
	}
	if err := c.verifyNormalizationArtifacts(ctx, submission.Identity, submission.Artifacts); err != nil {
		return nil, err
	}
	return &directNormalizationRecovery{submission: submission, stage: stage, digest: proof.SubmissionSHA256}, nil
}

func (c *Coordinator) matchesDirectNormalizationSubmission(item *TodoItem, submission directNormalizationSubmission, proof ResultNormalizationEvidence) bool {
	identity := submission.Identity
	receipt := item.ExecutionReceipt
	runMatches := identity.RunID == receipt.RunID || slices.ContainsFunc(receipt.RepairProvenance.History, func(attempt RepairAttemptProvenance) bool {
		return attempt.InvocationRunID == identity.RunID
	})
	if submission.EvidenceIdentity != (submitResultRuntimeIdentity{}) {
		original, ok := normalizationWorkEvidenceIdentity(item)
		runMatches = ok && sameTaskResultOccurrence(original, submission.EvidenceIdentity) &&
			submission.Artifacts == nil && original.Attempt == identity.Attempt && original.OccurrenceRevision <= identity.OccurrenceRevision
	}
	return submission.BranchID == c.activeBranchID() && validSubmitResultIdentity(identity) && runMatches &&
		identity.TaskID == item.ID && identity.Attempt == receipt.Attempt && strings.EqualFold(identity.Agent, item.Agent) &&
		identity.OccurrenceRevision <= item.OccurrenceRevision && normalizationOccurrence(identity, nil) == proof.OccurrenceSHA256 && len(submission.Wire) <= resultPayloadMaxBytes
}

func (c *Coordinator) resumeDirectNormalization(ctx context.Context, task TaskDef, item *TodoItem) (string, error) {
	recovery, err := c.loadDirectNormalizationRecovery(ctx, item)
	if err != nil {
		return "", c.settleNormalizedRecoveryRead(item, task, rejectNormalizationRecoveryRead(err))
	}
	localCtx := context.WithValue(ctx, directNormalizationRecoveryKey{}, recovery)
	response, err := c.runLocalResultContinuation(localCtx, item, item.ExecutionReceipt.Attempt, "normalized-"+recovery.digest, string(recovery.submission.Wire))
	if err != nil {
		return "", c.settleNormalizedRecoveryRead(item, task, rejectNormalizationRecoveryRead(err))
	}
	if response.IsError {
		repair, err := c.checkpointDirectNormalizationRejection(item, task, response)
		if repair {
			return c.resumeProtocolIncompleteResult(ctx, task, c.todoItemByID(item.ID))
		}
		return "", err
	}
	result := c.GetTaskResult(item.ID)
	if err := validateSubmittedTaskResult(result); err != nil {
		return "", err
	}
	if err := c.materializeDirectNormalizationReceipt(item, result); err != nil {
		return "", err
	}
	if err := validateCompletedTaskResult(result); err != nil {
		current := c.todoItemByID(item.ID)
		return "", c.settleIncompleteNormalizedHandoff(item.ID, current.ExecutionReceipt, result, err)
	}
	return c.finishProtocolRepair(ctx, item, task, item.Agent, task.Model, result, item.Output)
}

func (c *Coordinator) settleNormalizedRecoveryRead(item *TodoItem, task TaskDef, err error) error {
	reason := localResultRejectionReason(err)
	if c.isCommittedFanoutCollection(item) {
		err = &resultCollectionFailure{Reason: reason, Checkpoint: currentResultCollection(item), Cause: err}
	}
	if reason == RepairFailureCommit {
		return err
	}
	if reason != RepairFailureAction {
		reason = RepairFailureSource
	}
	detail := c.FailureDetail(err, FailureSourceError)
	if item.ExecutionReceipt != nil {
		receipt := cloneExecutionReceipt(item.ExecutionReceipt)
		if receipt.RepairProvenance == nil {
			receipt.RepairProvenance = &RepairProvenance{}
		}
		receipt.RepairProvenance.FailureReason, receipt.RepairProvenance.Error = reason, detail
		receipt.RepairProvenance.Success = false
		if receipt.WorkSubmissionFailure == RepairFailureCommit {
			receipt.WorkSubmissionFailure = reason
		}
		if receiptErr := c.setAttemptReceipt(item.ID, &receipt); receiptErr != nil {
			return errors.Join(err, receiptErr)
		}
	}
	blocked := TaskBlocked
	return errors.Join(err, c.persistFailureWithOutput(item.Agent, task.Goal, item.ID, detail, NeedsHuman, FailureProtocol, &blocked, item.Output))
}

func (c *Coordinator) checkpointDirectNormalizationRejection(item *TodoItem, task TaskDef, response fantasy.ToolResponse) (repair bool, checkpointErr error) {
	err := fmt.Errorf("sealed normalized submission rejected: %s", response.Content)
	receipt := cloneExecutionReceipt(item.ExecutionReceipt)
	reason := submissionFailureMetadata(response.Metadata)
	if reason == "" {
		reason = RepairFailureContract
	}
	if reason == RepairFailureCommit {
		return false, rejectLocalResult(reason, err)
	}
	p := receipt.RepairProvenance
	p.FailureReason, p.Error = reason, c.FailureDetail(err, FailureSourceError)
	if receipt.WorkSubmissionFailure == RepairFailureCommit {
		receipt.WorkSubmissionFailure = reason
	}
	if reason == RepairFailureInvalidSchema {
		receipt.ResultValidation = ResultValidationInvalid
		receipt.ResultValidationFailures += max(1, c.takeResultValidationFailures(item.ID))
		if n := len(p.History); n > 0 && p.History[n-1].Attempt == p.RepairAttempts {
			last := &p.History[n-1]
			last.FailureReason, last.Error = reason, p.Error
		}
	}
	if persistErr := c.setAttemptReceipt(item.ID, &receipt); persistErr != nil {
		return false, errors.Join(err, persistErr)
	}
	if persistErr := c.emitTaskEventsFromCheckpoint(c.taskTracker.TodoList().Items()); persistErr != nil {
		return false, errors.Join(err, persistErr)
	}
	if reason == RepairFailureInvalidSchema {
		return true, nil
	}
	blocked := TaskBlocked
	return false, errors.Join(rejectLocalResult(reason, err), c.persistFailureWithOutput(item.Agent, task.Goal, item.ID, p.Error, NeedsHuman, FailureProtocol, &blocked, item.Output))
}

func (c *Coordinator) materializeNormalizedSchemaCorrection(item *TodoItem, agentName string, result *TaskResult) error {
	receipt := item.ExecutionReceipt
	history := slices.Clone(receipt.RepairProvenance.History)
	if len(history) == 0 || history[len(history)-1].Attempt != receipt.RepairProvenance.RepairAttempts ||
		history[len(history)-1].FailureReason != RepairFailureInvalidSchema || result.StructuredPayload == nil || !validNormalizedResultPayload(result.StructuredPayload) {
		return errors.New("accepted normalization correction has no matching reserved model turn")
	}
	last := &history[len(history)-1]
	last.Success, last.SubmittedResult, last.FailureReason = true, result, ""
	return c.checkpointProtocolRepairAttempt(item, agentName, last.Prompt, history, result, true, "", nil)
}

func (c *Coordinator) recoveredNormalizationRejection(ctx context.Context, item *TodoItem) (string, error) {
	if !normalizedDirectContinuationReceipt(item) || item.ExecutionReceipt.RepairProvenance.FailureReason != RepairFailureInvalidSchema {
		return "", nil
	}
	recovery, err := c.loadDirectNormalizationRecovery(ctx, item)
	if err != nil {
		return "", err
	}
	return protocolRepairRejectedSubmission(&toolCallEvidence{
		rejectedSubmitInput: string(recovery.submission.Wire),
		rejectedSubmitError: item.ExecutionReceipt.RepairProvenance.Error,
	}, nil), nil
}

func (c *Coordinator) materializeDirectNormalizationReceipt(item *TodoItem, result *TaskResult) error {
	proof := item.ExecutionReceipt.InterruptedResultNormalization
	payload := result.StructuredPayload
	if payload == nil || payload.Normalization == nil {
		return errors.New("direct normalization continuation has no canonical payload and normalization evidence")
	}
	actual := payload.Normalization.evidence()
	actual.SubmissionSHA256 = proof.SubmissionSHA256
	if actual != proof || payload.RawSHA256 != proof.RawSHA256 {
		return errors.New("direct normalization continuation does not match the interrupted receipt")
	}
	receipt := cloneExecutionReceipt(item.ExecutionReceipt)
	provenance := receipt.RepairProvenance
	provenance.Success, provenance.SubmittedResult = true, result
	provenance.FailureReason, provenance.Error = "", ""
	c.applyResultContractReceipt(item.ID, result, &receipt)
	if err := c.setAttemptReceipt(item.ID, &receipt); err != nil {
		return err
	}
	return c.emitTaskEventsFromCheckpoint(c.taskTracker.TodoList().Items())
}

// Rebuild runner-owned outputs from the frozen work receipt. The current
// dispatch owns publication, and only the original dispatch owns evidence.
func (c *Coordinator) finalizeRecoveredNormalizedResult(ctx context.Context, item *TodoItem, task TaskDef, result *TaskResult, output string) (*TaskResult, string, error) {
	if HasBlockingInvariantAssessment(item.InvariantVerification, result.InvariantVerification) {
		return nil, "", withFailureClassOverride(errors.New("runtime-attested invariant gate rejected the worker result"), FailureExecution)
	}
	if err := c.validateNormalizedCompletionSources(ctx, item); err != nil {
		return nil, "", err
	}
	if item.Status == TaskVerifying {
		return result, item.Output, nil
	}
	receipt := cloneExecutionReceipt(item.ExecutionReceipt)
	projection, err := c.directNormalizedWorkMedia(ctx, item, result, &receipt)
	if err != nil {
		return nil, "", err
	}
	transcript, err := c.normalizationTranscriptForOutput(ctx, task, &receipt)
	if err != nil {
		return nil, "", rejectNormalizationRecoveryRead(err)
	}
	if projection == nil && transcript == nil {
		return result, output, nil
	}
	identity, ok := c.activeTaskResultOccurrence(item.ID)
	if !ok {
		return nil, "", rejectLocalResult(RepairFailureSource, errTaskResultStale)
	}
	output, err = c.finalizeTaskResultRuntime(identity, func(finalized *TaskResult) (string, error) {
		if projection != nil {
			attachTaskRuntimeMedia(finalized, projection)
		}
		if transcript == nil {
			return output, nil
		}
		manifest, err := finalizeVerbatimTaskResult(transcript, finalized)
		if err != nil {
			return "", err
		}
		return manifest, taskResultSubmissionContractForTask(task).validateTranscriptFinalization(finalized)
	})
	if err != nil {
		return nil, "", rejectNormalizationRecoveryRead(err)
	}
	result = c.GetTaskResult(item.ID)
	if result == nil {
		return nil, "", rejectLocalResult(RepairFailureSource, errTaskResultStale)
	}
	receipt.RuntimeMediaHash = runtimeMediaHash(result)
	receipt.SubmittedResult = result
	if receipt.RepairProvenance != nil {
		receipt.RepairProvenance.SubmittedResult = result
	}
	if err := c.setAttemptReceipt(item.ID, &receipt); err != nil {
		return nil, "", rejectLocalResultPersistence(err)
	}
	return result, output, nil
}

func (c *Coordinator) resumeNormalizedVerification(ctx context.Context, task TaskDef, item *TodoItem) (string, error) {
	result, err := c.normalizedVerificationResult(item)
	if err != nil {
		return "", c.settleNormalizedRecoveryRead(item, task, rejectNormalizationRecoveryRead(err))
	}
	c.storeSubmittedTaskResult(item.ID, result)
	return c.finishProtocolRepair(ctx, item, task, item.Agent, task.Model, result, item.Output)
}

func (c *Coordinator) validateNormalizedCompletionSources(ctx context.Context, item *TodoItem) error {
	if err := c.validateProtocolRepairEvidenceReceipt(item, item.ResultContract); err != nil {
		return rejectResultSourceRead(err)
	}
	receipt := item.ExecutionReceipt
	if err := c.verifyResumeTranscript(ctx, receipt, taskTranscriptMediaType); err != nil {
		return rejectNormalizationRecoveryRead(err)
	}
	source, ok := normalizationWorkEvidenceIdentity(item)
	if !ok || item.TypedResult == nil {
		return rejectResultSourceRead(errors.New("normalized completion has no accepted artifact scope"))
	}
	if err := c.verifyNormalizedResultArtifacts(ctx, source, item.TypedResult, receipt); err != nil {
		return rejectNormalizationRecoveryRead(err)
	}
	if taskResultStatusIsSuccessful(item.TypedResult.Status) {
		if err := c.validateRequiredEvidenceDeliveryForResult(ctx, item.ID, receipt.Attempt, item.TypedResult, receipt.RunID); err != nil {
			return rejectNormalizationRecoveryRead(err)
		}
	}
	return nil
}

func (c *Coordinator) checkpointWorkerCompletion(ctx context.Context, taskID string, receipt *ExecutionReceipt, result *TaskResult, output string, normalized bool) error {
	durableOutput := ""
	detail := "running objective verification"
	if normalized {
		if !isCommittedNormalizedCollection(c.todoItemByID(taskID)) {
			receipt.SubmittedResult = result
			receipt.HandoffState = ResultHandoffSubmitted
			if err := c.setAttemptReceipt(taskID, receipt); err != nil {
				return err
			}
		}
		durableOutput = output
		detail = "settling accepted normalized submission"
	}
	return c.commitTaskTransitionFromCurrent(ctx, taskID, TaskVerifying, detail, durableOutput, nil)
}

func (c *Coordinator) normalizedCompletionWriteFailure(ctx context.Context, taskID, output string, err error) error {
	failure := rejectLocalResultPersistence(err)
	if isCommittedNormalizedCollection(c.todoItemByID(taskID)) {
		return failure
	}
	return errors.Join(failure, c.checkpointNormalizedContinuation(ctx, taskID, output))
}

func (c *Coordinator) normalizedVerificationResult(item *TodoItem) (*TaskResult, error) {
	result := item.TypedResult
	if !normalizedWorkerAcceptedHandoff(item.ResultContract, item.ExecutionReceipt, result) ||
		item.Status == TaskVerifying && !isCompletedLocalResult(result) {
		return nil, errors.New("verifying normalized task lacks its committed matching result receipt")
	}
	publication, found, err := c.readNormalizedVerificationPublication(item)
	if err != nil {
		return nil, err
	}
	// The objective outcome is written separately, after verification starts.
	receipt := *item.ExecutionReceipt
	receipt.VerifyResult = publication.Receipt.VerifyResult
	if item.Status == TaskVerifying && !found || found && !matchesNormalizedPublication(receipt, item.Output, publication) {
		return nil, errors.New("recovering normalized result differs from the sealed verification publication")
	}
	if payload := result.StructuredPayload; payload != nil {
		evidence := payload.Normalization
		receipt := item.ExecutionReceipt
		occurrence, err := c.acceptedNormalizationOccurrence(item, evidence)
		if err != nil {
			return nil, err
		}
		if occurrence != evidence.OccurrenceSHA256 || receipt.ResultNormalizerOccurrenceSHA256 != occurrence {
			return nil, errors.New("normalized result does not match the receipt's stage occurrence")
		}
		stage, _, err := c.readStagedNormalization(payload.Contract, evidence.OccurrenceSHA256, payload.RawSHA256)
		if err != nil {
			return nil, err
		}
		if stage == nil || !bytes.Equal(stage.Raw, evidence.Raw) || !bytes.Equal(stage.Output, evidence.Output) {
			return nil, errors.New("normalized result does not match the sealed stage bytes")
		}
	}
	return result, nil
}

func matchesNormalizedPublication(receipt ExecutionReceipt, output string, published normalizedVerificationPublication) bool {
	actual, actualErr := marshalNormalizedCheckpoint(normalizedVerificationPublication{Receipt: receipt, Output: output})
	expected, expectedErr := marshalNormalizedCheckpoint(published)
	return actualErr == nil && expectedErr == nil && bytes.Equal(actual, expected)
}

func (c *Coordinator) normalizedVerificationSteps(ctx context.Context, item *TodoItem, task TaskDef) ([]fantasy.StepResult, error) {
	if task.VerifySpec == nil || NormalizeVerificationSpec(*task.VerifySpec, task.Verify, task.VerifyMode).Type != VerifyToolCallAssert {
		return nil, nil
	}
	receipt := item.ExecutionReceipt
	transcript, err := c.restoreTranscriptEvidence(ctx, receipt, receipt.RunID)
	if err != nil {
		return nil, rejectNormalizationRecoveryRead(err)
	}
	return originalToolVerificationSteps(transcript.evidenceRecords()), nil
}

func originalToolVerificationSteps(records []taskTranscriptRecord) []fantasy.StepResult {
	var content fantasy.ResponseContent
	for _, record := range records {
		switch record.Event {
		case "tool_call":
			content = append(content, fantasy.ToolCallContent{ToolCallID: record.ToolCallID, ToolName: record.Tool, Input: record.Input})
		case "tool_result":
			content = append(content, fantasy.ToolResultContent{
				ToolCallID: record.ToolCallID, ToolName: record.Tool,
				Result: fantasy.ToolResultOutputContentText{Text: record.Output},
			})
		}
	}
	return []fantasy.StepResult{{Response: fantasy.Response{Content: content}}}
}

func (c *Coordinator) normalizationTranscriptForOutput(ctx context.Context, task TaskDef, receipt *ExecutionReceipt) (*taskTranscript, error) {
	if !taskUsesVerbatimTranscript(task) {
		return nil, nil
	}
	if err := c.verifyResumeTranscript(ctx, receipt, taskTranscriptMediaType); err != nil {
		return nil, err
	}
	store, err := OpenFileArtifactStoreReadOnly(c.artifactStoreRootPath(), c.artifactStoreRootPath())
	if err != nil {
		return nil, err
	}
	ref, err := store.Get(ctx, receipt.TranscriptRef)
	if err != nil {
		return nil, err
	}
	if ref.MediaType != taskTranscriptMediaType {
		return nil, errors.New("verbatim receipt does not reference a task transcript")
	}
	return &taskTranscript{todoID: receipt.TaskID, runID: receipt.RunID, attempt: receipt.Attempt, agent: receipt.ProducerID, sealed: &ref}, nil
}
