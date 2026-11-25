package team

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/kjelly/hufu/internal/media"
	"github.com/kjelly/hufu/internal/runtimestore"
	"github.com/kjelly/hufu/internal/tools"
)

// localResultAdmission owns validation and occurrence commit for local worker
// claims. Transport adapters decode their input and map these typed rejections;
// admission consumes authorized drafts and leaves task completion to the coordinator.
type localResultAdmission struct {
	coordinator              *Coordinator
	todoID                   string
	sink                     TaskResultSink
	normalizedCollectionWork *ExecutionReceipt
}

type localResultAdmissionError struct {
	reason RepairFailureReason
	cause  error
}

func (e *localResultAdmissionError) Error() string { return e.cause.Error() }
func (e *localResultAdmissionError) Unwrap() error { return e.cause }

func rejectLocalResult(reason RepairFailureReason, err error) error {
	return &localResultAdmissionError{reason: reason, cause: err}
}

func localResultRejectionReason(err error) RepairFailureReason {
	if collection, ok := errors.AsType[*resultCollectionFailure](err); ok {
		return collection.Reason
	}
	if rejection, ok := errors.AsType[*localResultAdmissionError](err); ok {
		return rejection.reason
	}
	return RepairFailureContract
}

func resultSourceReadFailureReason(err error) RepairFailureReason {
	_, fileFault := errors.AsType[*os.PathError](err)
	mediaFault, classifiedMedia := errors.AsType[*media.Error](err)
	if runtimestore.IsStorageFault(err) || (fileFault && !errors.Is(err, os.ErrNotExist)) ||
		(classifiedMedia && mediaFault.Code == media.DurabilityRequired) {
		return RepairFailureCommit
	}
	return RepairFailureSource
}

func rejectResultSourceRead(err error) error {
	return rejectLocalResult(resultSourceReadFailureReason(err), err)
}

func rejectLocalResultPersistence(err error) error {
	if err == nil {
		return nil
	}
	if _, typed := errors.AsType[*localResultAdmissionError](err); typed {
		return err
	}
	reason := RepairFailureCommit
	if errors.Is(err, errTaskResultStale) || errors.Is(err, errResultCollectionStale) || errors.Is(err, errArtifactCASConflict) {
		reason = RepairFailureSource
	}
	return rejectLocalResult(reason, err)
}

// admit consumes schema-decoded claims. The optional classifier distinguishes a
// model-facing payload shape error from failure to bind trusted source data.
// It cannot grant authority or replace the canonical payload validator.
func (a *localResultAdmission) admit(ctx context.Context, input SubmitResultInput, contract taskResultSubmissionContract, classifyPayloadRejection func() RepairFailureReason) (admissionErr error) {
	var submissionSHA256 string
	defer func() {
		if input.resultNormalization != nil && admissionErr != nil && localResultRejectionReason(admissionErr) == RepairFailureCommit {
			evidence := input.resultNormalization.evidence()
			evidence.SubmissionSHA256 = submissionSHA256
			admissionErr = &normalizedResultCommitError{cause: admissionErr, evidence: evidence}
		}
	}()
	if a.coordinator != nil {
		ref, _, err := a.coordinator.boundResultContract(a.todoID)
		if err != nil {
			return rejectLocalResult(RepairFailureContract, err)
		}
		cp, _ := ctx.Value(resultCollectionAdmissionKey{}).(*ResultCollectionCheckpoint)
		if ref != nil && ref.Normalizer != "" && cp == nil && hasNormalizerDraft(modelNormalizerDraft(input)) {
			controller := a.coordinator.occurrenceController(a.todoID)
			controller.normalizationMu.Lock()
			defer controller.normalizationMu.Unlock()
			input.normalizerDraft = modelNormalizerDraft(input)
			claims := input.taskResult()
			identity, err := submitResultRuntimeIdentityFromContext(ctx, a.coordinator, a.todoID)
			if err != nil {
				return rejectLocalResult(RepairFailureSource, err)
			}
			claims.Attempt = identity.Attempt
			if err := a.coordinator.validateTaskResultReceiptClaims(ctx, a.todoID, &claims); err != nil {
				return rejectResultSourceRead(err)
			}
			if len(claims.Artifacts) > 0 {
				if protocolRepairExecution(ctx) && !hasSealedDirectNormalization(ctx) {
					return rejectLocalResult(RepairFailureContract, errors.New("protocol repair cannot add artifact evidence"))
				}
				if a.forbidsArtifacts() {
					return rejectLocalResult(RepairFailureContract, errors.New("artifacts are forbidden by this task's execution contract"))
				}
				if err := a.coordinator.validateDirectResultArtifactClaims(ctx, a.coordinator.session.Workspace, claims.Artifacts); err != nil {
					return rejectResultSourceRead(err)
				}
			}
			normalized, err := a.coordinator.normalizeDirectSubmission(ctx, identity, input)
			if normalized != nil {
				input.resultNormalization = normalized.stage
				submissionSHA256 = normalized.digest
			}
			if err != nil {
				return err
			}
			input.normalizedArtifactRefs = normalized.submission.Artifacts
			if validSubmitResultIdentity(normalized.submission.EvidenceIdentity) {
				ctx = context.WithValue(ctx, normalizedCorrectionAcceptanceKey{}, normalized)
			}
		}
		input, err = a.coordinator.bindStagedNormalization(ctx, a.todoID, input)
		if err != nil {
			return err
		}
	}
	input, err := a.coordinator.projectResultClaims(ctx, a.todoID, input)
	if err != nil {
		return rejectProjectedClaims(err)
	}
	res, err := a.validateClaims(ctx, input, contract, classifyPayloadRejection, true)
	if err != nil {
		return err
	}
	err = a.commit(ctx, input, res, contract)
	if err != nil {
		if _, classified := errors.AsType[*localResultAdmissionError](err); !classified {
			return rejectLocalResult(RepairFailureContract, err)
		}
	}
	return err
}

// validateClaims is the pure claims boundary; admission and recovery use the
// same rules before any occurrence reservation or artifact materialization.
func (a *localResultAdmission) validateClaims(ctx context.Context, input SubmitResultInput, contract taskResultSubmissionContract, classifyPayloadRejection func() RepairFailureReason, recordFailure bool) (_ *TaskResult, admissionErr error) {
	reason := RepairFailureInvalidSchema
	defer func() {
		if admissionErr != nil {
			if _, classified := errors.AsType[*localResultAdmissionError](admissionErr); !classified {
				admissionErr = rejectLocalResult(reason, admissionErr)
			}
		}
	}()
	if a.coordinator != nil {
		var err error
		input, err = a.coordinator.bindStagedNormalization(ctx, a.todoID, input)
		if err != nil {
			return nil, err
		}
		input, err = a.coordinator.projectResultClaims(ctx, a.todoID, input)
		if err != nil {
			return nil, rejectProjectedClaims(err)
		}
	}
	var compiled *CompiledResultContract
	if a.coordinator != nil {
		var err error
		_, compiled, err = a.coordinator.boundResultContract(a.todoID)
		if err != nil {
			return nil, rejectLocalResult(RepairFailureContract, err)
		}
		if compiled != nil && compiled.runtimeCompletion != "" {
			input.Status, input.Summary, err = bindRuntimeCompletion(compiled.runtimeCompletion, strings.ToLower(strings.TrimSpace(input.Status)), input.Summary, input.completionFieldsSupplied, func() (map[string]any, error) {
				return a.coordinator.resultRuntimeInputValuesWithContext(ctx, compiled, a.todoID)
			})
			if err != nil {
				return nil, err
			}
		}
	}
	res := input.taskResult()
	if err := validateResultSummaryAndStatus(&res); err != nil {
		return nil, err
	}
	if runtimeCompletionFailure(compiled, res.Status) && len(bytes.TrimSpace(input.StructuredPayload)) != 0 {
		return nil, fmt.Errorf("omit structured_payload for an unsuccessful runtime-completion result")
	}
	if err := a.coordinator.validateRequiredArtifactReadSubmission(ctx, a.todoID); err != nil {
		return nil, rejectResultSourceRead(err)
	}
	reason = RepairFailureContract
	if decision, blocked := a.coordinator.decisionBlock(ctx); blocked && res.Status != TaskResultStatusBlocked {
		return nil, errors.New(decisionBlockResultDenial(decision, res.Status))
	}
	workerContract := contract
	if contract.RuntimeOwnedFilesRead {
		if input.FilesRead != nil {
			return nil, errors.New("files_read is runtime-owned; omit it entirely")
		}
		workerContract.FilesReadMinItems = 0
	}
	if err := workerContract.validateWorkerClaims(&res); err != nil {
		return nil, errors.New("submit_result contract violation: " + err.Error())
	}
	if err := validateFindingSeverities(res.Findings); err != nil {
		return nil, errors.New("submit_result contract violation: " + err.Error())
	}
	if contract.InvariantVerification == "" && input.InvariantAssessments != nil {
		return nil, errors.New("submit_result contract violation: ordinary task must omit invariant_assessments or submit null")
	}
	if a.coordinator != nil {
		if err := a.bindStructuredClaims(ctx, compiled, input, &res, classifyPayloadRejection, recordFailure); err != nil {
			return nil, err
		}
		if err := a.bindResultEnvelope(ctx, compiled, input, &res, contract, recordFailure); err != nil {
			return nil, err
		}
	} else if len(bytes.TrimSpace(input.StructuredPayload)) > 0 {
		return nil, errors.New("structured_payload is not accepted for this task because it has no result contract; omit the field")
	}

	if protocolRepairExecution(ctx) && len(res.Artifacts) > 0 {
		recovery, recovering := ctx.Value(directNormalizationRecoveryKey{}).(*directNormalizationRecovery)
		if !recovering || input.resultNormalization != recovery.stage {
			return nil, errors.New("protocol repair cannot add artifact evidence; submit the corrected result without artifacts")
		}
	}
	if len(res.Outputs) > 0 {
		return nil, errors.New("outputs are runtime-owned; cite execution receipt_ids instead of declaring task outputs")
	}
	if res.RawOutputRef != nil {
		return nil, errors.New("raw_output_ref is runtime-owned; workers cannot declare or copy transcript references")
	}
	if a.forbidsArtifacts() && len(res.Artifacts) > 0 {
		return nil, errors.New("artifacts are forbidden by this task's execution contract; omit the artifacts field and submit the result again")
	}
	// A model-authored failure summary is useful context, but it is not the
	// canonical execution record. Prefix non-success results with the first
	// runtime-observed closed-sequence failure so downstream errors cannot
	// misidentify which slot or exit status actually failed.
	if !taskResultStatusIsSuccessful(res.Status) {
		if fact := taskToolSequenceFromContext(ctx).failureSummary(); fact != "" {
			workerSummary := strings.TrimSpace(res.Summary)
			res.Summary = fact
			if workerSummary != "" {
				res.Summary += ". Worker report: " + workerSummary
			}
			res.Findings = append(res.Findings, Finding{
				Category: "runtime_execution",
				Summary:  fact,
				Detail:   "Runtime-owned closed-sequence evidence; consult the sealed transcript for tool output.",
			})
		}
	}
	// Security: strip model-injected HMAC signatures from submit_result tool input
	for i := range res.Evidence {
		res.Evidence[i].SystemHMAC = ""
	}
	return &res, nil
}

func (a *localResultAdmission) bindStructuredClaims(ctx context.Context, compiled *CompiledResultContract, input SubmitResultInput, res *TaskResult, classifyPayloadRejection func() RepairFailureReason, recordFailure bool) error {
	var observations []taskTranscriptRecord
	if compiled != nil && compiled.toolEvidence != nil {
		var err error
		observations, err = submissionToolEvidence(ctx, a.coordinator, a.todoID)
		if err != nil {
			return rejectLocalResult(RepairFailureSource, err)
		}
	}
	if runtimeCompletionFailure(compiled, res.Status) || allowsUnsettledDecisions(compiled, res.Status, input.StructuredPayload) {
		return nil
	}
	payload, err, payloadFormat := a.coordinator.validateStructuredPayloadClaims(ctx, a.todoID, input.StructuredPayload, recordFailure, observations)
	if err != nil {
		payloadReason := resultSourceReadFailureReason(err)
		if classifyPayloadRejection != nil && payloadReason != RepairFailureCommit {
			payloadReason = classifyPayloadRejection()
			if payloadFormat {
				payloadReason = RepairFailureInvalidSchema
			}
		}
		return rejectLocalResult(payloadReason, err)
	}
	res.StructuredPayload = payload
	if payload != nil && input.resultNormalization != nil {
		payload.Normalization = input.resultNormalization.clone()
		payload.RawSHA256 = sha256HexBytes(payload.Normalization.Raw)
	}
	if payload != nil && payload.EvidenceDowngrades > 0 && !hasRuntimeCompletion(compiled) {
		res.Summary = "Result includes claims without matching successful tool evidence; consult the structured payload for downgraded findings."
	}
	return nil
}

// commit reserves the occurrence before materializing artifacts and publishing
// canonical results. Only the coordinator's existing lifecycle may complete it.
func (a *localResultAdmission) commit(ctx context.Context, input SubmitResultInput, res *TaskResult, contract taskResultSubmissionContract) error {
	sink := a.sink
	if sink == nil && a.coordinator != nil {
		sink = coordinatorTaskResultSink{coordinator: a.coordinator}
	}
	if a.coordinator != nil {
		identity, err := submitResultRuntimeIdentityFromContext(ctx, a.coordinator, a.todoID)
		if err != nil {
			return rejectLocalResult(RepairFailureSource, fmt.Errorf("invalid submit_result runtime identity: %w", err))
		}
		// Receipt admission still needs the active occurrence, but it must run
		// before the transaction mutex is held because that validator reads the
		// occurrence through the public snapshot helper. The attempt is runtime
		// supplied here, never worker-authored.
		receiptCandidate := *res
		receiptCandidate.Attempt = identity.Attempt
		if err := a.coordinator.validateTaskResultReceiptClaims(ctx, a.todoID, &receiptCandidate); err != nil {
			return rejectResultSourceRead(err)
		}
		metadata, _ := invocationMetadataFromContext(ctx)
		if err := a.coordinator.attestInvariantClaims(a.todoID, identity.Attempt, metadata.ModelExecutionID, input.InvariantAssessments, res); err != nil {
			if _, collecting := ctx.Value(resultCollectionAdmissionKey{}).(*ResultCollectionCheckpoint); collecting {
				if _, mismatch := errors.AsType[*invariantFindingSeverityMismatch](err); mismatch {
					return rejectLocalResult(RepairFailureInvalidSchema, fmt.Errorf("invalid invariant assessment: %w", err))
				}
			}
			return errors.New("invalid invariant assessment: " + err.Error())
		}
		a.coordinator.attachTaskKnowledgeCoverage(a.todoID, identity.Attempt, metadata.ModelExecutionID, res)
		if err := a.coordinator.validateAcceptedRepairClaims(ctx, currentResultCollection(a.coordinator.todoItemByID(a.todoID)), res); err != nil {
			return err
		}
		tx, txErr := a.coordinator.beginTaskResultSubmission(identity)
		if txErr != nil {
			return rejectLocalResultPersistence(fmt.Errorf("store submitted result: %w", txErr))
		}
		rollback := func(cause error) error {
			if err := tx.rollback(); err != nil {
				return rejectLocalResult(RepairFailureCommit, errors.Join(cause, fmt.Errorf("rollback submitted result: %w", err)))
			}
			return cause
		}
		if base, ok := ctx.Value(resultPatchBaseKey{}).(string); ok {
			current := a.coordinator.todoItemByID(a.todoID)
			if current == nil || current.ResultCandidate == nil || current.ResultCandidate.SHA256 != base {
				return rollback(rejectLocalResult(RepairFailureSource, errors.New("candidate_stale")))
			}
		}
		// Runtime ownership begins only after the occurrence is reserved. The
		// candidate now contains the values that worker-independent assertions
		// are allowed to inspect.
		res.TaskID = identity.TaskID
		res.Attempt = identity.Attempt
		res.Agent = identity.Agent
		res.Source = "submitted"
		if _, collecting := ctx.Value(resultCollectionAdmissionKey{}).(*ResultCollectionCheckpoint); collecting {
			res.Source = "collected"
		}
		if res.Confidence == 0 {
			res.Confidence = 1.0
		}
		if err := a.validateMemoryClaims(ctx, input, res); err != nil {
			return rollback(err)
		}
		// Only a success claim can be mechanically contradicted by terminal
		// evidence. Check it while the occurrence is reserved so no projection
		// can be published before the result is accepted.
		if err := a.validateTerminalSuccess(ctx, identity, res.Status); err != nil {
			return rollback(err)
		}
		if input.normalizedArtifactRefs != nil {
			res.Artifacts = input.normalizedArtifactRefs
			tx.addMaterialized(res.Artifacts)
		} else if err := a.materializeSubmittedArtifacts(ctx, res, tx); err != nil {
			return rollback(rejectResultSourceRead(fmt.Errorf("invalid artifact declaration: %w", err)))
		}
		if err := contract.validateFinalizableResult(res); err != nil {
			if _, collecting := ctx.Value(resultCollectionAdmissionKey{}).(*ResultCollectionCheckpoint); collecting && collectionProjectionFormatFailure(contract, res) {
				return rollback(rejectLocalResult(RepairFailureInvalidSchema, err))
			}
			return rollback(fmt.Errorf("submit_result contract violation: %w", err))
		}
		// A reported assumption check is one of the three sources allowed to
		// change an assumption's status (spec §18.1). Validate it while the
		// occurrence is reserved: a check naming an assumption the decision
		// never declared is a contract violation, not a warning.
		if _, err := a.coordinator.ApplyAssumptionChecks(ctx, a.todoID, res.AssumptionChecks, AssumptionSourceTaskResult); err != nil {
			if _, ok := errors.AsType[*assumptionCheckPersistenceError](err); ok {
				return rollback(rejectLocalResult(RepairFailureCommit, fmt.Errorf("commit assumption checks: %w", err)))
			}
			return rollback(fmt.Errorf("invalid assumption check: %w", err))
		}
		// A reported check can invalidate the governing decision.  That is a
		// lifecycle outcome, not merely a later tool denial: reject this same
		// submit_result while its occurrence is still reserved so no result cache,
		// TaskDone projection, or success event can be published.
		if denial := a.coordinator.checkpointDenial(a.todoID); denial != "" {
			return rollback(errors.New("submit_result rejected by checkpoint: " + denial))
		}
		if _, isCoordinatorSink := sink.(coordinatorTaskResultSink); isCoordinatorSink {
			// The coordinator sink is represented by this transaction; invoking it
			// again would attempt a second reservation while this gate is held.
		} else if sink != nil {
			if err := sink.Submit(ctx, a.todoID, *res); err != nil {
				return rollback(rejectLocalResult(RepairFailureCommit, fmt.Errorf("store submitted result: %w", err)))
			}
		}
		var commitErr error
		if checkpoint, ok := ctx.Value(resultCollectionAdmissionKey{}).(*ResultCollectionCheckpoint); ok {
			commitErr = tx.commitCollection(ctx, res, checkpoint)
		} else {
			commitErr = tx.commit(ctx, res)
		}
		if err := commitErr; err != nil {
			if _, classified := errors.AsType[*localResultAdmissionError](err); !classified {
				err = rejectLocalResultPersistence(err)
			}
			return rollback(fmt.Errorf("store submitted result: %w", err))
		}
		if stop, ok := ctx.Value(acceptedTerminalResultStopKey{}).(*acceptedTerminalResultStop); ok {
			stop.markAccepted(identity)
		}
		a.coordinator.emitMemoryUsageEvents(res)
		tx.finish()
	} else if sink != nil {
		res.TaskID = a.todoID
		res.Source = "submitted"
		if res.Confidence == 0 {
			res.Confidence = 1.0
		}
		if err := contract.validateFinalizableResult(res); err != nil {
			return errors.New("submit_result contract violation: " + err.Error())
		}
		if err := sink.Submit(ctx, a.todoID, *res); err != nil {
			return rejectLocalResult(RepairFailureCommit, errors.New("store submitted result: "+err.Error()))
		}
	}
	return nil
}

func (a *localResultAdmission) validateMemoryClaims(ctx context.Context, input SubmitResultInput, result *TaskResult) error {
	if cp, collecting := ctx.Value(resultCollectionAdmissionKey{}).(*ResultCollectionCheckpoint); collecting && input.collectionSourcesBound {
		return a.coordinator.validateCollectedMemoryInput(ctx, cp, input, result)
	}
	if err := a.coordinator.validateMemoryUseClaims(ctx, a.todoID, result); err != nil {
		return rejectResultSourceRead(err)
	}
	return nil
}

func (a *localResultAdmission) forbidsArtifacts() bool {
	if a == nil || a.coordinator == nil || a.coordinator.taskTracker == nil || a.coordinator.taskTracker.TodoList() == nil {
		return false
	}
	for _, item := range a.coordinator.taskTracker.TodoList().Items() {
		if item != nil && item.ID == a.todoID {
			return item.Execution.ForbidArtifacts
		}
	}
	return false
}

// materializeSubmittedArtifacts makes a worker's artifact claim durable before
// accepting its terminal result. Previously a model could declare a missing
// path, receive a successful submit_result response, and leave the evidence
// manifest to fail only at run finalization. Artifacts are workspace-contained
// evidence, so snapshot them here or reject the result while the worker can
// still correct its claim.
func (a *localResultAdmission) materializeSubmittedArtifacts(ctx context.Context, res *TaskResult, tx *taskResultOccurrenceTransaction) error {
	if res == nil || len(res.Artifacts) == 0 {
		return nil
	}
	if a.coordinator == nil || a.coordinator.session == nil || strings.TrimSpace(a.coordinator.session.Workspace) == "" {
		return fmt.Errorf("artifact submission requires a workspace")
	}
	workspace := a.coordinator.session.Workspace
	storeRoot := workspace
	if checkpoint, _ := ctx.Value(resultCollectionAdmissionKey{}).(*ResultCollectionCheckpoint); checkpoint != nil && checkpoint.FanoutLeaf {
		// The source directory belongs to this leaf, while accepted bytes
		// must outlive its disposable workspace in the parent's durable CAS.
		storeRoot = a.coordinator.artifactStoreRootPath()
	}
	store, err := NewFileArtifactStore(storeRoot, workspace)
	if err != nil {
		return err
	}
	identity, err := submitResultRuntimeIdentityFromContext(ctx, a.coordinator, a.todoID)
	if err != nil {
		return err
	}
	if res.TaskID != identity.TaskID || res.Attempt != identity.Attempt || res.Agent != identity.Agent {
		return fmt.Errorf("result provenance does not match runtime occurrence")
	}
	if err := validateSubmittedArtifactClaims(ctx, workspace, res.Artifacts); err != nil {
		return err
	}
	for i, artifact := range res.Artifacts {
		ref, err := putSubmittedArtifact(ctx, store, identity, artifact)
		if err != nil {
			if tx != nil {
				tx.addMaterialized(nil)
			}
			return fmt.Errorf("artifacts[%d] %q: %w", i, artifact.Path, err)
		}
		res.Artifacts[i] = ref
	}
	if tx != nil {
		tx.addMaterialized(res.Artifacts)
	}
	return nil
}

func rejectProjectedClaims(err error) error {
	if _, ok := errors.AsType[*localResultAdmissionError](err); ok {
		return err
	}
	return rejectLocalResult(RepairFailureInvalidSchema, err)
}

func validateResultSummaryAndStatus(res *TaskResult) error {
	if res.Summary == "" {
		return errors.New("summary is required")
	}
	switch strings.ToLower(strings.TrimSpace(res.Status)) {
	case TaskResultStatusSuccess, TaskResultStatusCompletedWithGaps, TaskResultStatusPartial, TaskResultStatusFailed, TaskResultStatusBlocked:
		res.Status = strings.ToLower(strings.TrimSpace(res.Status))
	default:
		return errors.New("status must be success, completed_with_gaps, partial, failed, or blocked")
	}
	return nil
}

func (c *Coordinator) stageSubmittedArtifacts(identity submitResultRuntimeIdentity, refs []ArtifactRef) {
	if c == nil || len(refs) == 0 {
		return
	}
	identity = c.completeOccurrenceLease(identity)
	if !validSubmitResultIdentity(identity) {
		return
	}
	controller := c.occurrenceController(identity.TaskID)
	controller.mu.Lock()
	if controller.opened && sameTaskResultOccurrence(controller.identity, identity) && !controller.reserved && controller.result == nil {
		controller.pending = append([]ArtifactRef(nil), refs...)
	}
	controller.mu.Unlock()
}

func validateSubmittedArtifactClaims(ctx context.Context, workspace string, artifacts []ArtifactRef) error {
	for i, artifact := range artifacts {
		if artifact.ID != "" {
			return fmt.Errorf("artifacts[%d] %q: artifact ids are runtime-owned", i, artifact.Path)
		}
		if artifact.SHA256 != "" || artifact.Bytes != 0 || artifact.ByteSize != 0 || artifact.RunID != "" || artifact.TaskID != "" || artifact.Attempt != 0 || artifact.Agent != "" || artifact.Provider != "" || artifact.ToolCallID != "" || !artifact.CreatedAt.IsZero() {
			return fmt.Errorf("artifacts[%d] %q: artifact provenance is runtime-owned", i, artifact.Path)
		}
		if strings.TrimSpace(artifact.Path) == "" {
			return fmt.Errorf("artifacts[%d] path is required", i)
		}
		path, err := resolveArtifactSourcePath(workspace, artifact.Path)
		if err != nil {
			return fmt.Errorf("artifacts[%d] %q: %w", i, artifact.Path, err)
		}
		if policy, ok := ctx.Value(tools.ArtifactPathPolicyKey).(tools.ArtifactPathPolicy); ok {
			if err := tools.EnforceArtifactPathPolicy(path, &policy); err != nil {
				return fmt.Errorf("artifacts[%d] %q: %w", i, artifact.Path, err)
			}
		}
		info, err := os.Stat(path)
		if err != nil {
			return fmt.Errorf("artifacts[%d] %q: %w", i, artifact.Path, err)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("artifacts[%d] %q must be a regular file", i, artifact.Path)
		}
	}
	return nil
}

func putSubmittedArtifact(ctx context.Context, store *FileArtifactStore, identity submitResultRuntimeIdentity, artifact ArtifactRef) (ArtifactRef, error) {
	_, err := resolveArtifactSourcePath(store.sourceDir, artifact.Path)
	if err != nil {
		return ArtifactRef{}, err
	}
	putResult, err := store.Put(ctx, PutArtifactRequest{
		Kind: artifact.Kind, Path: artifact.Path, Description: artifact.Description, MediaType: artifact.MediaType,
		SourcePath: artifact.Path, RunID: identity.RunID, TaskID: identity.TaskID, Attempt: identity.Attempt, Agent: identity.Agent,
	})
	return putResult.ArtifactRef, err
}
