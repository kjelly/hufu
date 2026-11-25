package team

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"

	"charm.land/fantasy"
)

type resultCollectionAdmissionKey struct{}

// Collection publishes only fields that the durable task can accept. Keep the
// legacy tool schema separate so persisted tool sessions retain compatibility.
func collectionSubmissionInfo(c *Coordinator, taskID string) fantasy.ToolInfo {
	info := localResultSubmissionInfo(c, taskID)
	if localResultSubmissionContract(c, taskID).InvariantVerification == "" {
		delete(info.Parameters, "invariant_assessments")
	}
	return info
}

// The collector receives JSON as prompt text, not a provider tool schema.
// Show validation bounds and use the same shape for exhaustive repair feedback.
// Admission still validates the original pinned contract after binding sources.
func collectionGuidanceInfo(c *Coordinator, taskID string) (fantasy.ToolInfo, error) {
	info := collectionSubmissionInfo(c, taskID)
	ref, compiled, err := c.boundResultContract(taskID)
	if err != nil || compiled == nil {
		return info, err
	}
	plans, err := c.fixedCollectionPlans(taskID)
	if err != nil {
		return info, err
	}
	if ref.Normalizer != "" {
		return info, nil
	}
	schema := fixedCollectionPayloadSchema(*ref, compiled, plans, false, true)
	advertiseResultEvidenceKeys(schema, compiled.evidenceKeys)
	advertiseResultWorksetPaths(schema, compiled.worksetPaths)
	info.Parameters["structured_payload"] = providerResultIntegerStrings(schema, 0)
	return info, nil
}

// decodeCollectionClaims accepts exactly the published model claims schema.
// Only the schema's declared file-entry shorthand is normalized; text is never
// unwrapped and unsupported shorthand remains invalid.
func (c *Coordinator) decodeCollectionClaims(taskID string, raw []byte) (SubmitResultInput, error) {
	var input SubmitResultInput
	if len(bytes.TrimSpace(raw)) == 0 {
		return input, rejectLocalResult(RepairFailureResultMissing, fmt.Errorf("collector returned no result"))
	}
	if len(raw) > resultCollectionResponseMaxBytes || len(bytes.TrimSpace(raw)) == 0 || bytes.TrimSpace(raw)[0] != '{' {
		return input, rejectLocalResult(RepairFailureInvalidSchema, fmt.Errorf("collection response must be one bounded JSON object"))
	}
	claims, err := decodeUniqueJSON(raw)
	if err != nil {
		return input, rejectLocalResult(RepairFailureInvalidSchema, err)
	}
	fields, _ := claims.(map[string]any)
	_, statusSupplied := fields["status"]
	_, summarySupplied := fields["summary"]
	input.completionFieldsSupplied = statusSupplied || summarySupplied
	_, input.receiptIDsSupplied = fields["receipt_ids"]
	_, input.memoryUsesSupplied = fields["memory_uses"]
	info := collectionSubmissionInfo(c, taskID)
	if err := validateSubmissionFieldPresence(string(raw), info); err != nil {
		return input, rejectLocalResult(RepairFailureInvalidSchema, err)
	}
	if err := validateToolArguments(string(raw), info); err != nil {
		return input, rejectLocalResult(RepairFailureInvalidSchema, err)
	}
	if err := c.validateFixedCollectionInputJSON(taskID, string(raw)); err != nil {
		return input, rejectLocalResult(RepairFailureContract, err)
	}
	normalized, err := normalizeCollectionFileRefs(raw)
	if err != nil {
		return input, rejectLocalResult(RepairFailureInvalidSchema, err)
	}
	if err := json.Unmarshal(normalized, &input, json.RejectUnknownMembers(true), collectionDurationFormat); err != nil {
		return input, rejectLocalResult(RepairFailureInvalidSchema, err)
	}
	if input.Summary == "" && (!localResultSubmissionContract(c, taskID).runtimeCompletion || input.Status != "") {
		return input, rejectLocalResult(RepairFailureInvalidSchema, fmt.Errorf("summary is required"))
	}
	if ref, _, _ := c.boundResultContract(taskID); ref != nil && ref.Normalizer != "" {
		input.normalizerDraft, err = submittedNormalizerDraft(raw, false)
		if err != nil {
			return input, rejectLocalResult(RepairFailureInvalidSchema, err)
		}
	}
	return input, nil
}

func (c *Coordinator) prepareCollectionClaims(ctx context.Context, expected *ResultCollectionCheckpoint) (*ResultCollectionCheckpoint, error) {
	ref, compiled, err := c.boundResultContract(expected.TaskID)
	if err != nil {
		return nil, rejectLocalResult(RepairFailureContract, err)
	}
	if ref != nil && ref.Normalizer != "" {
		controller := c.occurrenceController(expected.TaskID)
		controller.normalizationMu.Lock()
		defer controller.normalizationMu.Unlock()
		current := collectionForReceipt(c.todoItemByID(expected.TaskID), collectionReceipt(c.todoItemByID(expected.TaskID), expected))
		if current == nil || current.Revision != expected.Revision || current.State != ResultCollectionReceived {
			return nil, rejectLocalResult(RepairFailureSource, errResultCollectionStale)
		}
	}
	ctx, err = c.restoreCollectionContext(ctx, expected)
	if err != nil {
		return nil, rejectResultSourceRead(err)
	}
	response, err := c.readCollectionBlobRecord(ctx, expected, "response", expected.Response)
	if err != nil {
		return nil, rejectResultSourceRead(err)
	}
	work, err := c.loadCollectionWork(ctx, c.todoItemByID(expected.TaskID), expected)
	if err != nil {
		return nil, rejectResultSourceRead(err)
	}
	ctx = context.WithValue(ctx, resultCollectionAdmissionKey{}, expected)
	// Bind the exact raw response in the checkpoint, then parse its JSON frame.
	raw, _ := collectionResponseJSON(response.Data)
	var omissions resultSourceOmissions
	if response.ResponseIssue == nil {
		omissions = submissionSourceOmissions(compiled, string(raw), false)
	}
	if err := c.validateIndependentSubmissionSources(ctx, expected.TaskID, compiled, omissions); err != nil {
		return nil, rejectResultSourceRead(err)
	}
	sourceErr := c.validateSubmissionSourceEnvelope(ctx, expected.TaskID, work.Workspace, string(raw), expected, compiled)
	if sourceErr != nil && localResultRejectionReason(sourceErr) != RepairFailureInvalidSchema {
		if _, typed := errors.AsType[*localResultAdmissionError](sourceErr); typed {
			return nil, sourceErr
		}
		return nil, rejectResultSourceRead(sourceErr)
	}
	if response.ResponseIssue != nil {
		if err := c.validateRejectedCollectionResponseSources(ctx, expected, work, compiled, response, false); err != nil {
			return nil, rejectNormalizationRecoveryRead(err)
		}
		return nil, rejectLocalResult(RepairFailureInvalidSchema, errors.New(response.ResponseIssue.Diagnostic()))
	}
	if sourceErr == nil && ref != nil && ref.Normalizer != "" && response.ResponseRedacted {
		return nil, rejectLocalResult(RepairFailureInvalidSchema, errors.New("omit credentials, tokens, and keys from the normalization submission"))
	}
	var input SubmitResultInput
	err = sourceErr
	if err == nil {
		input, err = c.decodeCollectionClaims(expected.TaskID, raw)
	}
	if err == nil {
		err = validateStoppedWorkClaims(work, input, localResultSubmissionContract(c, expected.TaskID).runtimeCompletion)
	}
	if err != nil {
		if ref != nil && ref.Normalizer == "" && localResultRejectionReason(err) == RepairFailureInvalidSchema {
			if sourceErr := c.validateReadablePayloadSources(ctx, expected.TaskID, compiled, string(raw), false); sourceErr != nil {
				return nil, sourceErr
			}
		}
		return nil, err
	}
	var normalization *resultNormalization
	if ref != nil && ref.Normalizer != "" && hasNormalizerDraft(modelNormalizerDraft(input)) {
		if _, err := c.validateCollectionSourceClaims(ctx, expected, work, input); err != nil {
			return nil, err
		}
		normalization, err = c.stageResultNormalization(ctx, expected.TaskID, modelNormalizerDraft(input), expected)
		if err != nil {
			return nil, err
		}
	}
	next, err := c.updateResultCollection(ctx, expected, func(next *ResultCollectionCheckpoint, _ *ExecutionReceipt) error {
		if next.State != ResultCollectionReceived {
			return errResultCollectionStale
		}
		ref, err := c.putCollectionBlobRecord(ctx, next, "claims", raw, nil, normalization)
		if err != nil {
			return rejectLocalResultPersistence(err)
		}
		next.State, next.Prepared = ResultCollectionPrepared, ref
		return nil
	})
	if err != nil {
		return nil, rejectNormalizationRecoveryRead(err)
	}
	return next, nil
}

// Prepared claims must still match the output sealed for their original
// draft, workspace and dispatch. The embedded snapshot is checked as a copy.
func (c *Coordinator) readCollectionNormalization(ref *ResultContractRef, cp *ResultCollectionCheckpoint, workspace string, draft []byte, embedded *resultNormalization, prepared bool) (*resultNormalization, error) {
	if ref == nil || ref.Normalizer == "" || !hasNormalizerDraft(draft) {
		if embedded != nil {
			return nil, rejectLocalResult(RepairFailureSource, errors.New("prepared normalization has no matching draft"))
		}
		return nil, nil
	}
	occurrence := normalizationOccurrence(submitResultRuntimeIdentity{}, cp)
	stage, found, err := readStagedNormalizationAt(workspaceDocs(workspace), c.artifactStoreRootPath(), *ref, occurrence, sha256HexBytes(draft))
	if err != nil {
		return nil, rejectNormalizationRecoveryRead(err)
	}
	if prepared && (stage == nil || !sameCanonicalEvidence(embedded, stage)) {
		return nil, rejectLocalResult(RepairFailureSource, errors.New("prepared normalization does not match the durable output"))
	}
	if found && stage == nil {
		return nil, rejectLocalResult(RepairFailureAction, errors.New("normalizer invocation has no durable output"))
	}
	return stage, nil
}

// commitPreparedCollection revalidates durable claims through ordinary local
// admission. It neither constructs a provider nor executes a tool handler.
func (c *Coordinator) commitPreparedCollection(ctx context.Context, expected *ResultCollectionCheckpoint) (commitErr error) {
	if expected == nil || expected.State != ResultCollectionPrepared {
		return errResultCollectionStale
	}
	var err error
	ctx, err = c.restoreCollectionContext(ctx, expected)
	if err != nil {
		return rejectResultSourceRead(err)
	}
	item := c.todoItemByID(expected.TaskID)
	work, err := c.loadCollectionWork(ctx, item, expected)
	if err != nil {
		return rejectResultSourceRead(err)
	}
	prepared, err := c.readCollectionBlobRecord(ctx, expected, "claims", expected.Prepared)
	if err != nil {
		return rejectResultSourceRead(err)
	}
	raw := prepared.Data
	ref, compiled, err := c.boundResultContract(item.ID)
	if err != nil {
		return rejectLocalResult(RepairFailureContract, err)
	}
	ctx = context.WithValue(ctx, resultCollectionAdmissionKey{}, expected)
	if ref != nil && ref.Normalizer == "" {
		defer func() {
			if commitErr != nil && localResultRejectionReason(commitErr) == RepairFailureInvalidSchema {
				if err := c.validateReadablePayloadSources(ctx, item.ID, compiled, string(raw), false); err != nil {
					commitErr = err
				}
			}
		}()
	}
	input, err := c.decodeCollectionClaims(item.ID, raw)
	if err != nil {
		return err
	}
	if err := validateStoppedWorkClaims(work, input, localResultSubmissionContract(c, expected.TaskID).runtimeCompletion); err != nil {
		return err
	}
	input, err = c.validateCollectionSourceClaims(ctx, expected, work, input)
	if err != nil {
		return err
	}
	stage, err := c.readCollectionNormalization(ref, expected, work.Workspace, input.normalizerDraft, prepared.Normalization, true)
	if err != nil {
		return err
	}
	// Resolve only keys issued by successful authorized views in the original
	// work occurrence, after checking independent receipt and artifact claims.
	resolved, err := c.resolveSubmissionEvidenceKeys(ctx, item.ID, string(raw), input.normalizerDraft)
	if err != nil {
		return c.collectionEvidenceRejection(ctx, expected, work, string(raw), err)
	}
	decoded, err := c.decodeCollectionClaims(item.ID, []byte(resolved))
	if err != nil {
		return err
	}
	decoded.ReceiptIDs, decoded.collectionSourcesBound = input.ReceiptIDs, input.collectionSourcesBound
	decoded.normalizerDraft, decoded.resultNormalization = input.normalizerDraft, stage
	admission := localResultAdmission{coordinator: c, todoID: item.ID}
	if normalizationContract(item) {
		admission.normalizedCollectionWork = &work.Receipt
	}
	err = admission.admit(ctx, decoded, localResultSubmissionContract(c, item.ID), func() RepairFailureReason {
		// validateStructuredPayloadClaims classifies canonical model-schema
		// failures after independent source checks. Other failures stay closed.
		return RepairFailureSource
	})
	if isResultReferenceChoiceError(err) {
		return c.collectionEvidenceRejection(ctx, expected, work, resolved, err)
	}
	return err
}

// Independent sources are checked before a provider or model can be asked to
// reshape a submission. A malformed payload cannot conceal forged references.
func (c *Coordinator) validateCollectionSourceClaims(ctx context.Context, cp *ResultCollectionCheckpoint, work *resultCollectionWork, input SubmitResultInput) (SubmitResultInput, error) {
	if err := c.bindCollectionReceiptFields(ctx, cp, &input); err != nil {
		return input, err
	}
	claims := input.taskResult()
	claims.Attempt = cp.Attempt
	if len(claims.AssumptionChecks) > 0 && len(collectionRepairGrants(collectionReceipt(c.todoItemByID(cp.TaskID), cp))) > 0 {
		if err := c.validateInertCollectionAssumptions(ctx, cp, claims.AssumptionChecks); err != nil {
			return input, rejectResultSourceRead(err)
		}
	}
	if err := c.validateTaskResultReceiptClaims(ctx, cp.TaskID, &claims); err != nil {
		return input, rejectResultSourceRead(err)
	}
	if len(claims.Artifacts) > 0 {
		if err := validateSubmittedArtifactClaims(ctx, work.Workspace, claims.Artifacts); err != nil {
			return input, rejectResultSourceRead(err)
		}
	}
	return input, nil
}

func (c *Coordinator) acceptedCollectionResult(ctx context.Context, item *TodoItem, checkpoint *ResultCollectionCheckpoint) (*TaskResult, error) {
	if checkpoint == nil || checkpoint.State != ResultCollectionCommitted {
		return nil, errResultCollectionStale
	}
	if _, err := c.loadCollectionWork(ctx, item, checkpoint); err != nil {
		return nil, err
	}
	return c.readAcceptedCollectionResult(ctx, item, checkpoint)
}

func (c *Coordinator) readAcceptedCollectionResult(ctx context.Context, item *TodoItem, checkpoint *ResultCollectionCheckpoint) (*TaskResult, error) {
	body, err := c.readCollectionBlob(ctx, checkpoint, "accepted", checkpoint.Accepted)
	if err != nil {
		return nil, err
	}
	var result TaskResult
	if err := json.Unmarshal(body, &result, json.RejectUnknownMembers(true), collectionDurationFormat); err != nil {
		return nil, err
	}
	if result.TaskID != checkpoint.TaskID || result.Attempt != checkpoint.Attempt || result.Agent != checkpoint.Agent || !isLocalSubmittedResultSource(result.Source) {
		return nil, errResultCollectionStale
	}
	if payload := result.StructuredPayload; payload != nil {
		receipt := collectionReceipt(item, checkpoint)
		if receipt == nil || !collectionReceiptMatchesResultPayload(receipt, checkpoint, payload) {
			return nil, errResultCollectionStale
		}
	}
	return &result, nil
}

func collectionReceiptMatchesResultPayload(receipt *ExecutionReceipt, checkpoint *ResultCollectionCheckpoint, payload *ResultPayload) bool {
	return receiptMatchesResultPayload(receipt, payload) && (payload.Normalization == nil ||
		payload.Normalization.OccurrenceSHA256 == normalizationOccurrence(submitResultRuntimeIdentity{}, checkpoint))
}

// Persist the accepted canonical bytes before publishing the occurrence latch.
// Lifecycle transitions and competing submissions use the same projection lock.
func (tx *taskResultOccurrenceTransaction) commitCollection(ctx context.Context, result *TaskResult, expected *ResultCollectionCheckpoint) error {
	if tx == nil || tx.controller == nil || tx.finished || result == nil || expected == nil {
		return errTaskResultStale
	}
	if err := tx.validateCommit(ctx, result); err != nil {
		return err
	}
	c := tx.coordinator
	c.taskProjectionMu.Lock()
	defer c.taskProjectionMu.Unlock()
	tx.controller.projectionMu.Lock()
	defer tx.controller.projectionMu.Unlock()
	snapshot := tx.controller.snapshot()
	if !snapshot.opened || !snapshot.reserved || snapshot.result != nil || !sameTaskResultOccurrence(snapshot.identity, tx.identity) ||
		tx.identity.RunID != expected.SourceRunID || tx.identity.TaskID != expected.TaskID || tx.identity.Attempt != expected.Attempt ||
		tx.identity.OccurrenceRevision != expected.SubmissionRevision || tx.identity.DispatchID != expected.SubmissionDispatchID {
		return errTaskResultStale
	}
	item := c.todoItemByID(expected.TaskID)
	receipt := collectionReceipt(item, expected)
	if receipt == nil {
		return errResultCollectionStale
	}
	pinned, err := collectionDigest(expected)
	if err != nil {
		return err
	}
	current, err := collectionDigest(receipt.ResultCollection)
	if err != nil || current != pinned || expected.State != ResultCollectionPrepared {
		return errResultCollectionStale
	}
	work, err := c.loadCollectionWork(ctx, item, expected)
	if err != nil {
		return rejectResultSourceRead(err)
	}
	canonical := cloneTaskResult(result)
	canonical.TaskID, canonical.Attempt, canonical.Agent = tx.identity.TaskID, tx.identity.Attempt, tx.identity.Agent
	if err := c.attachCollectedTranscript(ctx, item, expected, canonical); err != nil {
		return rejectResultSourceRead(err)
	}
	body, err := json.Marshal(canonical, json.Deterministic(true), collectionDurationFormat)
	if err != nil {
		return err
	}
	next := cloneResultCollectionCheckpoint(expected)
	next.Revision++
	next.Accepted, err = c.putCollectionBlob(ctx, next, "accepted", body)
	if err != nil {
		return err
	}
	next.State = ResultCollectionCommitted
	if work.StopReason == workStopToolLoop {
		// Preserve the failed work receipt in Work, while publishing the
		// accepted collection's terminal outcome on the current receipt.
		collected := cloneExecutionReceipt(receipt)
		collected.ExitCode = new(0)
		receipt = &collected
	}
	if err := c.appendCollectionCheckpointLocked(ctx, item, receipt, next, canonical); err != nil {
		return err
	}
	tx.controller.mu.Lock()
	tx.controller.result = canonical
	tx.controller.reserved = false
	tx.controller.pending = nil
	tx.controller.mu.Unlock()
	return c.publishTaskResultProjectionLocked(tx.identity, canonical, tx.controller)
}
